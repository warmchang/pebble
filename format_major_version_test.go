// Copyright 2021 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/vfs/atomicfs"
	"github.com/stretchr/testify/require"
)

// TestFormatMajorVersionValues checks that we don't accidentally change the
// numbers of format versions.
func TestFormatMajorVersionStableValues(t *testing.T) {
	require.Equal(t, FormatDefault, FormatMajorVersion(0))

	require.Equal(t, FormatFlushableIngest, FormatMajorVersion(13))
	require.Equal(t, FormatPrePebblev1MarkedCompacted, FormatMajorVersion(14))
	require.Equal(t, FormatDeleteSizedAndObsolete, FormatMajorVersion(15))
	require.Equal(t, FormatVirtualSSTables, FormatMajorVersion(16))
	require.Equal(t, FormatSyntheticPrefixSuffix, FormatMajorVersion(17))
	require.Equal(t, FormatFlushableIngestExcises, FormatMajorVersion(18))
	require.Equal(t, FormatColumnarBlocks, FormatMajorVersion(19))
	require.Equal(t, FormatWALSyncChunks, FormatMajorVersion(20))
	require.Equal(t, FormatTableFormatV6, FormatMajorVersion(21))
	require.Equal(t, formatDeprecatedExperimentalValueSeparation, FormatMajorVersion(22))
	require.Equal(t, formatFooterAttributes, FormatMajorVersion(23))
	require.Equal(t, FormatValueSeparation, FormatMajorVersion(24))

	// When we add a new version, we should add a check for the new version in
	// addition to updating these expected values.
	require.Equal(t, FormatNewest, FormatMajorVersion(25))
	require.Equal(t, internalFormatNewest, FormatMajorVersion(25))
	require.Equal(t, FormatExciseBoundsRecord, FormatMajorVersion(25))
}

func TestFormatMajorVersion_MigrationDefined(t *testing.T) {
	for v := FormatMinSupported; v <= FormatNewest; v++ {
		if _, ok := formatMajorVersionMigrations[v]; !ok {
			t.Errorf("format major version %d has no migration defined", v)
		}
	}
}

func TestRatchetFormat(t *testing.T) {
	fs := vfs.NewMem()
	opts := &Options{FS: fs}
	opts.WithFSDefaults()
	d, err := Open("", opts)
	require.NoError(t, err)
	require.NoError(t, d.Set([]byte("foo"), []byte("bar"), Sync))
	require.Equal(t, FormatFlushableIngest, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatPrePebblev1MarkedCompacted))
	require.Equal(t, FormatPrePebblev1MarkedCompacted, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatDeleteSizedAndObsolete))
	require.Equal(t, FormatDeleteSizedAndObsolete, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatVirtualSSTables))
	require.Equal(t, FormatVirtualSSTables, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatSyntheticPrefixSuffix))
	require.Equal(t, FormatSyntheticPrefixSuffix, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatFlushableIngestExcises))
	require.Equal(t, FormatFlushableIngestExcises, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatColumnarBlocks))
	require.Equal(t, FormatColumnarBlocks, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatWALSyncChunks))
	require.Equal(t, FormatWALSyncChunks, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatTableFormatV6))
	require.Equal(t, FormatTableFormatV6, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(formatDeprecatedExperimentalValueSeparation))
	require.Equal(t, formatDeprecatedExperimentalValueSeparation, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(formatFooterAttributes))
	require.Equal(t, formatFooterAttributes, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatValueSeparation))
	require.Equal(t, FormatValueSeparation, d.FormatMajorVersion())
	require.NoError(t, d.RatchetFormatMajorVersion(FormatExciseBoundsRecord))
	require.Equal(t, FormatExciseBoundsRecord, d.FormatMajorVersion())

	require.NoError(t, d.Close())

	// If we Open the database again, leaving the default format, the
	// database should Open using the persisted FormatNewest.
	opts = &Options{FS: fs, Logger: testLogger{t}}
	opts.WithFSDefaults()
	d, err = Open("", opts)
	require.NoError(t, err)
	require.Equal(t, internalFormatNewest, d.FormatMajorVersion())
	require.NoError(t, d.Close())

	// Move the marker to a version that does not exist.
	m, _, err := atomicfs.LocateMarker(fs, "", formatVersionMarkerName)
	require.NoError(t, err)
	require.NoError(t, m.Move("999999"))
	require.NoError(t, m.Close())

	opts = &Options{
		FS:                 fs,
		FormatMajorVersion: FormatMinSupported,
	}
	opts.WithFSDefaults()
	_, err = Open("", opts)
	require.Error(t, err)
	require.EqualError(t, err, `pebble: database "" written in unknown format major version 999999`)
}

