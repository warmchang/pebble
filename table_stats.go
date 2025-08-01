// Copyright 2020 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/cockroachdb/crlib/crtime"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/invariants"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/internal/keyspan/keyspanimpl"
	"github.com/cockroachdb/pebble/internal/manifest"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/redact"
)

// In-memory statistics about tables help inform compaction picking, but may
// be expensive to calculate or load from disk. Every time a database is
// opened, these statistics must be reloaded or recalculated. To minimize
// impact on user activity and compactions, we load these statistics
// asynchronously in the background and store loaded statistics in each
// table's *TableMetadata.
//
// This file implements the asynchronous loading of statistics by maintaining
// a list of files that require statistics, alongside their LSM levels.
// Whenever new files are added to the LSM, the files are appended to
// d.mu.tableStats.pending. If a stats collection job is not currently
// running, one is started in a separate goroutine.
//
// The stats collection job grabs and clears the pending list, computes table
// statistics relative to the current readState and updates the tables' file
// metadata. New pending files may accumulate during a stats collection job,
// so a completing job triggers a new job if necessary. Only one job runs at a
// time.
//
// When an existing database is opened, all files lack in-memory statistics.
// These files' stats are loaded incrementally whenever the pending list is
// empty by scanning a current readState for files missing statistics. Once a
// job completes a scan without finding any remaining files without
// statistics, it flips a `loadedInitial` flag. From then on, the stats
// collection job only needs to load statistics for new files appended to the
// pending list.

func (d *DB) maybeCollectTableStatsLocked() {
	if d.shouldCollectTableStatsLocked() {
		go d.collectTableStats()
	}
}

// updateTableStatsLocked is called when new files are introduced, after the
// read state has been updated. It may trigger a new stat collection.
// DB.mu must be locked when calling.
func (d *DB) updateTableStatsLocked(newTables []manifest.NewTableEntry) {
	var needStats bool
	for _, nf := range newTables {
		if !nf.Meta.StatsValid() {
			needStats = true
			break
		}
	}
	if !needStats {
		return
	}

	d.mu.tableStats.pending = append(d.mu.tableStats.pending, newTables...)
	d.maybeCollectTableStatsLocked()
}

func (d *DB) shouldCollectTableStatsLocked() bool {
	return !d.mu.tableStats.loading &&
		d.closed.Load() == nil &&
		!d.opts.DisableTableStats &&
		(len(d.mu.tableStats.pending) > 0 || !d.mu.tableStats.loadedInitial)
}

// collectTableStats runs a table stats collection job, returning true if the
// invocation did the collection work, false otherwise (e.g. if another job was
// already running).
func (d *DB) collectTableStats() bool {
	const maxTableStatsPerScan = 50

	d.mu.Lock()
	if !d.shouldCollectTableStatsLocked() {
		d.mu.Unlock()
		return false
	}
	ctx := context.Background()

	pending := d.mu.tableStats.pending
	d.mu.tableStats.pending = nil
	d.mu.tableStats.loading = true
	jobID := d.newJobIDLocked()
	loadedInitial := d.mu.tableStats.loadedInitial
	// Drop DB.mu before performing IO.
	d.mu.Unlock()

	// Every run of collectTableStats either collects stats from the pending
	// list (if non-empty) or from scanning the version (loadedInitial is
	// false). This job only runs if at least one of those conditions holds.

	// Grab a read state to scan for tables.
	rs := d.loadReadState()
	var collected []collectedStats
	var hints []deleteCompactionHint
	if len(pending) > 0 {
		collected, hints = d.loadNewFileStats(ctx, rs, pending)
	} else {
		var moreRemain bool
		var buf [maxTableStatsPerScan]collectedStats
		collected, hints, moreRemain = d.scanReadStateTableStats(ctx, rs, buf[:0])
		loadedInitial = !moreRemain
	}
	rs.unref()

	// Update the TableMetadata with the loaded stats while holding d.mu.
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mu.tableStats.loading = false
	if loadedInitial && !d.mu.tableStats.loadedInitial {
		d.mu.tableStats.loadedInitial = loadedInitial
		d.opts.EventListener.TableStatsLoaded(TableStatsInfo{
			JobID: int(jobID),
		})
	}

	maybeCompact := false
	for _, c := range collected {
		c.TableMetadata.Stats = c.TableStats
		maybeCompact = maybeCompact || tableTombstoneCompensation(c.TableMetadata) > 0
		sanityCheckStats(c.TableMetadata, d.opts.Logger, "collected stats")
		c.TableMetadata.StatsMarkValid()
	}

	d.mu.tableStats.cond.Broadcast()
	d.maybeCollectTableStatsLocked()
	if len(hints) > 0 && !d.opts.private.disableDeleteOnlyCompactions {
		// Verify that all of the hint tombstones' files still exist in the
		// current version. Otherwise, the tombstone itself may have been
		// compacted into L6 and more recent keys may have had their sequence
		// numbers zeroed.
		//
		// Note that it's possible that the tombstone file is being compacted
		// presently. In that case, the file will be present in v. When the
		// compaction finishes compacting the tombstone file, it will detect
		// and clear the hint.
		//
		// See DB.maybeUpdateDeleteCompactionHints.
		v := d.mu.versions.currentVersion()
		keepHints := hints[:0]
		for _, h := range hints {
			if v.Contains(h.tombstoneLevel, h.tombstoneFile) {
				keepHints = append(keepHints, h)
			}
		}
		d.mu.compact.deletionHints = append(d.mu.compact.deletionHints, keepHints...)
	}
	if maybeCompact {
		d.maybeScheduleCompaction()
	}
	return true
}

type collectedStats struct {
	*manifest.TableMetadata
	manifest.TableStats
}

