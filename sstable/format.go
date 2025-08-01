// Copyright 2022 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/sstable/blockiter"
	"github.com/cockroachdb/pebble/sstable/colblk"
	"github.com/cockroachdb/pebble/sstable/rowblk"
)

// TableFormat specifies the format version for sstables. The legacy LevelDB
// format is format version 1.
type TableFormat uint32

// The available table formats, representing the tuple (magic number, version
// number). Note that these values are not (and should not) be serialized to
// disk. The ordering should follow the order the versions were introduced to
// Pebble (i.e. the history is linear).
const (
	TableFormatUnspecified TableFormat = iota
	TableFormatLevelDB
	TableFormatRocksDBv2

	// TableFormatPebblev1 adds block properties.
	TableFormatPebblev1

	// TableFormatPebblev2 adds range keys
	TableFormatPebblev2

	// TableFormatPebblev3 adds value blocks.
	TableFormatPebblev3

	// TableFormatPebblev4 adds DELSIZED tombstones.
	TableFormatPebblev4

	// TableFormatPebblev5 adds columnar blocks.
	TableFormatPebblev5 // Columnar blocks.

	// TableFormatPebblev6 adds:
	//  - checksum for footer;
	//  - blob value handles;
	//  - columnar metaindex;
	//  - MinLZ compression support.
	//
	// Supported by CockroachDB v25.2 and later.
	TableFormatPebblev6

	// TableFormatPebblev7 adds:
	//  - columnar + compressed properties block;
	//  - footer attributes.
	//
	// Supported by CockroachDB v25.3 and later.
	TableFormatPebblev7

	NumTableFormats

	TableFormatMax = NumTableFormats - 1

	// TableFormatMinSupported is the minimum format supported by Pebble.  This
	// package still supports older formats for uses outside of Pebble
	// (CockroachDB uses it to read data from backups that could be old).
	TableFormatMinSupported = TableFormatPebblev1
)

var footerSizes [NumTableFormats]int = [NumTableFormats]int{
	TableFormatLevelDB:   levelDBFooterLen,
	TableFormatRocksDBv2: rocksDBFooterLen,
	TableFormatPebblev1:  rocksDBFooterLen,
	TableFormatPebblev2:  rocksDBFooterLen,
	TableFormatPebblev3:  rocksDBFooterLen,
	TableFormatPebblev4:  rocksDBFooterLen,
	TableFormatPebblev5:  rocksDBFooterLen,
	TableFormatPebblev6:  checkedPebbleDBFooterLen,
	TableFormatPebblev7:  pebbleDBv7FooterLen,
}