func testBasicDB(d *DB) error {
	key := []byte("a")
	value := []byte("b")
	if err := d.Set(key, value, nil); err != nil {
		return err
	}
	if err := d.Flush(); err != nil {
		return err
	}
	if err := d.Compact(context.Background(), nil, []byte("\xff"), false); err != nil {
		return err
	}

	iter, _ := d.NewIter(nil)
	for valid := iter.First(); valid; valid = iter.Next() {
	}
	if err := iter.Close(); err != nil {
		return err
	}
	return nil
}

func TestFormatMajorVersions(t *testing.T) {
	for vers := FormatMinSupported; vers <= FormatNewest; vers++ {
		t.Run(fmt.Sprintf("vers=%03d", vers), func(t *testing.T) {
			fs := vfs.NewCrashableMem()
			opts := &Options{
				FS:                 fs,
				FormatMajorVersion: vers,
				Logger:             testLogger{t},
			}
			opts.WithFSDefaults()

			// Create a database at this format major version and perform
			// some very basic operations.
			d, err := Open("", opts)
			require.NoError(t, err)
			require.NoError(t, testBasicDB(d))
			require.NoError(t, d.Close())

			// Re-open the database at this format major version, and again
			// perform some basic operations.
			d, err = Open("", opts)
			require.NoError(t, err)
			require.NoError(t, testBasicDB(d))
			require.NoError(t, d.Close())

			t.Run("upgrade-at-open", func(t *testing.T) {
				for upgradeVers := vers + 1; upgradeVers <= FormatNewest; upgradeVers++ {
					t.Run(fmt.Sprintf("upgrade-vers=%03d", upgradeVers), func(t *testing.T) {
						// We use vfs.MemFS's CrashClone to perform an upgrade without
						// affecting the original filesystem.
						opts := opts.Clone()
						opts.FS = fs.CrashClone(vfs.CrashCloneCfg{UnsyncedDataPercent: 0})

						// Re-open the database, passing a higher format
						// major version in the Options to automatically
						// ratchet the format major version. Ensure some
						// basic operations pass.
						opts.FormatMajorVersion = upgradeVers
						d, err = Open("", opts)
						require.NoError(t, err)
						require.Equal(t, upgradeVers, d.FormatMajorVersion())
						require.NoError(t, testBasicDB(d))
						require.NoError(t, d.Close())

						// Re-open to ensure the upgrade persisted.
						d, err = Open("", opts)
						require.NoError(t, err)
						require.Equal(t, upgradeVers, d.FormatMajorVersion())
						require.NoError(t, testBasicDB(d))
						require.NoError(t, d.Close())
					})
				}
			})

			t.Run("upgrade-while-open", func(t *testing.T) {
				for upgradeVers := vers + 1; upgradeVers <= FormatNewest; upgradeVers++ {
					t.Run(fmt.Sprintf("upgrade-vers=%03d", upgradeVers), func(t *testing.T) {
						// Ensure the previous tests don't overwrite our
						// options.
						require.Equal(t, vers, opts.FormatMajorVersion)

						// We use vfs.MemFS's CrashClone to perform an upgrade without
						// affecting the original filesystem.
						opts := opts.Clone()
						opts.FS = fs.CrashClone(vfs.CrashCloneCfg{UnsyncedDataPercent: 0})

						// Re-open the database, still at the current format
						// major version. Perform some basic operations,
						// ratchet the format version up, and perform
						// additional basic operations.
						d, err = Open("", opts)
						require.NoError(t, err)
						require.NoError(t, testBasicDB(d))
						require.Equal(t, vers, d.FormatMajorVersion())
						require.NoError(t, d.RatchetFormatMajorVersion(upgradeVers))
						require.Equal(t, upgradeVers, d.FormatMajorVersion())
						require.NoError(t, testBasicDB(d))
						require.NoError(t, d.Close())

						// Re-open to ensure the upgrade persisted.
						d, err = Open("", opts)
						require.NoError(t, err)
						require.Equal(t, upgradeVers, d.FormatMajorVersion())
						require.NoError(t, testBasicDB(d))
						require.NoError(t, d.Close())
					})
				}
			})
		})
	}
}