func (d *DB) loadNewFileStats(
	ctx context.Context, rs *readState, pending []manifest.NewTableEntry,
) ([]collectedStats, []deleteCompactionHint) {
	var hints []deleteCompactionHint
	collected := make([]collectedStats, 0, len(pending))
	for _, nf := range pending {
		// A file's stats might have been populated by an earlier call to
		// loadNewFileStats if the file was moved.
		// NB: We're not holding d.mu which protects f.Stats, but only
		// collectTableStats updates f.Stats for active files, and we
		// ensure only one goroutine runs it at a time through
		// d.mu.tableStats.loading.
		if nf.Meta.StatsValid() {
			continue
		}

		// The file isn't guaranteed to still be live in the readState's
		// version. It may have been deleted or moved. Skip it if it's not in
		// the expected level.
		if !rs.current.Contains(nf.Level, nf.Meta) {
			continue
		}

		stats, newHints, err := d.loadTableStats(ctx, rs.current, nf.Level, nf.Meta)
		if err != nil {
			d.opts.EventListener.BackgroundError(err)
			continue
		}
		// NB: We don't update the TableMetadata yet, because we aren't holding
		// DB.mu. We'll copy it to the TableMetadata after we're finished with
		// IO.
		collected = append(collected, collectedStats{
			TableMetadata: nf.Meta,
			TableStats:    stats,
		})
		hints = append(hints, newHints...)
	}
	return collected, hints
}

// scanReadStateTableStats is run by an active stat collection job when there
// are no pending new files, but there might be files that existed at Open for
// which we haven't loaded table stats.
func (d *DB) scanReadStateTableStats(
	ctx context.Context, rs *readState, fill []collectedStats,
) ([]collectedStats, []deleteCompactionHint, bool) {
	moreRemain := false
	var hints []deleteCompactionHint
	sizesChecked := make(map[base.DiskFileNum]struct{})
	for l, levelMetadata := range rs.current.Levels {
		for f := range levelMetadata.All() {
			// NB: We're not holding d.mu which protects f.Stats, but only the
			// active stats collection job updates f.Stats for active files,
			// and we ensure only one goroutine runs it at a time through
			// d.mu.tableStats.loading. This makes it safe to read validity
			// through f.Stats.ValidLocked despite not holding d.mu.
			if f.StatsValid() {
				continue
			}

			// Limit how much work we do per read state. The older the read
			// state is, the higher the likelihood files are no longer being
			// used in the current version. If we've exhausted our allowance,
			// return true for the last return value to signal there's more
			// work to do.
			if len(fill) == cap(fill) {
				moreRemain = true
				return fill, hints, moreRemain
			}

			// If the file is remote and not SharedForeign, we should check if its size
			// matches. This is because checkConsistency skips over remote files.
			//
			// SharedForeign and External files are skipped as their sizes are allowed
			// to have a mismatch; the size stored in the TableBacking is just the part
			// of the file that is referenced by this Pebble instance, not the size of
			// the whole object.
			objMeta, err := d.objProvider.Lookup(base.FileTypeTable, f.TableBacking.DiskFileNum)
			if err != nil {
				// Set `moreRemain` so we'll try again.
				moreRemain = true
				d.opts.EventListener.BackgroundError(err)
				continue
			}

			shouldCheckSize := objMeta.IsRemote() &&
				!d.objProvider.IsSharedForeign(objMeta) &&
				!objMeta.IsExternal()
			if _, ok := sizesChecked[f.TableBacking.DiskFileNum]; !ok && shouldCheckSize {
				size, err := d.objProvider.Size(objMeta)
				fileSize := f.TableBacking.Size
				if err != nil {
					moreRemain = true
					d.opts.EventListener.BackgroundError(err)
					continue
				}
				if size != int64(fileSize) {
					err := errors.Errorf(
						"during consistency check in loadTableStats: L%d: %s: object size mismatch (%s): %d (provider) != %d (MANIFEST)",
						errors.Safe(l), f.TableNum, d.objProvider.Path(objMeta),
						errors.Safe(size), errors.Safe(fileSize))
					d.opts.EventListener.BackgroundError(err)
					d.opts.Logger.Fatalf("%s", err)
				}

				sizesChecked[f.TableBacking.DiskFileNum] = struct{}{}
			}

			stats, newHints, err := d.loadTableStats(ctx, rs.current, l, f)
			if err != nil {
				// Set `moreRemain` so we'll try again.
				moreRemain = true
				d.opts.EventListener.BackgroundError(err)
				continue
			}
			fill = append(fill, collectedStats{
				TableMetadata: f,
				TableStats:    stats,
			})
			hints = append(hints, newHints...)
		}
	}
	return fill, hints, moreRemain
}

func (d *DB) loadTableStats(
	ctx context.Context, v *manifest.Version, level int, meta *manifest.TableMetadata,
) (manifest.TableStats, []deleteCompactionHint, error) {
	var stats manifest.TableStats
	var compactionHints []deleteCompactionHint

	err := d.fileCache.withReader(
		ctx, block.NoReadEnv, meta, func(r *sstable.Reader, env sstable.ReadEnv) (err error) {
			loadedProps, err := r.ReadPropertiesBlock(ctx, nil /* buffer pool */)
			if err != nil {
				return err
			}
			props := loadedProps.CommonProperties
			if meta.Virtual {
				props = loadedProps.GetScaledProperties(meta.TableBacking.Size, meta.Size)
			}
			stats.NumEntries = props.NumEntries
			stats.NumDeletions = props.NumDeletions
			stats.NumRangeKeySets = props.NumRangeKeySets
			stats.ValueBlocksSize = props.ValueBlocksSize
			stats.RawKeySize = props.RawKeySize
			stats.RawValueSize = props.RawValueSize
			stats.CompressionType = block.CompressionProfileByName(props.CompressionName)

			if loadedProps.CompressionStats != "" {
				var err error
				stats.CompressionStats, err = block.ParseCompressionStats(loadedProps.CompressionStats)
				if invariants.Enabled && err != nil {
					panic(errors.AssertionFailedf("pebble: error parsing compression stats %q for table %s: %v", loadedProps.CompressionStats, meta.TableNum, err))
				}
				if meta.Virtual {
					meta.Stats.CompressionStats.Scale(meta.Size, meta.TableBacking.Size)
				}
			}

			if props.NumDataBlocks > 0 {
				stats.TombstoneDenseBlocksRatio = float64(props.NumTombstoneDenseBlocks) / float64(props.NumDataBlocks)
			}

			if props.NumPointDeletions() > 0 {
				if err = d.loadTablePointKeyStats(ctx, &props, v, level, meta, &stats); err != nil {
					return
				}
			}
			if r.Attributes.Intersects(sstable.AttributeRangeDels | sstable.AttributeRangeKeyDels) {
				compactionHints, err = d.loadTableRangeDelStats(ctx, r, v, level, meta, &stats, env)
				if err != nil {
					return
				}
			}
			return
		})
	if err != nil {
		return stats, nil, err
	}
	return stats, compactionHints, nil
}