// TableFormatPebblev4, in addition to DELSIZED, introduces the use of
// InternalKeyKindSSTableInternalObsoleteBit.
//
// 1. Motivation
//
// We have various related problems caused by Pebble snapshots:
//
// - P1: RANGEDELs that delete points in the same sstable, but the points
//   happen to not get deleted during compactions because of an open snapshot.
//   This causes very expensive iteration, that has been observed in
//   production deployments
//
// - P2: When iterating over a foreign sstable (in disaggregated storage), we
//   need to do (a) point collapsing to expose at most one point per user key,
//   (b) apply RANGEDELs in the sstable to hide deleted points in the same
//   sstable. This per-sstable point collapsing iteration needs to be very
//   efficient (ideally as efficient from a CPU perspective as iteration over
//   regular sstables) since foreign sstables can be very long-lived -- one of
//   the goals of disaggregated storage is to scale compute and disk bandwidth
//   resources as a function of the hot (from a write perspective) data and
//   not the whole data, so we don't want to have to rewrite foreign sstables
//   solely to improve read performance.
//
// The ideal solution for P2 would allow user-facing reads to utilize the
// existing SST iterators (with slight modifications) and with no loss of
// efficiency. And for P1 and P2 we would like to skip whole blocks of
// overwritten/deleted points. Even when we can't skip whole blocks, avoiding
// key comparisons at iteration time to discover what points are deleted is
// very desirable, since keys can be long.
//
// We observe that:
//
// - Reads:
//   - All user-facing reads in CockroachDB use iterators over the DB, hence
//     have a higher read seqnum than all sstables (there are some rare cases
//     that can violate this, but those are not important from a performance
//     optimization perspective).
//
//   - Certain internal-facing reads in CockroachDB use snapshots, but the
//     snapshots are shortlived enough that most L5 and L6 sstables will have
//     all seqnums lower than the snapshot seqnum.
//
// - Writes:
//   - We already do key comparisons between points when writing the sstable
//     to ensure that the sstable invariant (monotonically increasing internal
//     keys) is not violated. So we know which points share the same userkey,
//     and thereby which points are obsolete because there is a more recent
//     point in the same sstable.
//
//   - The compactionIter knows which point id deleted by a RANGEDEL even if
//     the point does need to be written because of a snapshot.
//
//   So this known information can be encoded in the sstable at write time and
//   utilized for optimized reading.
//
// 2. Solution
//
// We primarily scope the solution to the following point kinds: SET,
// SETWITHDEL, DEL, DELSIZED, SINGLEDEL. These are the ones marked locally
// obsolete, i.e., obsolete within the sstable, and we can guarantee that at
// most one point will be exposed per user key. MERGE keys create more
// complexity: MERGE followed by MERGE causes multiple keys to not be
// obsolete. Same applies for MERGE followed by SET/SETWITHDEL/DEL*. Note
// that:
//
// - For regular sst iteration, the obsolete marking is a performance
//   optimization, and multiple keys for the same userkey can be handled by
//   higher layers in the iterator tree (specifically pebble.Iterator).
//
// - For foreign sst iteration, we disallow MERGEs to be written to such
//   shared ssts (details below).
//
// The key kinds are marked with an obsolete bit
// (InternalKeyKindSSTableInternalObsoleteBit) when the key-value pair is
// obsolete. This marking is done within blockWriter, based on information
// passed to it by Writer. In turn, Writer uses a combination of key
// comparisons, and information provided by compactionIter to decide whether a
// key-value pair is obsolete. Additionally, a Pebble-internal
// BlockPropertyCollector (obsoleteKeyBlockPropertyCollector) is used to mark
// blocks where all key-value pairs are obsolete. Since the common case is
// non-obsolete blocks, this block property collector uses the empty byte
// slice to represent a non-obsolete block, which consumes no space in
// BlockHandleWithProperties.Props.
//
// At read time, the obsolete bit is only visible to the blockIter, which can
// be optionally configured to hide obsolete points. This hiding is only
// configured for data block iterators for sstables being read by user-facing
// iterators at a seqnum greater than the max seqnum in the sstable.
// Additionally, when this hiding is configured, a Pebble-internal block
// property filter (obsoleteKeyBlockPropertyFilter), is used to skip whole
// blocks that are obsolete.
//
// 2.1 Correctness
//
// Due to the level invariant, the sequence of seqnums for a user key in a
// sstable represents a contiguous subsequence of the seqnums for the userkey
// across the whole LSM, and is more recent than the seqnums in a sstable in a
// lower level. So exposing exactly one point from a sstable for a userkey
// will also mask the points for the userkey in lower levels. If we expose no
// point, because of RANGEDELs, that RANGEDEL will also mask the points in
// lower levels.
//
// Note that we do not need to do anything special at write time for
// SETWITHDEL and SINGLEDEL. This is because these key kinds are treated
// specially only by compactions, which typically do not hide obsolete points
// (see exception below). For regular reads, SETWITHDEL behaves the same as
// SET and SINGLEDEL behaves the same as DEL.
//
// 2.1.1 Compaction reads of a foreign sstable
//
// Compaction reads of a foreign sstable behave like regular reads in that
// only non-obsolete points are exposed. Consider a L5 foreign sstable with
// b.SINGLEDEL that is non-obsolete followed by obsolete b.DEL. And a L6
// foreign sstable with two b.SETs. The SINGLEDEL will be exposed, and not the
// DEL, but this is not a correctness issue since only one of the SETs in the
// L6 sstable will be exposed. However, this works only because we have
// limited the number of foreign sst levels to two, and is extremely fragile.
// For robust correctness, non-obsolete SINGLEDELs in foreign sstables should
// be exposed as DELs.
//
// Additionally, to avoid false positive accounting errors in DELSIZED, we
// should expose them as DEL.
//
// NB: as of writing this comment, we do not have end-to-end support for
// SINGLEDEL for disaggregated storage since pointCollapsingIterator (used by
// ScanInternal) does not support SINGLEDEL. So the disaggregated key spans
// are required to never have SINGLEDELs (which is fine for CockroachDB since
// only the MVCC key space uses disaggregated storage, and SINGLEDELs are only
// used for the non-MVCC locks and intents).
//
// 2.2 Strictness and MERGE
//
// Setting the obsolete bit on point keys is advanced usage, so we support two
// modes, both of which must be truthful when setting the obsolete bit, but
// vary in when they don't set the obsolete bit.
//
// - Non-strict: In this mode, the bit does not need to be set for keys that
//   are obsolete. Additionally, any sstable containing MERGE keys can only
//   use this mode. An iterator over such an sstable, when configured to
//   hideObsoletePoints, can expose multiple internal keys per user key, and
//   can expose keys that are deleted by rangedels in the same sstable. This
//   is the mode that non-advanced users should use. Pebble without
//   disaggregated storage will also use this mode and will best-effort set
//   the obsolete bit, to optimize iteration when snapshots have retained many
//   obsolete keys.
//
// - Strict: In this mode, every obsolete key must have the obsolete bit set,
//   and no MERGE keys are permitted. An iterator over such an sstable, when
//   configured to hideObsoletePoints satisfies two properties:
//   - S1: will expose at most one internal key per user key, which is the
//     most recent one.
//   - S2: will never expose keys that are deleted by rangedels in the same
//     sstable.
//
//   This is the mode for two use cases in disaggregated storage (which will
//   exclude parts of the key space that has MERGEs), for levels that contain
//   sstables that can become foreign sstables:
//   - Pebble compaction output to these levels that can become foreign
//     sstables.
//
//   - CockroachDB ingest operations that can ingest into the levels that can
//     become foreign sstables. Note, these are not sstables corresponding to
//     copied data for CockroachDB range snapshots. This case occurs for
//     operations like index backfills: these trivially satisfy the strictness
//     criteria since they only write one key per userkey.
//
//     TODO(sumeer): this latter case is not currently supported, since only
//     Writer.AddWithForceObsolete calls are permitted for writing strict
//     obsolete sstables. This is done to reduce the likelihood of bugs. One
//     simple way to lift this limitation would be to disallow adding any
//     RANGEDELs when a Pebble-external writer is trying to construct a strict
//     obsolete sstable.

