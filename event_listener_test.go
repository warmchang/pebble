// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/crlib/crtime"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/testutils"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/redact"
	"github.com/stretchr/testify/require"
)

// Verify event listener actions, as well as expected filesystem operations.
func TestEventListener(t *testing.T) {
	if runtime.GOARCH == "386" {
		t.Skip("skipped on 32-bit due to slightly varied output")
	}
	var d *DB
	var memLog base.InMemLogger
	mem := vfs.NewMem()
	require.NoError(t, mem.MkdirAll("ext", 0755))

	datadriven.RunTest(t, "testdata/event_listener", func(t *testing.T, td *datadriven.TestData) string {
		switch td.Cmd {
		case "open":
			memLog.Reset()
			lel := MakeLoggingEventListener(&memLog)
			flushBegin, flushEnd := lel.FlushBegin, lel.FlushEnd
			lel.FlushBegin = func(info FlushInfo) {
				// Make deterministic.
				info.InputBytes = 100
				flushBegin(info)
			}
			lel.FlushEnd = func(info FlushInfo) {
				// Make deterministic.
				info.InputBytes = 100
				flushEnd(info)
			}
			tableIngested := lel.TableIngested
			lel.TableIngested = func(info TableIngestInfo) {
				// Make deterministic.
				info.WaitFlushDuration = 200 * time.Millisecond
				info.ManifestUpdateDuration = 100 * time.Millisecond
				info.BlockReadDuration = 300 * time.Millisecond
				info.BlockReadBytes = 7894
				tableIngested(info)
			}
			opts := &Options{
				// The table stats collector runs asynchronously and its
				// timing is less predictable. It increments nextJobID, which
				// can make these tests flaky. The TableStatsLoaded event is
				// tested separately in TestTableStats.
				DisableTableStats:     true,
				FS:                    vfs.WithLogging(mem, memLog.Infof),
				FormatMajorVersion:    internalFormatNewest,
				EventListener:         &lel,
				MaxManifestFileSize:   1,
				L0CompactionThreshold: 10,
				WALDir:                "wal",
			}
			opts.Experimental.EnableColumnarBlocks = func() bool { return true }
			var err error
			d, err = Open("db", opts)
			if err != nil {
				return err.Error()
			}
			t := time.Now()
			d.timeNow = func() time.Time {
				t = t.Add(time.Second)
				return t
			}
			d.opts.private.testingAlwaysWaitForCleanup = true
			return memLog.String()

		case "close":
			memLog.Reset()
			if err := d.Close(); err != nil {
				return err.Error()
			}
			return memLog.String()

		case "flush":
			memLog.Reset()
			if err := d.Set([]byte("a"), nil, nil); err != nil {
				return err.Error()
			}
			if err := d.Flush(); err != nil {
				return err.Error()
			}
			return memLog.String()

		case "compact":
			memLog.Reset()
			if err := d.Set([]byte("a"), nil, nil); err != nil {
				return err.Error()
			}
			if err := d.Compact(context.Background(), []byte("a"), []byte("b"), false); err != nil {
				return err.Error()
			}
			return memLog.String()

		case "checkpoint":
			memLog.Reset()
			if err := d.Checkpoint("checkpoint"); err != nil {
				return err.Error()
			}
			return memLog.String()

		case "disable-file-deletions":
			memLog.Reset()
			d.mu.Lock()
			d.disableFileDeletions()
			d.mu.Unlock()
			return memLog.String()

		case "enable-file-deletions":
			memLog.Reset()
			func() {
				defer func() {
					if r := recover(); r != nil {
						memLog.Infof("%v", r)
					}
				}()
				d.mu.Lock()
				defer d.mu.Unlock()
				d.enableFileDeletions()
			}()
			d.TestOnlyWaitForCleaning()
			return memLog.String()

		case "ingest":
			memLog.Reset()
			f, err := mem.Create("ext/0", vfs.WriteCategoryUnspecified)
			if err != nil {
				return err.Error()
			}
			w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
				TableFormat: d.TableFormat(),
			})
			if err := w.Set([]byte("a"), nil); err != nil {
				return err.Error()
			}
			if err := w.Close(); err != nil {
				return err.Error()
			}
			if err := d.Ingest(context.Background(), []string{"ext/0"}); err != nil {
				return err.Error()
			}
			return memLog.String()

		case "ingest-flushable":
			memLog.Reset()

			// Prevent flushes during this test to ensure determinism.
			d.mu.Lock()
			d.mu.compact.flushing = true
			d.mu.Unlock()

			b := d.NewBatch()
			if err := b.Set([]byte("a"), nil, nil); err != nil {
				return err.Error()
			}
			if err := d.Apply(b, nil); err != nil {
				return err.Error()
			}
			writeTable := func(name string, key byte) error {
				f, err := mem.Create(name, vfs.WriteCategoryUnspecified)
				if err != nil {
					return err
				}
				w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), sstable.WriterOptions{
					TableFormat: d.TableFormat(),
				})
				if err := w.Set([]byte{key}, nil); err != nil {
					return err
				}
				if err := w.Close(); err != nil {
					return err
				}
				return nil
			}
			tableA, tableB := "ext/a", "ext/b"
			if err := writeTable(tableA, 'a'); err != nil {
				return err.Error()
			}
			if err := writeTable(tableB, 'b'); err != nil {
				return err.Error()
			}
			if err := d.Ingest(context.Background(), []string{tableA, tableB}); err != nil {
				return err.Error()
			}

			// Re-enable flushes, to allow the subsequent flush to proceed.
			d.mu.Lock()
			d.mu.compact.flushing = false
			d.mu.Unlock()
			if err := d.Flush(); err != nil {
				return err.Error()
			}
			return memLog.String()

		case "metrics":
			// The asynchronous loading of table stats can change metrics, so
			// wait for all the tables' stats to be loaded.
			d.mu.Lock()
			d.waitTableStats()
			d.mu.Unlock()

			return d.Metrics().StringForTests()

		case "sstables":
			var buf bytes.Buffer
			tableInfos, _ := d.SSTables()
			for i, level := range tableInfos {
				if len(level) == 0 {
					continue
				}
				fmt.Fprintf(&buf, "%d:\n", i)
				for _, m := range level {
					fmt.Fprintf(&buf, "  %d:[%s-%s]\n",
						m.FileNum, m.Smallest.UserKey, m.Largest.UserKey)
				}
			}
			return buf.String()

		default:
			return fmt.Sprintf("unknown command: %s", td.Cmd)
		}
	})
}