// loadTablePointKeyStats calculates the point key statistics for the given
// table. The provided manifest.TableStats are updated.
func (d *DB) loadTablePointKeyStats(
	ctx context.Context,
	props *sstable.CommonProperties,
	v *manifest.Version,
	level int,
	meta *manifest.TableMetadata,
	stats *manifest.TableStats,
) error {
	// TODO(jackson): If the file has a wide keyspace, the average
	// value size beneath the entire file might not be representative
	// of the size of the keys beneath the point tombstones.
	// We could write the ranges of 'clusters' of point tombstones to
	// a sstable property and call averageValueSizeBeneath for each of
	// these narrower ranges to improve the estimate.
	avgValLogicalSize, compressionRatio, err := d.estimateSizesBeneath(ctx, v, level, meta, props)
	if err != nil {
		return err
	}
	stats.PointDeletionsBytesEstimate =
		pointDeletionsBytesEstimate(props, avgValLogicalSize, compressionRatio)
	return nil
}

// loadTableRangeDelStats calculates the range deletion and range key deletion
// statistics for the given table.
func (d *DB) loadTableRangeDelStats(
	ctx context.Context,
	r *sstable.Reader,
	v *manifest.Version,
	level int,
	meta *manifest.TableMetadata,
	stats *manifest.TableStats,
	env sstable.ReadEnv,
) ([]deleteCompactionHint, error) {
	iter, err := newCombinedDeletionKeyspanIter(ctx, d.opts.Comparer, r, meta, env)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var compactionHints []deleteCompactionHint
	// We iterate over the defragmented range tombstones and range key deletions,
	// which ensures we don't double count ranges deleted at different sequence
	// numbers. Also, merging abutting tombstones reduces the number of calls to
	// estimateReclaimedSizeBeneath which is costly, and improves the accuracy of
	// our overall estimate.
	s, err := iter.First()
	for ; s != nil; s, err = iter.Next() {
		start, end := s.Start, s.End
		// We only need to consider deletion size estimates for tables that contain
		// RANGEDELs.
		var maxRangeDeleteSeqNum base.SeqNum
		for _, k := range s.Keys {
			if k.Kind() == base.InternalKeyKindRangeDelete && maxRangeDeleteSeqNum < k.SeqNum() {
				maxRangeDeleteSeqNum = k.SeqNum()
				break
			}
		}

		// If the file is in the last level of the LSM, there is no data beneath
		// it. The fact that there is still a range tombstone in a bottommost file
		// indicates two possibilites:
		//   1. an open snapshot kept the tombstone around, and the data the
		//      tombstone deletes is contained within the file itself.
		//   2. the file was ingested.
		// In the first case, we'd like to estimate disk usage within the file
		// itself since compacting the file will drop that covered data. In the
		// second case, we expect that compacting the file will NOT drop any
		// data and rewriting the file is a waste of write bandwidth. We can
		// distinguish these cases by looking at the table metadata's sequence
		// numbers. A file's range deletions can only delete data within the
		// file at lower sequence numbers. All keys in an ingested sstable adopt
		// the same sequence number, preventing tombstones from deleting keys
		// within the same file. We check here if the largest RANGEDEL sequence
		// number is greater than the file's smallest sequence number. If it is,
		// the RANGEDEL could conceivably (although inconclusively) delete data
		// within the same file.
		//
		// Note that this heuristic is imperfect. If a table containing a range
		// deletion is ingested into L5 and subsequently compacted into L6 but
		// an open snapshot prevents elision of covered keys in L6, the
		// resulting RangeDeletionsBytesEstimate will incorrectly include all
		// covered keys.
		//
		// TODO(jackson): We could prevent the above error in the heuristic by
		// computing the file's RangeDeletionsBytesEstimate during the
		// compaction itself. It's unclear how common this is.
		//
		// NOTE: If the span `s` wholly contains a table containing range keys,
		// the returned size estimate will be slightly inflated by the range key
		// block. However, in practice, range keys are expected to be rare, and
		// the size of the range key block relative to the overall size of the
		// table is expected to be small.
		if level == numLevels-1 && meta.SmallestSeqNum < maxRangeDeleteSeqNum {
			size, err := r.EstimateDiskUsage(start, end, env, meta.IterTransforms())
			if err != nil {
				return nil, err
			}
			stats.RangeDeletionsBytesEstimate += size

			// As the file is in the bottommost level, there is no need to collect a
			// deletion hint.
			continue
		}

		// While the size estimates for point keys should only be updated if this
		// span contains a range del, the sequence numbers are required for the
		// hint. Unconditionally descend, but conditionally update the estimates.
		hintType := compactionHintFromKeys(s.Keys)
		estimate, hintSeqNum, err := d.estimateReclaimedSizeBeneath(ctx, v, level, start, end, hintType)
		if err != nil {
			return nil, err
		}
		stats.RangeDeletionsBytesEstimate += estimate

		// hintSeqNum is the smallest sequence number contained in any
		// file overlapping with the hint and in a level below it.
		if hintSeqNum == math.MaxUint64 {
			continue
		}
		compactionHints = append(compactionHints, deleteCompactionHint{
			hintType:                hintType,
			start:                   slices.Clone(start),
			end:                     slices.Clone(end),
			tombstoneFile:           meta,
			tombstoneLevel:          level,
			tombstoneLargestSeqNum:  s.LargestSeqNum(),
			tombstoneSmallestSeqNum: s.SmallestSeqNum(),
			fileSmallestSeqNum:      hintSeqNum,
		})
	}
	if err != nil {
		return nil, err
	}
	return compactionHints, nil
}