// parseTableFormat parses the given magic bytes and version into its
// corresponding internal TableFormat.
func parseTableFormat(magic []byte, version uint32) (TableFormat, error) {
	switch string(magic) {
	case levelDBMagic:
		return TableFormatLevelDB, nil
	case rocksDBMagic:
		if version != rocksDBFormatVersion2 {
			return TableFormatUnspecified, base.CorruptionErrorf(
				"(unsupported rocksdb format version %d)", errors.Safe(version))
		}
		return TableFormatRocksDBv2, nil
	case pebbleDBMagic:
		switch version {
		case 1:
			return TableFormatPebblev1, nil
		case 2:
			return TableFormatPebblev2, nil
		case 3:
			return TableFormatPebblev3, nil
		case 4:
			return TableFormatPebblev4, nil
		case 5:
			return TableFormatPebblev5, nil
		case 6:
			return TableFormatPebblev6, nil
		case 7:
			return TableFormatPebblev7, nil
		default:
			return TableFormatUnspecified, base.CorruptionErrorf(
				"(unsupported pebble format version %d)", errors.Safe(version))
		}
	default:
		return TableFormatUnspecified, base.CorruptionErrorf(
			"(bad magic number: 0x%x)", magic)
	}
}

// BlockColumnar returns true iff the table format uses the columnar format for
// data, index and keyspan blocks.
func (f TableFormat) BlockColumnar() bool {
	return f >= TableFormatPebblev5
}

// FooterSize returns the maximum size of the footer for the table format.
func (f TableFormat) FooterSize() int {
	return footerSizes[f]
}

func (f TableFormat) newIndexIter() blockiter.Index {
	if !f.BlockColumnar() {
		return new(rowblk.IndexIter)
	}
	return new(colblk.IndexIter)
}

// AsTuple returns the TableFormat's (Magic String, Version) tuple.
func (f TableFormat) AsTuple() (string, uint32) {
	switch f {
	case TableFormatLevelDB:
		return levelDBMagic, 0
	case TableFormatRocksDBv2:
		return rocksDBMagic, 2
	case TableFormatPebblev1:
		return pebbleDBMagic, 1
	case TableFormatPebblev2:
		return pebbleDBMagic, 2
	case TableFormatPebblev3:
		return pebbleDBMagic, 3
	case TableFormatPebblev4:
		return pebbleDBMagic, 4
	case TableFormatPebblev5:
		return pebbleDBMagic, 5
	case TableFormatPebblev6:
		return pebbleDBMagic, 6
	case TableFormatPebblev7:
		return pebbleDBMagic, 7
	default:
		panic("sstable: unknown table format version tuple")
	}
}

// String returns the TableFormat (Magic String,Version) tuple.
func (f TableFormat) String() string {
	switch f {
	case TableFormatUnspecified:
		return "unspecified"
	case TableFormatLevelDB:
		return "(LevelDB)"
	case TableFormatRocksDBv2:
		return "(RocksDB,v2)"
	case TableFormatPebblev1:
		return "(Pebble,v1)"
	case TableFormatPebblev2:
		return "(Pebble,v2)"
	case TableFormatPebblev3:
		return "(Pebble,v3)"
	case TableFormatPebblev4:
		return "(Pebble,v4)"
	case TableFormatPebblev5:
		return "(Pebble,v5)"
	case TableFormatPebblev6:
		return "(Pebble,v6)"
	case TableFormatPebblev7:
		return "(Pebble,v7)"
	default:
		panic("sstable: unknown table format version tuple")
	}
}

var tableFormatStrings = func() map[string]TableFormat {
	strs := make(map[string]TableFormat, NumTableFormats)
	for f := TableFormatUnspecified; f < NumTableFormats; f++ {
		strs[f.String()] = f
	}
	return strs
}()

// ParseTableFormatString parses a TableFormat from its human-readable string
// representation.
func ParseTableFormatString(s string) (TableFormat, error) {
	f, ok := tableFormatStrings[s]
	if !ok {
		return TableFormatUnspecified, errors.Errorf("unknown table format %q", s)
	}
	return f, nil
}