func TestFormatMajorVersions_TableFormat(t *testing.T) {
	// NB: This test is intended to validate the mapping between every
	// FormatMajorVersion and sstable.TableFormat exhaustively. This serves as a
	// sanity check that new versions have a corresponding mapping. The test
	// fixture is intentionally verbose.

	m := map[FormatMajorVersion][2]sstable.TableFormat{
		FormatDefault:                               {sstable.TableFormatPebblev1, sstable.TableFormatPebblev3},
		FormatFlushableIngest:                       {sstable.TableFormatPebblev1, sstable.TableFormatPebblev3},
		FormatPrePebblev1MarkedCompacted:            {sstable.TableFormatPebblev1, sstable.TableFormatPebblev3},
		FormatDeleteSizedAndObsolete:                {sstable.TableFormatPebblev1, sstable.TableFormatPebblev4},
		FormatVirtualSSTables:                       {sstable.TableFormatPebblev1, sstable.TableFormatPebblev4},
		FormatSyntheticPrefixSuffix:                 {sstable.TableFormatPebblev1, sstable.TableFormatPebblev4},
		FormatFlushableIngestExcises:                {sstable.TableFormatPebblev1, sstable.TableFormatPebblev4},
		FormatColumnarBlocks:                        {sstable.TableFormatPebblev1, sstable.TableFormatPebblev5},
		FormatWALSyncChunks:                         {sstable.TableFormatPebblev1, sstable.TableFormatPebblev5},
		FormatTableFormatV6:                         {sstable.TableFormatPebblev1, sstable.TableFormatPebblev6},
		formatDeprecatedExperimentalValueSeparation: {sstable.TableFormatPebblev1, sstable.TableFormatPebblev6},
		formatFooterAttributes:                      {sstable.TableFormatPebblev1, sstable.TableFormatPebblev7},
		FormatValueSeparation:                       {sstable.TableFormatPebblev1, sstable.TableFormatPebblev7},
		FormatExciseBoundsRecord:                    {sstable.TableFormatPebblev1, sstable.TableFormatPebblev7},
	}

	// Valid versions.
	for fmv := FormatMinSupported; fmv <= internalFormatNewest; fmv++ {
		got := [2]sstable.TableFormat{fmv.MinTableFormat(), fmv.MaxTableFormat()}
		require.Equalf(t, m[fmv], got, "got %s; want %s", got, m[fmv])
		require.True(t, got[0] <= got[1] /* min <= max */)
	}

	// Invalid versions.
	fmv := internalFormatNewest + 1
	require.Panics(t, func() { _ = fmv.MaxTableFormat() })
	require.Panics(t, func() { _ = fmv.MinTableFormat() })
}

// TestFormatMajorVersions_ColumnarBlocks ensures that
// Experimental.EnableColumnarBlocks is respected on recent format major
// versions.
func TestFormatMajorVersions_ColumnarBlocks(t *testing.T) {
	type testCase struct {
		fmv     FormatMajorVersion
		colBlks bool
		want    sstable.TableFormat
	}
	testCases := []testCase{
		{
			fmv:     FormatTableFormatV6,
			colBlks: true,
			want:    sstable.TableFormatPebblev6,
		},
		{
			fmv:     FormatTableFormatV6,
			colBlks: false,
			want:    sstable.TableFormatPebblev4,
		},
		{
			fmv:     FormatColumnarBlocks,
			colBlks: true,
			want:    sstable.TableFormatPebblev5,
		},
		{
			fmv:     FormatColumnarBlocks,
			colBlks: false,
			want:    sstable.TableFormatPebblev4,
		},
		{
			fmv:     FormatFlushableIngestExcises,
			colBlks: true,
			want:    sstable.TableFormatPebblev4,
		},
		{
			fmv:     FormatNewest,
			colBlks: false,
			want:    sstable.TableFormatPebblev4,
		},
		{
			fmv:     formatDeprecatedExperimentalValueSeparation,
			colBlks: true,
			want:    sstable.TableFormatPebblev6,
		},
		{
			fmv:     formatDeprecatedExperimentalValueSeparation,
			colBlks: false,
			want:    sstable.TableFormatPebblev4,
		},
		{
			fmv:     FormatNewest,
			colBlks: true,
			want:    sstable.TableFormatPebblev7,
		},
	}
	for _, tc := range testCases {
		t.Run(fmt.Sprintf("(%s,%t)", tc.fmv, tc.colBlks), func(t *testing.T) {
			opts := &Options{
				FS:                 vfs.NewMem(),
				FormatMajorVersion: tc.fmv,
			}
			opts.Experimental.EnableColumnarBlocks = func() bool { return tc.colBlks }
			d, err := Open("", opts)
			require.NoError(t, err)
			defer func() { _ = d.Close() }()
			require.Equal(t, tc.want, d.TableFormat())
		})
	}
}