func TestWriteStallEvents(t *testing.T) {
	const flushCount = 10
	const writeStallEnd = "write stall ending"

	testCases := []struct {
		delayFlush bool
		expected   string
	}{
		{true, "memtable count limit reached"},
		{false, "L0 file count limit exceeded"},
	}

	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			stallEnded := make(chan struct{}, 1)
			createReleased := make(chan struct{}, flushCount)
			var log base.InMemLogger
			var delayOnce sync.Once
			listener := &EventListener{
				TableCreated: func(info TableCreateInfo) {
					if c.delayFlush == (info.Reason == "flushing") {
						delayOnce.Do(func() {
							<-createReleased
						})
					}
				},
				WriteStallBegin: func(info WriteStallBeginInfo) {
					log.Infof("%s", info.String())
					createReleased <- struct{}{}
				},
				WriteStallEnd: func() {
					log.Infof("%s", writeStallEnd)
					select {
					case stallEnded <- struct{}{}:
					default:
					}
				},
			}
			d, err := Open("db", &Options{
				EventListener:               listener,
				FS:                          vfs.NewMem(),
				MemTableSize:                initialMemTableSize,
				MemTableStopWritesThreshold: 2,
				L0CompactionThreshold:       2,
				L0StopWritesThreshold:       2,
			})
			require.NoError(t, err)
			defer d.Close()

			for i := 0; i < flushCount; i++ {
				require.NoError(t, d.Set([]byte("a"), nil, NoSync))

				ch, err := d.AsyncFlush()
				require.NoError(t, err)

				// If we're delaying the flush (because we're testing for memtable
				// write stalls), we can't wait for the flush to finish as doing so
				// would deadlock. If we're not delaying the flush (because we're
				// testing for L0 write stals), we wait for the flush to finish so we
				// don't create too many memtables which would trigger a memtable write
				// stall.
				if !c.delayFlush {
					<-ch
				}
				if strings.Contains(log.String(), c.expected) {
					break
				}
			}
			<-stallEnded

			events := log.String()
			require.Contains(t, events, c.expected)
			require.Contains(t, events, writeStallEnd)
			if testing.Verbose() {
				t.Logf("\n%s", events)
			}
		})
	}
}

type redactLogger struct {
	logger Logger
}

// Infof implements the Logger.Infof interface.
func (l redactLogger) Infof(format string, args ...interface{}) {
	l.logger.Infof("%s", redact.Sprintf(format, args...).Redact())
}

// Errorf implements the Logger.Errorf interface.
func (l redactLogger) Errorf(format string, args ...interface{}) {
	l.logger.Errorf("%s", redact.Sprintf(format, args...).Redact())
}

// Fatalf implements the Logger.Fatalf interface.
func (l redactLogger) Fatalf(format string, args ...interface{}) {
	l.logger.Fatalf("%s", redact.Sprintf(format, args...).Redact())
}