func (d *DB) estimateSizesBeneath(
	ctx context.Context,
	v *manifest.Version,
	level int,
	meta *manifest.TableMetadata,
	fileProps *sstable.CommonProperties,
) (avgValueLogicalSize, compressionRatio float64, err error) {
	// Find all files in lower levels that overlap with meta,
	// summing their value sizes and entry counts.

	// Include the file itself. This is important because in some instances, the
	// computed compression ratio is applied to the tombstones contained within
	// `meta` itself. If there are no files beneath `meta` in the LSM, we would
	// calculate a compression ratio of 0 which is not accurate for the file's
	// own tombstones.
	var (
		// TODO(sumeer): The entryCount includes the tombstones, which can be small,
		// resulting in a lower than expected avgValueLogicalSize. For an example of
		// this effect see the estimate in testdata/compaction_picker_scores (search
		// for "point-deletions-bytes-estimate: 163850").
		fileSum    = meta.Size + meta.EstimatedReferenceSize()
		entryCount = fileProps.NumEntries
		keySum     = fileProps.RawKeySize
		valSum     = fileProps.RawValueSize
	)

	for l := level + 1; l < numLevels; l++ {
		for tableBeneath := range v.Overlaps(l, meta.UserKeyBounds()).All() {
			fileSum += tableBeneath.Size + tableBeneath.EstimatedReferenceSize()
			if tableBeneath.StatsValid() {
				entryCount += tableBeneath.Stats.NumEntries
				keySum += tableBeneath.Stats.RawKeySize
				valSum += tableBeneath.Stats.RawValueSize
				continue
			}
			// If stats aren't available, we need to read the properties block.
			err := d.fileCache.withReader(ctx, block.NoReadEnv, tableBeneath, func(v *sstable.Reader, _ sstable.ReadEnv) (err error) {
				loadedProps, err := v.ReadPropertiesBlock(ctx, nil /* buffer pool */)
				if err != nil {
					return err
				}
				props := loadedProps.CommonProperties
				if tableBeneath.Virtual {
					props = loadedProps.GetScaledProperties(tableBeneath.TableBacking.Size, tableBeneath.Size)
				}

				entryCount += props.NumEntries
				keySum += props.RawKeySize
				valSum += props.RawValueSize
				return nil
			})
			if err != nil {
				return 0, 0, err
			}
		}
	}
	if entryCount == 0 {
		return 0, 0, nil
	}
	// RawKeySize and RawValueSize are uncompressed totals. We'll need to scale
	// the value sum according to the data size to account for compression,
	// index blocks and metadata overhead. Eg:
	//
	//    Compression rate        ×  Average uncompressed value size
	//
	//                            ↓
	//
	//         FileSize              RawValueSize
	//   -----------------------  ×  ------------
	//   RawKeySize+RawValueSize     NumEntries
	//
	// We return the average logical value size plus the compression ratio,
	// leaving the scaling to the caller. This allows the caller to perform
	// additional compression ratio scaling if necessary.
	uncompressedSum := float64(keySum + valSum)
	compressionRatio = float64(fileSum) / uncompressedSum
	if compressionRatio > 1 {
		// We can get huge compression ratios due to the fixed overhead of files
		// containing a tiny amount of data. By setting this to 1, we are ignoring
		// that overhead, but we accept that tradeoff since the total bytes in
		// such overhead is not large.
		compressionRatio = 1
	}
	avgValueLogicalSize = (float64(valSum) / float64(entryCount))
	return avgValueLogicalSize, compressionRatio, nil
}

func (d *DB) estimateReclaimedSizeBeneath(
	ctx context.Context,
	v *manifest.Version,
	level int,
	start, end []byte,
	hintType deleteCompactionHintType,
) (estimate uint64, hintSeqNum base.SeqNum, err error) {
	// Find all files in lower levels that overlap with the deleted range
	// [start, end).
	//
	// An overlapping file might be completely contained by the range
	// tombstone, in which case we can count the entire file size in
	// our estimate without doing any additional I/O.
	//
	// Otherwise, estimating the range for the file requires
	// additional I/O to read the file's index blocks.
	hintSeqNum = math.MaxUint64
	// TODO(jbowens): When there are multiple sub-levels in L0 and the RANGEDEL
	// is from a higher sub-level, we incorrectly skip the files in the lower
	// sub-levels when estimating this overlap.
	for l := level + 1; l < numLevels; l++ {
		for file := range v.Overlaps(l, base.UserKeyBoundsEndExclusive(start, end)).All() {
			// Determine whether we need to update size estimates and hint seqnums
			// based on the type of hint and the type of keys in this file.
			var updateEstimates, updateHints bool
			switch hintType {
			case deleteCompactionHintTypePointKeyOnly:
				// The range deletion byte estimates should only be updated if this
				// table contains point keys. This ends up being an overestimate in
				// the case that table also has range keys, but such keys are expected
				// to contribute a negligible amount of the table's overall size,
				// relative to point keys.
				if file.HasPointKeys {
					updateEstimates = true
				}
				// As the initiating span contained only range dels, hints can only be
				// updated if this table does _not_ contain range keys.
				if !file.HasRangeKeys {
					updateHints = true
				}
			case deleteCompactionHintTypeRangeKeyOnly:
				// The initiating span contained only range key dels. The estimates
				// apply only to point keys, and are therefore not updated.
				updateEstimates = false
				// As the initiating span contained only range key dels, hints can
				// only be updated if this table does _not_ contain point keys.
				if !file.HasPointKeys {
					updateHints = true
				}
			case deleteCompactionHintTypePointAndRangeKey:
				// Always update the estimates and hints, as this hint type can drop a
				// file, irrespective of the mixture of keys. Similar to above, the
				// range del bytes estimates is an overestimate.
				updateEstimates, updateHints = true, true
			default:
				panic(fmt.Sprintf("pebble: unknown hint type %s", hintType))
			}
			startCmp := d.cmp(start, file.Smallest().UserKey)
			endCmp := d.cmp(file.Largest().UserKey, end)
			if startCmp <= 0 && (endCmp < 0 || endCmp == 0 && file.Largest().IsExclusiveSentinel()) {
				// The range fully contains the file, so skip looking it up in table
				// cache/looking at its indexes and add the full file size.
				if updateEstimates {
					estimate += file.Size
				}
				if updateHints && hintSeqNum > file.SmallestSeqNum {
					hintSeqNum = file.SmallestSeqNum
				}
			} else if d.cmp(file.Smallest().UserKey, end) <= 0 && d.cmp(start, file.Largest().UserKey) <= 0 {
				// Partial overlap.
				if hintType == deleteCompactionHintTypeRangeKeyOnly {
					// If the hint that generated this overlap contains only range keys,
					// there is no need to calculate disk usage, as the reclaimable space
					// is expected to be minimal relative to point keys.
					continue
				}
				var size uint64
				err := d.fileCache.withReader(ctx, block.NoReadEnv, file,
					func(r *sstable.Reader, env sstable.ReadEnv) (err error) {
						size, err = r.EstimateDiskUsage(start, end, env, file.IterTransforms())
						return err
					})
				if err != nil {
					return 0, hintSeqNum, err
				}
				estimate += size
				if updateHints && hintSeqNum > file.SmallestSeqNum && d.FormatMajorVersion() >= FormatVirtualSSTables {
					// If the format major version is past Virtual SSTables, deletion only
					// hints can also apply to partial overlaps with sstables.
					hintSeqNum = file.SmallestSeqNum
				}
			}
		}
	}
	return estimate, hintSeqNum, nil
}