func TestEventListenerRedact(t *testing.T) {
	// The vast majority of event listener fields logged are safe and do not
	// need to be redacted. Verify that the rare, unsafe error does appear in
	// the log redacted.
	var log base.InMemLogger
	l := MakeLoggingEventListener(redactLogger{logger: &log})
	l.WALDeleted(WALDeleteInfo{
		JobID:   5,
		FileNum: base.DiskFileNum(20),
		Err:     errors.Errorf("unredacted error: %s", "unredacted string"),
	})
	require.Equal(t, "[JOB 5] WAL delete error: unredacted error: ‹×›\n", log.String())
}

func TestEventListenerEnsureDefaultsSetsAllCallbacks(t *testing.T) {
	e := EventListener{}
	e.EnsureDefaults(nil)
	testAllCallbacksSetInEventListener(t, e)
}

func TestMakeLoggingEventListenerSetsAllCallbacks(t *testing.T) {
	e := MakeLoggingEventListener(nil)
	testAllCallbacksSetInEventListener(t, e)
}

type mockLogger struct {
	infoFunc  func(format string, args ...interface{})
	errorFunc func(format string, args ...interface{})
	fatalFunc func(format string, args ...interface{})
}

func (l *mockLogger) Infof(format string, args ...interface{}) {
	if l.infoFunc != nil {
		l.infoFunc(format, args...)
	}
}

func (l *mockLogger) Errorf(format string, args ...interface{}) {
	if l.errorFunc != nil {
		l.errorFunc(format, args...)
	}
}

func (l *mockLogger) Fatalf(format string, args ...interface{}) {
	if l.fatalFunc != nil {
		l.fatalFunc(format, args...)
	}
}

func newCountingMockLogger(t *testing.T) (*mockLogger, *int, *int) {
	var infoCount, errorCount int
	return &mockLogger{
		infoFunc: func(format string, args ...interface{}) {
			infoCount++
		},
		errorFunc: func(format string, args ...interface{}) {
			errorCount++
		},
		fatalFunc: func(format string, args ...interface{}) {
			t.Fatal("Unexpected call to Fatalf")
		},
	}, &infoCount, &errorCount
}

func TestMakeLoggingEventListenerBackgroundErrorOtherError(t *testing.T) {
	mockLogger, infoCount, errorCount := newCountingMockLogger(t)
	e := MakeLoggingEventListener(mockLogger)

	e.BackgroundError(errors.New("an example error"))
	require.Equal(t, 0, *infoCount)
	require.Equal(t, 1, *errorCount)

	testAllCallbacksSetInEventListener(t, e)
}

func TestTeeEventListenerSetsAllCallbacks(t *testing.T) {
	e := TeeEventListener(EventListener{}, EventListener{})
	testAllCallbacksSetInEventListener(t, e)
}

func testAllCallbacksSetInEventListener(t *testing.T, e EventListener) {
	t.Helper()
	v := reflect.ValueOf(e)
	for i := 0; i < v.NumField(); i++ {
		fType := v.Type().Field(i)
		fVal := v.Field(i)
		require.Equal(t, reflect.Func, fType.Type.Kind(), "unexpected non-func field: %s", fType.Name)
		require.False(t, fVal.IsNil(), "unexpected nil field: %s", fType.Name)
	}
}

func TestLowDiskReporter(t *testing.T) {
	const totalBytes = 1000
	testCases := []struct {
		// time, as a fraction of lowDiskSpaceFrequency.
		time       float64
		availBytes uint64
		expected   bool
	}{
		{time: 1, availBytes: 900, expected: false},
		{time: 1.1, availBytes: 101, expected: false},
		// We reached the 10% threshold.
		{time: 1.2, availBytes: 100, expected: true},
		{time: 1.5, availBytes: 100, expected: false},
		{time: 1.9, availBytes: 100, expected: false},
		// Enough time has passed.
		{time: 2.3, availBytes: 100, expected: true},
		{time: 2.4, availBytes: 80, expected: false},
		// We reached the 5% threshold.
		{time: 2.5, availBytes: 50, expected: true},
		{time: 2.6, availBytes: 500, expected: false},
		{time: 2.7, availBytes: 50, expected: false},
		{time: 2.8, availBytes: 31, expected: false},
		// We reached the 3% threshold.
		{time: 2.9, availBytes: 29, expected: true},
		// We reached the 2% threshold.
		{time: 3.0, availBytes: 20, expected: true},
		// We reached the 1% threshold.
		{time: 3.1, availBytes: 10, expected: true},
		{time: 3.2, availBytes: 1, expected: false},
		// We don't post another event for a higher threshold until enough time has
		// passed.
		{time: 3.3, availBytes: 50, expected: false},
		{time: 4.2, availBytes: 50, expected: true},
	}
	var r lowDiskSpaceReporter
	for _, tc := range testCases {
		threshold, ok := r.findThreshold(tc.availBytes, totalBytes)
		result := ok && r.shouldReport(threshold, 123456789+crtime.Mono(tc.time*float64(lowDiskSpaceFrequency)))
		if result != tc.expected {
			t.Errorf("time: %v  expected: %t, got %t (threshold=%d,ok=%t)", tc.time, tc.expected, result, threshold, ok)
		}
	}
}

func TestLowDiskSpaceEvent(t *testing.T) {
	var lastInfo atomic.Value

	listener := &EventListener{
		LowDiskSpace: func(info LowDiskSpaceInfo) {
			lastInfo.Store(info)
		},
	}
	fs := &mockDiskUsageFS{
		FS: vfs.NewMem(),
	}
	fs.usage.Store(vfs.DiskUsage{
		AvailBytes: 1000,
		TotalBytes: 1000,
		UsedBytes:  0,
	})

	opts := &Options{
		FS:            fs,
		EventListener: listener,
	}

	d, err := Open("db", opts)
	require.NoError(t, err)
	defer d.Close()

	require.NoError(t, d.Set([]byte("a"), []byte("avalue"), nil))
	require.NoError(t, d.Flush())
	require.Nil(t, lastInfo.Load())

	fs.usage.Store(vfs.DiskUsage{
		AvailBytes: 50,
		TotalBytes: 1000,
		UsedBytes:  950,
	})

	require.NoError(t, d.Set([]byte("b"), []byte("bvalue"), nil))
	require.NoError(t, d.Flush())
	require.Equal(t, LowDiskSpaceInfo{
		AvailBytes:       50,
		TotalBytes:       1000,
		PercentThreshold: 5,
	}, lastInfo.Load())
}

type mockDiskUsageFS struct {
	vfs.FS

	usage atomic.Value // vfs.DiskUsage
}

func (fs *mockDiskUsageFS) GetDiskUsage(path string) (vfs.DiskUsage, error) {
	return fs.usage.Load().(vfs.DiskUsage), nil
}

func TestSSTCorruptionEvent(t *testing.T) {
	for _, test := range []string{"missing-file", "missing-before-open", "meta-block-corruption", "data-block-corruption"} {
		t.Run(test, func(t *testing.T) {
			var mu sync.Mutex
			var events []DataCorruptionInfo
			fs := vfs.NewMem()
			opts := &Options{
				FS: fs,
				// We use panicLogger to avoid errors being printed and make sure Fatalf
				// is not called.
				Logger: panicLogger{},
				EventListener: &EventListener{
					DataCorruption: func(info DataCorruptionInfo) {
						mu.Lock()
						defer mu.Unlock()
						events = append(events, info)
					},
				},
				DisableAutomaticCompactions: true,
			}
			d, err := Open("", opts)
			require.NoError(t, err)
			key := func(k int) []byte {
				return []byte(fmt.Sprintf("key-%05d", k))
			}

			for i := 0; i < 100; i++ {
				d.Set(key(i), []byte(fmt.Sprintf("value-%05d", i)), nil)
			}
			require.NoError(t, d.Flush())
			require.NoError(t, d.Compact(
				context.Background(), []byte("a"), []byte("z"), false /* parallelize */))

			// We expect a single sst file.
			files := testutils.CheckErr(fs.List(""))
			files = slices.DeleteFunc(files, func(name string) bool {
				return !strings.HasSuffix(name, ".sst")
			})
			require.Lenf(t, files, 1, "expected a single sst file, got %v", files)
			sstFileName := files[0]

			switch test {
			case "missing-file":
				require.NoError(t, fs.Remove(sstFileName))
			case "missing-before-open":
				require.NoError(t, d.Close())
				require.NoError(t, fs.Remove(sstFileName))
				opts.DisableConsistencyCheck = true
				d, err = Open("", opts)
				require.NoError(t, err)
			case "meta-block-corruption":
				buf, err := fs.UnsafeGetFileDataBuffer(sstFileName)
				require.NoError(t, err)
				buf[len(buf)-100]++
			case "data-block-corruption":
				buf, err := fs.UnsafeGetFileDataBuffer(sstFileName)
				require.NoError(t, err)
				buf[20]++
			default:
				t.Fatalf("invalid test")
			}
			_, _, err = d.Get(key(5))
			require.Error(t, err)
			require.True(t, IsCorruptionError(err))
			infoInError := ExtractDataCorruptionInfo(err)
			require.NotNil(t, infoInError)
			require.Greater(t, len(events), 0)
			info := events[0]
			require.Equal(t, info.Path, sstFileName)
			require.False(t, info.IsRemote)
			require.Equal(t, base.UserKeyBoundsInclusive(key(0), key(99)), info.Bounds)
			require.Equal(t, info, *infoInError)

			d.Close()
		})
	}
}