var lastSanityCheckStatsLog crtime.AtomicMono

func sanityCheckStats(meta *manifest.TableMetadata, logger Logger, info string) {
	// Values for PointDeletionsBytesEstimate and RangeDeletionsBytesEstimate that
	// exceed this value are likely indicative of a bug (eg, underflow).
	const maxDeletionBytesEstimate = 1 << 50 // 1 PiB

	if meta.Stats.PointDeletionsBytesEstimate > maxDeletionBytesEstimate ||
		meta.Stats.RangeDeletionsBytesEstimate > maxDeletionBytesEstimate {
		if invariants.Enabled {
			panic(fmt.Sprintf("%s: table %s has extreme deletion bytes estimates: point=%d range=%d",
				info, meta.TableNum,
				redact.Safe(meta.Stats.PointDeletionsBytesEstimate),
				redact.Safe(meta.Stats.RangeDeletionsBytesEstimate),
			))
		}
		if v := lastSanityCheckStatsLog.Load(); v == 0 || v.Elapsed() > 30*time.Second {
			logger.Errorf("%s: table %s has extreme deletion bytes estimates: point=%d range=%d",
				info, meta.TableNum,
				redact.Safe(meta.Stats.PointDeletionsBytesEstimate),
				redact.Safe(meta.Stats.RangeDeletionsBytesEstimate),
			)
			lastSanityCheckStatsLog.Store(crtime.NowMono())
		}
	}
}

func maybeSetStatsFromProperties(
	meta *manifest.TableMetadata, props *sstable.Properties, logger Logger,
) bool {
	// If a table contains range deletions or range key deletions, we defer the
	// stats collection. There are two main reasons for this:
	//
	//  1. Estimating the potential for reclaimed space due to a range deletion
	//     tombstone requires scanning the LSM - a potentially expensive operation
	//     that should be deferred.
	//  2. Range deletions and / or range key deletions present an opportunity to
	//     compute "deletion hints", which also requires a scan of the LSM to
	//     compute tables that would be eligible for deletion.
	//
	// These two tasks are deferred to the table stats collector goroutine.
	if props.NumRangeDeletions != 0 || props.NumRangeKeyDels != 0 {
		return false
	}

	// If a table is more than 10% point deletions without user-provided size
	// estimates, don't calculate the PointDeletionsBytesEstimate statistic
	// using our limited knowledge. The table stats collector can populate the
	// stats and calculate an average of value size of all the tables beneath
	// the table in the LSM, which will be more accurate.
	if unsizedDels := (props.NumDeletions - props.NumSizedDeletions); unsizedDels > props.NumEntries/10 {
		return false
	}

	var pointEstimate uint64
	if props.NumEntries > 0 {
		// Use the file's own average key and value sizes as an estimate. This
		// doesn't require any additional IO and since the number of point
		// deletions in the file is low, the error introduced by this crude
		// estimate is expected to be small.
		avgValSize, compressionRatio := estimatePhysicalSizes(meta, &props.CommonProperties)
		pointEstimate = pointDeletionsBytesEstimate(&props.CommonProperties, avgValSize, compressionRatio)
	}

	meta.Stats.NumEntries = props.NumEntries
	meta.Stats.NumDeletions = props.NumDeletions
	meta.Stats.NumRangeKeySets = props.NumRangeKeySets
	meta.Stats.PointDeletionsBytesEstimate = pointEstimate
	meta.Stats.RangeDeletionsBytesEstimate = 0
	meta.Stats.ValueBlocksSize = props.ValueBlocksSize
	meta.Stats.RawKeySize = props.RawKeySize
	meta.Stats.RawValueSize = props.RawValueSize
	meta.Stats.CompressionType = block.CompressionProfileByName(props.CompressionName)
	if props.CompressionStats != "" {
		var err error
		meta.Stats.CompressionStats, err = block.ParseCompressionStats(props.CompressionStats)
		if invariants.Enabled && err != nil {
			panic(errors.AssertionFailedf("pebble: error parsing compression stats %q for table %s: %v", props.CompressionStats, meta.TableNum, err))
		}
		if meta.Virtual {
			meta.Stats.CompressionStats.Scale(meta.Size, meta.TableBacking.Size)
		}
	}
	meta.StatsMarkValid()
	sanityCheckStats(meta, logger, "stats from properties")
	return true
}

func pointDeletionsBytesEstimate(
	props *sstable.CommonProperties, avgValLogicalSize, compressionRatio float64,
) (estimate uint64) {
	if props.NumEntries == 0 {
		return 0
	}
	numPointDels := props.NumPointDeletions()
	if numPointDels == 0 {
		return 0
	}
	// Estimate the potential space to reclaim using the table's own properties.
	// There may or may not be keys covered by any individual point tombstone.
	// If not, compacting the point tombstone into L6 will at least allow us to
	// drop the point deletion key and will reclaim the tombstone's key bytes.
	// If there are covered key(s), we also get to drop key and value bytes for
	// each covered key.
	//
	// Some point tombstones (DELSIZEDs) carry a user-provided estimate of the
	// uncompressed size of entries that will be elided by fully compacting the
	// tombstone. For these tombstones, there's no guesswork—we use the
	// RawPointTombstoneValueSizeHint property which is the sum of all these
	// tombstones' encoded values.
	//
	// For un-sized point tombstones (DELs), we estimate assuming that each
	// point tombstone on average covers 1 key and using average value sizes.
	// This is almost certainly an overestimate, but that's probably okay
	// because point tombstones can slow range iterations even when they don't
	// cover a key.
	//
	// TODO(jackson): This logic doesn't directly incorporate fixed per-key
	// overhead (8-byte trailer, plus at least 1 byte encoding the length of the
	// key and 1 byte encoding the length of the value). This overhead is
	// indirectly incorporated through the compression ratios, but that results
	// in the overhead being smeared per key-byte and value-byte, rather than
	// per-entry. This per-key fixed overhead can be nontrivial, especially for
	// dense swaths of point tombstones. Give some thought as to whether we
	// should directly include fixed per-key overhead in the calculations.

	// Below, we calculate the tombstone contributions and the shadowed keys'
	// contributions separately.
	var tombstonesLogicalSize float64
	var shadowedLogicalSize float64

	// 1. Calculate the contribution of the tombstone keys themselves.
	if props.RawPointTombstoneKeySize > 0 {
		tombstonesLogicalSize += float64(props.RawPointTombstoneKeySize)
	} else {
		// This sstable predates the existence of the RawPointTombstoneKeySize
		// property. We can use the average key size within the file itself and
		// the count of point deletions to estimate the size.
		tombstonesLogicalSize += float64(numPointDels * props.RawKeySize / props.NumEntries)
	}

	// 2. Calculate the contribution of the keys shadowed by tombstones.
	//
	// 2a. First account for keys shadowed by DELSIZED tombstones. THE DELSIZED
	// tombstones encode the size of both the key and value of the shadowed KV
	// entries. These sizes are aggregated into a sstable property.
	shadowedLogicalSize += float64(props.RawPointTombstoneValueSize)

	// 2b. Calculate the contribution of the KV entries shadowed by ordinary DEL
	// keys.
	numUnsizedDels := invariants.SafeSub(numPointDels, props.NumSizedDeletions)
	{
		// The shadowed keys have the same exact user keys as the tombstones
		// themselves, so we can use the `tombstonesLogicalSize` we computed
		// earlier as an estimate. There's a complication that
		// `tombstonesLogicalSize` may include DELSIZED keys we already
		// accounted for.
		shadowedLogicalSize += float64(tombstonesLogicalSize) / float64(numPointDels) * float64(numUnsizedDels)

		// Calculate the contribution of the deleted values. The caller has
		// already computed an average logical size (possibly computed across
		// many sstables).
		shadowedLogicalSize += float64(numUnsizedDels) * avgValLogicalSize
	}

	// Scale both tombstone and shadowed totals by logical:physical ratios to
	// account for compression, metadata overhead, etc.
	//
	//      Physical             FileSize
	//     -----------  = -----------------------
	//      Logical       RawKeySize+RawValueSize
	//
	return uint64((tombstonesLogicalSize + shadowedLogicalSize) * compressionRatio)
}

func estimatePhysicalSizes(
	tableMeta *manifest.TableMetadata, props *sstable.CommonProperties,
) (avgValLogicalSize, compressionRatio float64) {
	// RawKeySize and RawValueSize are uncompressed totals. Scale according to
	// the data size to account for compression, index blocks and metadata
	// overhead. Eg:
	//
	//    Compression rate        ×  Average uncompressed value size
	//
	//                            ↓
	//
	//         FileSize              RawValSize
	//   -----------------------  ×  ----------
	//   RawKeySize+RawValueSize     NumEntries
	//
	physicalSize := tableMeta.Size + tableMeta.EstimatedReferenceSize()
	uncompressedSum := props.RawKeySize + props.RawValueSize
	compressionRatio = float64(physicalSize) / float64(uncompressedSum)
	if compressionRatio > 1 {
		// We can get huge compression ratios due to the fixed overhead of files
		// containing a tiny amount of data. By setting this to 1, we are ignoring
		// that overhead, but we accept that tradeoff since the total bytes in
		// such overhead is not large.
		compressionRatio = 1
	}
	avgValLogicalSize = (float64(props.RawValueSize) / float64(props.NumEntries))
	return avgValLogicalSize, compressionRatio
}

// newCombinedDeletionKeyspanIter returns a keyspan.FragmentIterator that
// returns "ranged deletion" spans for a single table, providing a combined view
// of both range deletion and range key deletion spans. The
// tableRangedDeletionIter is intended for use in the specific case of computing
// the statistics and deleteCompactionHints for a single table.
//
// As an example, consider the following set of spans from the range deletion
// and range key blocks of a table:
//
//		      |---------|     |---------|         |-------| RANGEKEYDELs
//		|-----------|-------------|           |-----|       RANGEDELs
//	  __________________________________________________________
//		a b c d e f g h i j k l m n o p q r s t u v w x y z
//
// The tableRangedDeletionIter produces the following set of output spans, where
// '1' indicates a span containing only range deletions, '2' is a span
// containing only range key deletions, and '3' is a span containing a mixture
// of both range deletions and range key deletions.
//
//		   1       3       1    3    2          1  3   2
//		|-----|---------|-----|---|-----|     |---|-|-----|
//	  __________________________________________________________
//		a b c d e f g h i j k l m n o p q r s t u v w x y z
//
// Algorithm.
//
// The iterator first defragments the range deletion and range key blocks
// separately. During this defragmentation, the range key block is also filtered
// so that keys other than range key deletes are ignored. The range delete and
// range key delete keyspaces are then merged.
//
// Note that the only fragmentation introduced by merging is from where a range
// del span overlaps with a range key del span. Within the bounds of any overlap
// there is guaranteed to be no further fragmentation, as the constituent spans
// have already been defragmented. To the left and right of any overlap, the
// same reasoning applies. For example,
//
//		         |--------|         |-------| RANGEKEYDEL
//		|---------------------------|         RANGEDEL
//		|----1---|----3---|----1----|---2---| Merged, fragmented spans.
//	  __________________________________________________________
//		a b c d e f g h i j k l m n o p q r s t u v w x y z
//
// Any fragmented abutting spans produced by the merging iter will be of
// differing types (i.e. a transition from a span with homogenous key kinds to a
// heterogeneous span, or a transition from a span with exclusively range dels
// to a span with exclusively range key dels). Therefore, further
// defragmentation is not required.
//
// Each span returned by the tableRangeDeletionIter will have at most four keys,
// corresponding to the largest and smallest sequence numbers encountered across
// the range deletes and range keys deletes that comprised the merged spans.
func newCombinedDeletionKeyspanIter(
	ctx context.Context,
	comparer *base.Comparer,
	r *sstable.Reader,
	m *manifest.TableMetadata,
	env sstable.ReadEnv,
) (keyspan.FragmentIterator, error) {
	// The range del iter and range key iter are each wrapped in their own
	// defragmenting iter. For each iter, abutting spans can always be merged.
	var equal = keyspan.DefragmentMethodFunc(func(_ base.CompareRangeSuffixes, a, b *keyspan.Span) bool { return true })
	// Reduce keys by maintaining a slice of at most length two, corresponding to
	// the largest and smallest keys in the defragmented span. This maintains the
	// contract that the emitted slice is sorted by (SeqNum, Kind) descending.
	reducer := func(current, incoming []keyspan.Key) []keyspan.Key {
		if len(current) == 0 && len(incoming) == 0 {
			// While this should never occur in practice, a defensive return is used
			// here to preserve correctness.
			return current
		}
		var largest, smallest keyspan.Key
		var set bool
		for _, keys := range [2][]keyspan.Key{current, incoming} {
			if len(keys) == 0 {
				continue
			}
			first, last := keys[0], keys[len(keys)-1]
			if !set {
				largest, smallest = first, last
				set = true
				continue
			}
			if first.Trailer > largest.Trailer {
				largest = first
			}
			if last.Trailer < smallest.Trailer {
				smallest = last
			}
		}
		if largest.Equal(comparer.CompareRangeSuffixes, smallest) {
			current = append(current[:0], largest)
		} else {
			current = append(current[:0], largest, smallest)
		}
		return current
	}

	// The separate iters for the range dels and range keys are wrapped in a
	// merging iter to join the keyspaces into a single keyspace. The separate
	// iters are only added if the particular key kind is present.
	mIter := &keyspanimpl.MergingIter{}
	var transform = keyspan.TransformerFunc(func(_ base.CompareRangeSuffixes, in keyspan.Span, out *keyspan.Span) error {
		if in.KeysOrder != keyspan.ByTrailerDesc {
			return base.AssertionFailedf("combined deletion iter encountered keys in non-trailer descending order")
		}
		out.Start, out.End = in.Start, in.End
		out.Keys = append(out.Keys[:0], in.Keys...)
		out.KeysOrder = keyspan.ByTrailerDesc
		// NB: The order of by-trailer descending may have been violated,
		// because we've layered rangekey and rangedel iterators from the same
		// sstable into the same keyspanimpl.MergingIter. The MergingIter will
		// return the keys in the order that the child iterators were provided.
		// Sort the keys to ensure they're sorted by trailer descending.
		keyspan.SortKeysByTrailer(out.Keys)
		return nil
	})
	mIter.Init(comparer, transform, new(keyspanimpl.MergingBuffers))
	iter, err := r.NewRawRangeDelIter(ctx, m.FragmentIterTransforms(), env)
	if err != nil {
		return nil, err
	}
	if iter != nil {
		// Assert expected bounds. In previous versions of Pebble, range
		// deletions persisted to sstables could exceed the bounds of the
		// containing files due to "split user keys." This required readers to
		// constrain the tombstones' bounds to the containing file at read time.
		// See docs/range_deletions.md for an extended discussion of the design
		// and invariants at that time.
		//
		// We've since compacted away all 'split user-keys' and in the process
		// eliminated all "untruncated range tombstones" for physical sstables.
		// We no longer need to perform truncation at read time for these
		// sstables.
		//
		// At the same time, we've also introduced the concept of "virtual
		// SSTables" where the table metadata's effective bounds can again be
		// reduced to be narrower than the contained tombstones. These virtual
		// SSTables handle truncation differently, performing it using
		// keyspan.Truncate when the sstable's range deletion iterator is
		// opened.
		//
		// Together, these mean that we should never see untruncated range
		// tombstones any more—and the merging iterator no longer accounts for
		// their existence. Since there's abundant subtlety that we're relying
		// on, we choose to be conservative and assert that these invariants
		// hold. We could (and previously did) choose to only validate these
		// bounds in invariants builds, but the most likely avenue for these
		// tombstones' existence is through a bug in a migration and old data
		// sitting around in an old store from long ago.
		//
		// The table stats collector will read all files' range deletions
		// asynchronously after Open, and provides a perfect opportunity to
		// validate our invariants without harming user latency. We also
		// previously performed truncation here which similarly required key
		// comparisons, so replacing those key comparisons with assertions
		// should be roughly similar in performance.
		//
		// TODO(jackson): Only use AssertBounds in invariants builds in the
		// following release.
		iter = keyspan.AssertBounds(
			iter, m.PointKeyBounds.Smallest(), m.PointKeyBounds.LargestUserKey(), comparer.Compare,
		)
		dIter := &keyspan.DefragmentingIter{}
		dIter.Init(comparer, iter, equal, reducer, new(keyspan.DefragmentingBuffers))
		iter = dIter
		mIter.AddLevel(iter)
	}

	iter, err = r.NewRawRangeKeyIter(ctx, m.FragmentIterTransforms(), env)
	if err != nil {
		return nil, err
	}
	if iter != nil {
		// Assert expected bounds in tests.
		if invariants.Sometimes(50) {
			if m.HasRangeKeys {
				iter = keyspan.AssertBounds(
					iter, m.RangeKeyBounds.Smallest(), m.RangeKeyBounds.LargestUserKey(), comparer.Compare,
				)
			}
		}
		// Wrap the range key iterator in a filter that elides keys other than range
		// key deletions.
		iter = keyspan.Filter(iter, func(in *keyspan.Span, buf []keyspan.Key) []keyspan.Key {
			keys := buf[:0]
			for _, k := range in.Keys {
				if k.Kind() != base.InternalKeyKindRangeKeyDelete {
					continue
				}
				keys = append(keys, k)
			}
			return keys
		}, comparer.Compare)
		dIter := &keyspan.DefragmentingIter{}
		dIter.Init(comparer, iter, equal, reducer, new(keyspan.DefragmentingBuffers))
		iter = dIter
		mIter.AddLevel(iter)
	}

	return mIter, nil
}

// rangeKeySetsAnnotator is a manifest.Annotator that annotates B-Tree nodes
// with the sum of the files' counts of range key fragments. The count of range
// key sets may change once a table's stats are loaded asynchronously, so its
// values are marked as cacheable only if a file's stats have been loaded.
var rangeKeySetsAnnotator = manifest.SumAnnotator(func(f *manifest.TableMetadata) (uint64, bool) {
	return f.Stats.NumRangeKeySets, f.StatsValid()
})

// tombstonesAnnotator is a manifest.Annotator that annotates B-Tree nodes
// with the sum of the files' counts of tombstones (DEL, SINGLEDEL and RANGEDEL
// keys). The count of tombstones may change once a table's stats are loaded
// asynchronously, so its values are marked as cacheable only if a file's stats
// have been loaded.
var tombstonesAnnotator = manifest.SumAnnotator(func(f *manifest.TableMetadata) (uint64, bool) {
	return f.Stats.NumDeletions, f.StatsValid()
})

// valueBlocksSizeAnnotator is a manifest.Annotator that annotates B-Tree
// nodes with the sum of the files' Properties.ValueBlocksSize. The value block
// size may change once a table's stats are loaded asynchronously, so its
// values are marked as cacheable only if a file's stats have been loaded.
var valueBlockSizeAnnotator = manifest.SumAnnotator(func(f *manifest.TableMetadata) (uint64, bool) {
	return f.Stats.ValueBlocksSize, f.StatsValid()
})

// pointDeletionsBytesEstimateAnnotator is a manifest.Annotator that annotates
// B-Tree nodes with the sum of the files' PointDeletionsBytesEstimate. This
// value may change once a table's stats are loaded asynchronously, so its
// values are marked as cacheable only if a file's stats have been loaded.
var pointDeletionsBytesEstimateAnnotator = manifest.SumAnnotator(func(f *manifest.TableMetadata) (uint64, bool) {
	return f.Stats.PointDeletionsBytesEstimate, f.StatsValid()
})

// rangeDeletionsBytesEstimateAnnotator is a manifest.Annotator that annotates
// B-Tree nodes with the sum of the files' RangeDeletionsBytesEstimate. This
// value may change once a table's stats are loaded asynchronously, so its
// values are marked as cacheable only if a file's stats have been loaded.
var rangeDeletionsBytesEstimateAnnotator = manifest.SumAnnotator(func(f *manifest.TableMetadata) (uint64, bool) {
	return f.Stats.RangeDeletionsBytesEstimate, f.StatsValid()
})

// compressionStatsAnnotator is a manifest.Annotator that annotates B-tree nodes
// with the compression statistics for tables. Its annotation type is
// block.CompressionStats. The compression type may change once a table's stats
// are loaded asynchronously, so its values are marked as cacheable only if a
// file's stats have been loaded. Statistics for virtual tables are estimated
// from the physical table statistics, proportional to the estimated virtual
// table size.
var compressionStatsAnnotator = manifest.Annotator[CompressionMetrics]{
	Aggregator: compressionStatsAggregator{},
}

type compressionStatsAggregator struct{}

func (a compressionStatsAggregator) Zero(dst *CompressionMetrics) *CompressionMetrics {
	if dst == nil {
		return new(CompressionMetrics)
	}
	*dst = CompressionMetrics{}
	return dst
}

func (a compressionStatsAggregator) Accumulate(
	f *manifest.TableMetadata, dst *CompressionMetrics,
) (v *CompressionMetrics, cacheOK bool) {
	if statsValid := f.StatsValid(); !statsValid || f.Stats.CompressionStats.IsEmpty() {
		dst.CompressedBytesWithoutStats += f.Size
		return dst, statsValid
	}
	dst.Add(&f.Stats.CompressionStats)
	return dst, true
}

func (a compressionStatsAggregator) Merge(
	src *CompressionMetrics, dst *CompressionMetrics,
) *CompressionMetrics {
	dst.MergeWith(src)
	return dst
}
