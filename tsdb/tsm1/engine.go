// Package tsm1 provides a TSDB in the Time Structured Merge tree format.
package tsm1 // import "github.com/influxdata/platform/tsdb/tsm1"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/influxdata/influxql"
	"github.com/influxdata/platform/logger"
	"github.com/influxdata/platform/models"
	"github.com/influxdata/platform/pkg/bytesutil"
	"github.com/influxdata/platform/pkg/limiter"
	"github.com/influxdata/platform/pkg/metrics"
	"github.com/influxdata/platform/query"
	"github.com/influxdata/platform/tsdb"
	"github.com/influxdata/platform/tsdb/tsi1"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

//go:generate env GO111MODULE=on go run github.com/influxdata/platform/tools/tmpl -i -data=file_store.gen.go.tmpldata file_store.gen.go.tmpl=file_store.gen.go
//go:generate env GO111MODULE=on go run github.com/influxdata/platform/tools/tmpl -i -d isArray=y -data=file_store.gen.go.tmpldata file_store.gen.go.tmpl=file_store_array.gen.go
//go:generate env GO111MODULE=on go run github.com/benbjohnson/tmpl -data=@encoding.gen.go.tmpldata encoding.gen.go.tmpl
//go:generate env GO111MODULE=on go run github.com/benbjohnson/tmpl -data=@compact.gen.go.tmpldata compact.gen.go.tmpl
//go:generate env GO111MODULE=on go run github.com/benbjohnson/tmpl -data=@reader.gen.go.tmpldata reader.gen.go.tmpl

var (
	// Static objects to prevent small allocs.
	keyFieldSeparatorBytes = []byte(keyFieldSeparator)
	emptyBytes             = []byte{}
)

var (
	tsmGroup                  = metrics.MustRegisterGroup("platform-tsm1")
	numberOfRefCursorsCounter = metrics.MustRegisterCounter("cursors_ref", metrics.WithGroup(tsmGroup))
)

// NewContextWithMetricsGroup creates a new context with a tsm1 metrics.Group for tracking
// various metrics when accessing TSM data.
func NewContextWithMetricsGroup(ctx context.Context) context.Context {
	group := metrics.NewGroup(tsmGroup)
	return metrics.NewContextWithGroup(ctx, group)
}

// MetricsGroupFromContext returns the tsm1 metrics.Group associated with the context
// or nil if no group has been assigned.
func MetricsGroupFromContext(ctx context.Context) *metrics.Group {
	return metrics.GroupFromContext(ctx)
}

const (
	// keyFieldSeparator separates the series key from the field name in the composite key
	// that identifies a specific field in series
	keyFieldSeparator = "#!~#"

	// deleteFlushThreshold is the size in bytes of a batch of series keys to delete.
	deleteFlushThreshold = 50 * 1024 * 1024

	// MaxPointsPerBlock is the maximum number of points in an encoded block in a TSM file
	MaxPointsPerBlock = 1000
)

// An EngineOption is a functional option for changing the configuration of
// an Engine.
type EngineOption func(i *Engine)

// WithWAL sets the WAL for the Engine
var WithWAL = func(wal Log) EngineOption {
	// be defensive: it's very easy to pass in a nil WAL here
	// which will panic. Set any nil WALs to the NopWAL.
	if pwal, _ := wal.(*WAL); pwal == nil {
		wal = NopWAL{}
	}

	return func(e *Engine) {
		e.WAL = wal
	}
}

// WithTraceLogging sets if trace logging is enabled for the engine.
var WithTraceLogging = func(logging bool) EngineOption {
	return func(e *Engine) {
		e.FileStore.enableTraceLogging(logging)
	}
}

// WithCompactionPlanner sets the compaction planner for the engine.
var WithCompactionPlanner = func(planner CompactionPlanner) EngineOption {
	return func(e *Engine) {
		planner.SetFileStore(e.FileStore)
		e.CompactionPlan = planner
	}
}

// Engine represents a storage engine with compressed blocks.
type Engine struct {
	mu sync.RWMutex

	index *tsi1.Index

	// The following group of fields is used to track the state of level compactions within the
	// Engine. The WaitGroup is used to monitor the compaction goroutines, the 'done' channel is
	// used to signal those goroutines to shutdown. Every request to disable level compactions will
	// call 'Wait' on 'wg', with the first goroutine to arrive (levelWorkers == 0 while holding the
	// lock) will close the done channel and re-assign 'nil' to the variable. Re-enabling will
	// decrease 'levelWorkers', and when it decreases to zero, level compactions will be started
	// back up again.

	wg           *sync.WaitGroup // waitgroup for active level compaction goroutines
	done         chan struct{}   // channel to signal level compactions to stop
	levelWorkers int             // Number of "workers" that expect compactions to be in a disabled state

	snapDone chan struct{}   // channel to signal snapshot compactions to stop
	snapWG   *sync.WaitGroup // waitgroup for running snapshot compactions

	path         string
	sfile        *tsdb.SeriesFile
	logger       *zap.Logger // Logger to be used for important messages
	traceLogger  *zap.Logger // Logger to be used when trace-logging is on.
	traceLogging bool

	WAL            Log
	Cache          *Cache
	Compactor      *Compactor
	CompactionPlan CompactionPlanner
	FileStore      *FileStore

	MaxPointsPerBlock int

	// CacheFlushMemorySizeThreshold specifies the minimum size threshold for
	// the cache when the engine should write a snapshot to a TSM file
	CacheFlushMemorySizeThreshold uint64

	// CacheFlushWriteColdDuration specifies the length of time after which if
	// no writes have been committed to the WAL, the engine will write
	// a snapshot of the cache to a TSM file
	CacheFlushWriteColdDuration time.Duration

	// Invoked when creating a backup file "as new".
	formatFileName FormatFileNameFunc

	// Controls whether to enabled compactions when the engine is open
	enableCompactionsOnOpen bool

	compactionTracker   *compactionTracker // Used to track state of compactions.
	defaultMetricLabels prometheus.Labels  // N.B this must not be mutated after Open is called.

	// Limiter for concurrent compactions.
	compactionLimiter limiter.Fixed

	scheduler *scheduler
}

// NewEngine returns a new instance of Engine.
func NewEngine(path string, idx *tsi1.Index, config Config, options ...EngineOption) *Engine {
	fs := NewFileStore(path)
	fs.openLimiter = limiter.NewFixed(config.MaxConcurrentOpens)
	fs.tsmMMAPWillNeed = config.MADVWillNeed

	cache := NewCache(uint64(config.Cache.MaxMemorySize))

	c := NewCompactor()
	c.Dir = path
	c.FileStore = fs
	c.RateLimit = limiter.NewRate(
		int(config.Compaction.Throughput),
		int(config.Compaction.ThroughputBurst))

	// determine max concurrent compactions informed by the system
	maxCompactions := config.Compaction.MaxConcurrent
	if maxCompactions == 0 {
		maxCompactions = runtime.GOMAXPROCS(0) / 2 // Default to 50% of cores for compactions

		// On systems with more cores, cap at 4 to reduce disk utilization.
		if maxCompactions > 4 {
			maxCompactions = 4
		}

		if maxCompactions < 1 {
			maxCompactions = 1
		}
	}

	// Don't allow more compactions to run than cores.
	if maxCompactions > runtime.GOMAXPROCS(0) {
		maxCompactions = runtime.GOMAXPROCS(0)
	}

	logger := zap.NewNop()
	e := &Engine{
		path:        path,
		index:       idx,
		sfile:       idx.SeriesFile(),
		logger:      logger,
		traceLogger: logger,

		WAL:   NopWAL{},
		Cache: cache,

		FileStore: fs,
		Compactor: c,
		CompactionPlan: NewDefaultPlanner(fs,
			time.Duration(config.Compaction.FullWriteColdDuration)),

		CacheFlushMemorySizeThreshold: uint64(config.Cache.SnapshotMemorySize),
		CacheFlushWriteColdDuration:   time.Duration(config.Cache.SnapshotWriteColdDuration),
		enableCompactionsOnOpen:       true,
		formatFileName:                DefaultFormatFileName,
		compactionLimiter:             limiter.NewFixed(maxCompactions),
		scheduler:                     newScheduler(maxCompactions),
	}

	for _, option := range options {
		option(e)
	}

	return e
}

func (e *Engine) WithFormatFileNameFunc(formatFileNameFunc FormatFileNameFunc) {
	e.Compactor.WithFormatFileNameFunc(formatFileNameFunc)
	e.formatFileName = formatFileNameFunc
}

func (e *Engine) WithParseFileNameFunc(parseFileNameFunc ParseFileNameFunc) {
	e.FileStore.WithParseFileNameFunc(parseFileNameFunc)
	e.Compactor.WithParseFileNameFunc(parseFileNameFunc)
}

func (e *Engine) WithFileStoreObserver(obs FileStoreObserver) {
	e.FileStore.WithObserver(obs)
}

func (e *Engine) WithCompactionPlanner(planner CompactionPlanner) {
	planner.SetFileStore(e.FileStore)
	e.CompactionPlan = planner
}

// SetDefaultMetricLabels sets the default labels for metrics on the engine.
// It must be called before the Engine is opened.
func (e *Engine) SetDefaultMetricLabels(labels prometheus.Labels) {
	e.defaultMetricLabels = labels
}

// SetEnabled sets whether the engine is enabled.
func (e *Engine) SetEnabled(enabled bool) {
	e.enableCompactionsOnOpen = enabled
	e.SetCompactionsEnabled(enabled)
}

// SetCompactionsEnabled enables compactions on the engine.  When disabled
// all running compactions are aborted and new compactions stop running.
func (e *Engine) SetCompactionsEnabled(enabled bool) {
	if enabled {
		e.enableSnapshotCompactions()
		e.enableLevelCompactions(false)
	} else {
		e.disableSnapshotCompactions()
		e.disableLevelCompactions(false)
	}
}

// enableLevelCompactions will request that level compactions start back up again
//
// 'wait' signifies that a corresponding call to disableLevelCompactions(true) was made at some
// point, and the associated task that required disabled compactions is now complete
func (e *Engine) enableLevelCompactions(wait bool) {
	// If we don't need to wait, see if we're already enabled
	if !wait {
		e.mu.RLock()
		if e.done != nil {
			e.mu.RUnlock()
			return
		}
		e.mu.RUnlock()
	}

	e.mu.Lock()
	if wait {
		e.levelWorkers -= 1
	}
	if e.levelWorkers != 0 || e.done != nil {
		// still waiting on more workers or already enabled
		e.mu.Unlock()
		return
	}

	// last one to enable, start things back up
	e.Compactor.EnableCompactions()
	e.done = make(chan struct{})
	wg := new(sync.WaitGroup)
	wg.Add(1)
	e.wg = wg
	e.mu.Unlock()

	go func() { defer wg.Done(); e.compact(wg) }()
}

// disableLevelCompactions will stop level compactions before returning.
//
// If 'wait' is set to true, then a corresponding call to enableLevelCompactions(true) will be
// required before level compactions will start back up again.
func (e *Engine) disableLevelCompactions(wait bool) {
	e.mu.Lock()
	old := e.levelWorkers
	if wait {
		e.levelWorkers += 1
	}

	// Hold onto the current done channel so we can wait on it if necessary
	waitCh := e.done
	wg := e.wg

	if old == 0 && e.done != nil {
		// It's possible we have closed the done channel and released the lock and another
		// goroutine has attempted to disable compactions.  We're current in the process of
		// disabling them so check for this and wait until the original completes.
		select {
		case <-e.done:
			e.mu.Unlock()
			return
		default:
		}

		// Prevent new compactions from starting
		e.Compactor.DisableCompactions()

		// Stop all background compaction goroutines
		close(e.done)
		e.mu.Unlock()
		wg.Wait()

		// Signal that all goroutines have exited.
		e.mu.Lock()
		e.done = nil
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	// Compaction were already disabled.
	if waitCh == nil {
		return
	}

	// We were not the first caller to disable compactions and they were in the process
	// of being disabled.  Wait for them to complete before returning.
	<-waitCh
	wg.Wait()
}

func (e *Engine) enableSnapshotCompactions() {
	// Check if already enabled under read lock
	e.mu.RLock()
	if e.snapDone != nil {
		e.mu.RUnlock()
		return
	}
	e.mu.RUnlock()

	// Check again under write lock
	e.mu.Lock()
	if e.snapDone != nil {
		e.mu.Unlock()
		return
	}

	e.Compactor.EnableSnapshots()
	e.snapDone = make(chan struct{})
	wg := new(sync.WaitGroup)
	wg.Add(1)
	e.snapWG = wg
	e.mu.Unlock()

	go func() { defer wg.Done(); e.compactCache() }()
}

func (e *Engine) disableSnapshotCompactions() {
	e.mu.Lock()
	if e.snapDone == nil {
		e.mu.Unlock()
		return
	}

	// We may be in the process of stopping snapshots.  See if the channel
	// was closed.
	select {
	case <-e.snapDone:
		e.mu.Unlock()
		return
	default:
	}

	// first one here, disable and wait for completion
	close(e.snapDone)
	e.Compactor.DisableSnapshots()
	wg := e.snapWG
	e.mu.Unlock()

	// Wait for the snapshot goroutine to exit.
	wg.Wait()

	// Signal that the goroutines are exit and everything is stopped by setting
	// snapDone to nil.
	e.mu.Lock()
	e.snapDone = nil
	e.mu.Unlock()

	// If the cache is empty, free up its resources as well.
	if e.Cache.Size() == 0 {
		e.Cache.Free()
	}
}

// ScheduleFullCompaction will force the engine to fully compact all data stored.
// This will cancel and running compactions and snapshot any data in the cache to
// TSM files.  This is an expensive operation.
func (e *Engine) ScheduleFullCompaction() error {
	// Snapshot any data in the cache
	if err := e.WriteSnapshot(); err != nil {
		return err
	}

	// Cancel running compactions
	e.SetCompactionsEnabled(false)

	// Ensure compactions are restarted
	defer e.SetCompactionsEnabled(true)

	// Force the planner to only create a full plan.
	e.CompactionPlan.ForceFull()
	return nil
}

// Path returns the path the engine was opened with.
func (e *Engine) Path() string { return e.path }

func (e *Engine) SetFieldName(measurement []byte, name string) {
	e.index.SetFieldName(measurement, name)
}

func (e *Engine) MeasurementExists(name []byte) (bool, error) {
	return e.index.MeasurementExists(name)
}

func (e *Engine) MeasurementNamesByRegex(re *regexp.Regexp) ([][]byte, error) {
	return e.index.MeasurementNamesByRegex(re)
}

func (e *Engine) HasTagKey(name, key []byte) (bool, error) {
	return e.index.HasTagKey(name, key)
}

func (e *Engine) MeasurementTagKeysByExpr(name []byte, expr influxql.Expr) (map[string]struct{}, error) {
	return e.index.MeasurementTagKeysByExpr(name, expr)
}

func (e *Engine) TagKeyCardinality(name, key []byte) int {
	return e.index.TagKeyCardinality(name, key)
}

// SeriesN returns the unique number of series in the index.
func (e *Engine) SeriesN() int64 {
	return e.index.SeriesN()
}

// LastModified returns the time when this shard was last modified.
func (e *Engine) LastModified() time.Time {
	fsTime := e.FileStore.LastModified()

	if e.WAL.LastWriteTime().After(fsTime) {
		return e.WAL.LastWriteTime()
	}
	return fsTime
}

// MeasurementStats returns the current measurement stats for the engine.
func (e *Engine) MeasurementStats() (MeasurementStats, error) {
	return e.FileStore.MeasurementStats()
}

// DiskSize returns the total size in bytes of all TSM and WAL segments on disk.
func (e *Engine) DiskSize() int64 {
	walDiskSizeBytes := e.WAL.DiskSizeBytes()
	return e.FileStore.DiskSizeBytes() + walDiskSizeBytes
}

func (e *Engine) initTrackers() {
	mmu.Lock()
	defer mmu.Unlock()

	if bms == nil {
		// Initialise metrics if an engine has not done so already.
		bms = newBlockMetrics(e.defaultMetricLabels)
	}

	// Propagate prometheus metrics down into trackers.
	e.compactionTracker = newCompactionTracker(bms.compactionMetrics, e.defaultMetricLabels)
	e.FileStore.tracker = newFileTracker(bms.fileMetrics, e.defaultMetricLabels)
	e.Cache.tracker = newCacheTracker(bms.cacheMetrics, e.defaultMetricLabels)

	// Set default metrics on WAL if enabled.
	if wal, ok := e.WAL.(*WAL); ok {
		wal.tracker = newWALTracker(bms.walMetrics, e.defaultMetricLabels)
	}
	e.scheduler.setCompactionTracker(e.compactionTracker)
}

// Open opens and initializes the engine.
func (e *Engine) Open() error {
	e.initTrackers()

	if err := os.MkdirAll(e.path, 0777); err != nil {
		return err
	}

	if err := e.cleanup(); err != nil {
		return err
	}

	if err := e.WAL.Open(); err != nil {
		return err
	}

	if err := e.FileStore.Open(); err != nil {
		return err
	}

	if err := e.reloadCache(); err != nil {
		return err
	}

	e.Compactor.Open()

	if e.enableCompactionsOnOpen {
		e.SetCompactionsEnabled(true)
	}

	return nil
}

// Close closes the engine. Subsequent calls to Close are a nop.
func (e *Engine) Close() error {
	e.SetCompactionsEnabled(false)

	// Lock now and close everything else down.
	e.mu.Lock()
	defer e.mu.Unlock()
	e.done = nil // Ensures that the channel will not be closed again.

	if err := e.FileStore.Close(); err != nil {
		return err
	}
	return e.WAL.Close()
}

// WithLogger sets the logger for the engine.
func (e *Engine) WithLogger(log *zap.Logger) {
	e.logger = log.With(zap.String("engine", "tsm1"))

	if e.traceLogging {
		e.traceLogger = e.logger
	}

	if wal, ok := e.WAL.(*WAL); ok {
		wal.WithLogger(e.logger)
	}

	e.FileStore.WithLogger(e.logger)
}

// IsIdle returns true if the cache is empty, there are no running compactions and the
// shard is fully compacted.
func (e *Engine) IsIdle() bool {
	cacheEmpty := e.Cache.Size() == 0
	return cacheEmpty && e.compactionTracker.AllActive() == 0 && e.CompactionPlan.FullyCompacted()
}

// Free releases any resources held by the engine to free up memory or CPU.
func (e *Engine) Free() error {
	e.Cache.Free()
	return e.FileStore.Free()
}

// WritePoints writes metadata and point data into the engine.
// It returns an error if new points are added to an existing key.
func (e *Engine) WritePoints(points []models.Point) error {
	values := make(map[string][]Value, len(points))
	var (
		keyBuf  []byte
		baseLen int
	)

	for _, p := range points {
		keyBuf = append(keyBuf[:0], p.Key()...)
		keyBuf = append(keyBuf, keyFieldSeparator...)
		baseLen = len(keyBuf)
		iter := p.FieldIterator()
		t := p.Time().UnixNano()
		for iter.Next() {
			keyBuf = append(keyBuf[:baseLen], iter.FieldKey()...)

			var v Value
			switch iter.Type() {
			case models.Float:
				fv, err := iter.FloatValue()
				if err != nil {
					return err
				}
				v = NewFloatValue(t, fv)
			case models.Integer:
				iv, err := iter.IntegerValue()
				if err != nil {
					return err
				}
				v = NewIntegerValue(t, iv)
			case models.Unsigned:
				iv, err := iter.UnsignedValue()
				if err != nil {
					return err
				}
				v = NewUnsignedValue(t, iv)
			case models.String:
				v = NewStringValue(t, iter.StringValue())
			case models.Boolean:
				bv, err := iter.BooleanValue()
				if err != nil {
					return err
				}
				v = NewBooleanValue(t, bv)
			default:
				return fmt.Errorf("unknown field type for %s: %s", string(iter.FieldKey()), p.String())
			}
			values[string(keyBuf)] = append(values[string(keyBuf)], v)
		}
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	// first try to write to the cache
	if err := e.Cache.WriteMulti(values); err != nil {
		return err
	}

	// Then make the write durable in the cache.
	if _, err := e.WAL.WriteMulti(values); err != nil {
		return err
	}

	return nil
}

// DeleteSeriesRange removes the values between min and max (inclusive) from all series
func (e *Engine) DeleteSeriesRange(itr tsdb.SeriesIterator, min, max int64) error {
	return e.DeleteSeriesRangeWithPredicate(itr, func(name []byte, tags models.Tags) (int64, int64, bool) {
		return min, max, true
	})
}

// DeleteSeriesRangeWithPredicate removes the values between min and max (inclusive) from all series
// for which predicate() returns true. If predicate() is nil, then all values in range are removed.
func (e *Engine) DeleteSeriesRangeWithPredicate(itr tsdb.SeriesIterator, predicate func(name []byte, tags models.Tags) (int64, int64, bool)) error {
	var disableOnce bool

	// Ensure that the index does not compact away the measurement or series we're
	// going to delete before we're done with them.
	e.index.DisableCompactions()
	defer e.index.EnableCompactions()
	e.index.Wait()

	fs, err := e.index.RetainFileSet()
	if err != nil {
		return err
	}
	defer fs.Release()

	var (
		sz       int
		min, max int64 = math.MinInt64, math.MaxInt64

		// Indicator that the min/max time for the current batch has changed and
		// we need to flush the current batch before appending to it.
		flushBatch bool
	)

	// These are reversed from min/max to ensure they are different the first time through.
	newMin, newMax := int64(math.MaxInt64), int64(math.MinInt64)

	// There is no predicate, so setup newMin/newMax to delete the full time range.
	if predicate == nil {
		newMin = min
		newMax = max
	}

	batch := make([][]byte, 0, 10000)
	for {
		elem, err := itr.Next()
		if err != nil {
			return err
		} else if elem == nil {
			break
		}

		// See if the series should be deleted and if so, what range of time.
		if predicate != nil {
			var shouldDelete bool
			newMin, newMax, shouldDelete = predicate(elem.Name(), elem.Tags())
			if !shouldDelete {
				continue
			}

			// If the min/max happens to change for the batch, we need to flush
			// the current batch and start a new one.
			flushBatch = (min != newMin || max != newMax) && len(batch) > 0
		}

		if elem.Expr() != nil {
			if v, ok := elem.Expr().(*influxql.BooleanLiteral); !ok || !v.Val {
				return errors.New("fields not supported in WHERE clause during deletion")
			}
		}

		if !disableOnce {
			// Disable and abort running compactions so that tombstones added existing tsm
			// files don't get removed.  This would cause deleted measurements/series to
			// re-appear once the compaction completed.  We only disable the level compactions
			// so that snapshotting does not stop while writing out tombstones.  If it is stopped,
			// and writing tombstones takes a long time, writes can get rejected due to the cache
			// filling up.
			e.disableLevelCompactions(true)
			defer e.enableLevelCompactions(true)

			e.sfile.DisableCompactions()
			defer e.sfile.EnableCompactions()
			e.sfile.Wait()

			disableOnce = true
		}

		if sz >= deleteFlushThreshold || flushBatch {
			// Delete all matching batch.
			if err := e.deleteSeriesRange(batch, min, max); err != nil {
				return err
			}
			batch = batch[:0]
			sz = 0
			flushBatch = false
		}

		// Use the new min/max time for the next iteration
		min = newMin
		max = newMax

		key := models.MakeKey(elem.Name(), elem.Tags())
		sz += len(key)
		batch = append(batch, key)
	}

	if len(batch) > 0 {
		// Delete all matching batch.
		if err := e.deleteSeriesRange(batch, min, max); err != nil {
			return err
		}
	}

	e.index.Rebuild()
	return nil
}

// deleteSeriesRange removes the values between min and max (inclusive) from all series.  This
// does not update the index or disable compactions.  This should mainly be called by DeleteSeriesRange
// and not directly.
func (e *Engine) deleteSeriesRange(seriesKeys [][]byte, min, max int64) error {
	if len(seriesKeys) == 0 {
		return nil
	}

	// Ensure keys are sorted since lower layers require them to be.
	if !bytesutil.IsSorted(seriesKeys) {
		bytesutil.Sort(seriesKeys)
	}

	// Min and max time in the engine are slightly different from the query language values.
	if min == influxql.MinTime {
		min = math.MinInt64
	}
	if max == influxql.MaxTime {
		max = math.MaxInt64
	}

	// Run the delete on each TSM file in parallel
	if err := e.FileStore.Apply(func(r TSMFile) error {
		// See if this TSM file contains the keys and time range
		minKey, maxKey := seriesKeys[0], seriesKeys[len(seriesKeys)-1]
		tsmMin, tsmMax := r.KeyRange()

		tsmMin, _ = SeriesAndFieldFromCompositeKey(tsmMin)
		tsmMax, _ = SeriesAndFieldFromCompositeKey(tsmMax)

		overlaps := bytes.Compare(tsmMin, maxKey) <= 0 && bytes.Compare(tsmMax, minKey) >= 0
		if !overlaps || !r.OverlapsTimeRange(min, max) {
			return nil
		}

		// Delete each key we find in the file.  We seek to the min key and walk from there.
		batch := r.BatchDelete()
		iter := r.Iterator(minKey)
		var j int
		for iter.Next() {
			indexKey := iter.Key()
			seriesKey, _ := SeriesAndFieldFromCompositeKey(indexKey)

			for j < len(seriesKeys) && bytes.Compare(seriesKeys[j], seriesKey) < 0 {
				j++
			}

			if j >= len(seriesKeys) {
				break
			}
			if bytes.Equal(seriesKeys[j], seriesKey) {
				if err := batch.DeleteRange([][]byte{indexKey}, min, max); err != nil {
					batch.Rollback()
					return err
				}
			}
		}
		if err := iter.Err(); err != nil {
			batch.Rollback()
			return err
		}

		return batch.Commit()
	}); err != nil {
		return err
	}

	// find the keys in the cache and remove them
	deleteKeys := make([][]byte, 0, len(seriesKeys))

	// ApplySerialEntryFn cannot return an error in this invocation.
	_ = e.Cache.ApplyEntryFn(func(k []byte, _ *entry) error {
		seriesKey, _ := SeriesAndFieldFromCompositeKey([]byte(k))

		// Cache does not walk keys in sorted order, so search the sorted
		// series we need to delete to see if any of the cache keys match.
		i := bytesutil.SearchBytes(seriesKeys, seriesKey)
		if i < len(seriesKeys) && bytes.Equal(seriesKey, seriesKeys[i]) {
			// k is the measurement + tags + sep + field
			deleteKeys = append(deleteKeys, k)
		}
		return nil
	})

	// Sort the series keys because ApplyEntryFn iterates over the keys randomly.
	bytesutil.Sort(deleteKeys)

	e.Cache.DeleteRange(deleteKeys, min, max)

	// delete from the WAL
	if _, err := e.WAL.DeleteRange(deleteKeys, min, max); err != nil {
		return err
	}

	// The series are deleted on disk, but the index may still say they exist.
	// Depending on the the min,max time passed in, the series may or not actually
	// exists now.  To reconcile the index, we walk the series keys that still exists
	// on disk and cross out any keys that match the passed in series.  Any series
	// left in the slice at the end do not exist and can be deleted from the index.
	// Note: this is inherently racy if writes are occurring to the same measurement/series are
	// being removed.  A write could occur and exist in the cache at this point, but we
	// would delete it from the index.
	minKey := seriesKeys[0]

	// Apply runs this func concurrently.  The seriesKeys slice is mutated concurrently
	// by different goroutines setting positions to nil.
	if err := e.FileStore.Apply(func(r TSMFile) error {
		var j int

		// Start from the min deleted key that exists in this file.
		iter := r.Iterator(minKey)
		for iter.Next() {
			if j >= len(seriesKeys) {
				return nil
			}

			indexKey := iter.Key()
			seriesKey, _ := SeriesAndFieldFromCompositeKey(indexKey)

			// Skip over any deleted keys that are less than our tsm key
			cmp := bytes.Compare(seriesKeys[j], seriesKey)
			for j < len(seriesKeys) && cmp < 0 {
				j++
				if j >= len(seriesKeys) {
					return nil
				}
				cmp = bytes.Compare(seriesKeys[j], seriesKey)
			}

			// We've found a matching key, cross it out so we do not remove it from the index.
			if j < len(seriesKeys) && cmp == 0 {
				seriesKeys[j] = emptyBytes
				j++
			}
		}

		return iter.Err()
	}); err != nil {
		return err
	}

	// Have we deleted all values for the series? If so, we need to remove
	// the series from the index.
	if len(seriesKeys) > 0 {
		buf := make([]byte, 1024) // For use when accessing series file.
		ids := tsdb.NewSeriesIDSet()
		measurements := make(map[string]struct{}, 1)

		for _, k := range seriesKeys {
			if len(k) == 0 {
				continue // This key was wiped because it shouldn't be removed from index.
			}

			name, tags := models.ParseKeyBytes(k)
			sid := e.sfile.SeriesID(name, tags, buf)
			if sid.IsZero() {
				continue
			}

			// See if this series was found in the cache earlier
			i := bytesutil.SearchBytes(deleteKeys, k)

			var hasCacheValues bool
			// If there are multiple fields, they will have the same prefix.  If any field
			// has values, then we can't delete it from the index.
			for i < len(deleteKeys) && bytes.HasPrefix(deleteKeys[i], k) {
				if e.Cache.Values(deleteKeys[i]).Len() > 0 {
					hasCacheValues = true
					break
				}
				i++
			}

			if hasCacheValues {
				continue
			}

			measurements[string(name)] = struct{}{}
			// Remove the series from the local index.
			if err := e.index.DropSeries(sid, k, false); err != nil {
				return err
			}

			// Add the id to the set of delete ids.
			ids.Add(sid)
		}

		for k := range measurements {
			if err := e.index.DropMeasurementIfSeriesNotExist([]byte(k)); err != nil {
				return err
			}
		}

		// Remove the remaining ids from the series file as they no longer exist
		// in any shard.
		var err error
		ids.ForEach(func(id tsdb.SeriesID) {
			if err1 := e.sfile.DeleteSeriesID(id); err1 != nil {
				err = err1
			}
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// DeleteMeasurement deletes a measurement and all related series.
func (e *Engine) DeleteMeasurement(name []byte) error {
	// Delete the bulk of data outside of the fields lock.
	if err := e.deleteMeasurement(name); err != nil {
		return err
	}
	return nil
}

// DeleteMeasurement deletes a measurement and all related series.
func (e *Engine) deleteMeasurement(name []byte) error {
	// Attempt to find the series keys.
	itr, err := e.index.MeasurementSeriesIDIterator(name)
	if err != nil {
		return err
	} else if itr == nil {
		return nil
	}
	defer itr.Close()
	return e.DeleteSeriesRange(tsdb.NewSeriesIteratorAdapter(e.sfile, itr), math.MinInt64, math.MaxInt64)
}

// ForEachMeasurementName iterates over each measurement name in the engine.
func (e *Engine) ForEachMeasurementName(fn func(name []byte) error) error {
	return e.index.ForEachMeasurementName(fn)
}

func (e *Engine) CreateSeriesListIfNotExists(collection *tsdb.SeriesCollection) error {
	return e.index.CreateSeriesListIfNotExists(collection)
}

// WriteTo is not implemented.
func (e *Engine) WriteTo(w io.Writer) (n int64, err error) { panic("not implemented") }

// compactionLevel describes a snapshot or levelled compaction.
type compactionLevel int

func (l compactionLevel) String() string {
	switch l {
	case 0:
		return "snapshot"
	case 1, 2, 3:
		return fmt.Sprint(int(l))
	case 4:
		return "optimize"
	case 5:
		return "full"
	default:
		panic("unsupported compaction level")
	}
}

// compactionTracker tracks compactions and snapshots within the Engine.
//
// As well as being responsible for providing atomic reads and writes to the
// statistics tracking the various compaction operations, compactionTracker also
// mirrors any writes to the prometheus block metrics, which the Engine exposes.
//
// *NOTE* - compactionTracker fields should not be directory modified. Doing so
// could result in the Engine exposing inaccurate metrics.
type compactionTracker struct {
	metrics *compactionMetrics
	labels  prometheus.Labels
	// Note: Compactions are levelled as follows:
	// 0   	– Snapshots
	// 1-3 	– Levelled compactions
	// 4 	– Optimize compactions
	// 5	– Full compactions

	ok     [6]uint64 // Counter of TSM compactions (by level) that have successfully completed.
	active [6]uint64 // Gauge of TSM compactions (by level) currently running.
	errors [6]uint64 // Counter of TSM compcations (by level) that have failed due to error.
	queue  [6]uint64 // Gauge of TSM compactions queues (by level).
}

func newCompactionTracker(metrics *compactionMetrics, defaultLables prometheus.Labels) *compactionTracker {
	return &compactionTracker{metrics: metrics, labels: defaultLables}
}

// Labels returns a copy of the default labels used by the tracker's metrics.
// The returned map is safe for modification.
func (t *compactionTracker) Labels(level compactionLevel) prometheus.Labels {
	labels := make(prometheus.Labels, len(t.labels))
	for k, v := range t.labels {
		labels[k] = v
	}

	// All metrics have a level label.
	labels["level"] = fmt.Sprint(level)
	return labels
}

// Completed returns the total number of compactions for the provided level.
func (t *compactionTracker) Completed(level int) uint64 { return atomic.LoadUint64(&t.ok[level]) }

// Active returns the number of active snapshots (level 0),
// level 1, 2 or 3 compactions, optimize compactions (level 4), or full
// compactions (level 5).
func (t *compactionTracker) Active(level int) uint64 {
	return atomic.LoadUint64(&t.active[level])
}

// AllActive returns the number of active snapshots and compactions.
func (t *compactionTracker) AllActive() uint64 {
	var total uint64
	for i := 0; i < len(t.active); i++ {
		total += atomic.LoadUint64(&t.active[i])
	}
	return total
}

// ActiveOptimise returns the number of active Optimise compactions.
//
// ActiveOptimise is a helper for Active(4).
func (t *compactionTracker) ActiveOptimise() uint64 { return t.Active(4) }

// ActiveFull returns the number of active Full compactions.
//
// ActiveFull is a helper for Active(5).
func (t *compactionTracker) ActiveFull() uint64 { return t.Active(5) }

// Errors returns the total number of errors encountered attempting compactions
// for the provided level.
func (t *compactionTracker) Errors(level int) uint64 { return atomic.LoadUint64(&t.errors[level]) }

// IncActive increments the number of active compactions for the provided level.
func (t *compactionTracker) IncActive(level compactionLevel) {
	atomic.AddUint64(&t.active[level], 1)

	labels := t.Labels(level)
	t.metrics.CompactionsActive.With(labels).Inc()
}

// IncFullActive increments the number of active Full compactions.
func (t *compactionTracker) IncFullActive() { t.IncActive(5) }

// DecActive decrements the number of active compactions for the provided level.
func (t *compactionTracker) DecActive(level compactionLevel) {
	atomic.AddUint64(&t.active[level], ^uint64(0))

	labels := t.Labels(level)
	t.metrics.CompactionsActive.With(labels).Dec()
}

// DecFullActive decrements the number of active Full compactions.
func (t *compactionTracker) DecFullActive() { t.DecActive(5) }

// Attempted updates the number of compactions attempted for the provided level.
func (t *compactionTracker) Attempted(level compactionLevel, success bool, duration time.Duration) {
	if success {
		atomic.AddUint64(&t.ok[level], 1)

		labels := t.Labels(level)

		t.metrics.CompactionDuration.With(labels).Observe(duration.Seconds())

		labels["status"] = "ok"
		t.metrics.Compactions.With(labels).Inc()

		return
	}

	atomic.AddUint64(&t.errors[level], 1)

	labels := t.Labels(level)
	labels["status"] = "error"
	t.metrics.Compactions.With(labels).Inc()
}

// SnapshotAttempted updates the number of snapshots attempted.
func (t *compactionTracker) SnapshotAttempted(success bool, duration time.Duration) {
	t.Attempted(0, success, duration)
}

// SetQueue sets the compaction queue depth for the provided level.
func (t *compactionTracker) SetQueue(level compactionLevel, length uint64) {
	atomic.StoreUint64(&t.queue[level], length)

	labels := t.Labels(level)
	t.metrics.CompactionQueue.With(labels).Set(float64(length))
}

// SetOptimiseQueue sets the queue depth for Optimisation compactions.
func (t *compactionTracker) SetOptimiseQueue(length uint64) { t.SetQueue(4, length) }

// SetFullQueue sets the queue depth for Full compactions.
func (t *compactionTracker) SetFullQueue(length uint64) { t.SetQueue(5, length) }

// WriteSnapshot will snapshot the cache and write a new TSM file with its contents, releasing the snapshot when done.
func (e *Engine) WriteSnapshot() error {
	// Lock and grab the cache snapshot along with all the closed WAL
	// filenames associated with the snapshot

	started := time.Now()

	log, logEnd := logger.NewOperation(e.logger, "Cache snapshot", "tsm1_cache_snapshot")
	defer func() {
		elapsed := time.Since(started)
		log.Info("Snapshot for path written",
			zap.String("path", e.path),
			zap.Duration("duration", elapsed))
		logEnd()
	}()

	closedFiles, snapshot, err := func() (segments []string, snapshot *Cache, err error) {
		e.mu.Lock()
		defer e.mu.Unlock()

		if err = e.WAL.CloseSegment(); err != nil {
			return nil, nil, err
		}

		segments, err = e.WAL.ClosedSegments()
		if err != nil {
			return nil, nil, err
		}

		snapshot, err = e.Cache.Snapshot()
		return segments, snapshot, err
	}()

	if err != nil {
		return err
	}

	if snapshot.Size() == 0 {
		e.Cache.ClearSnapshot(true)
		return nil
	}

	// The snapshotted cache may have duplicate points and unsorted data.  We need to deduplicate
	// it before writing the snapshot.  This can be very expensive so it's done while we are not
	// holding the engine write lock.
	dedup := time.Now()
	snapshot.Deduplicate()
	e.traceLogger.Info("Snapshot for path deduplicated",
		zap.String("path", e.path),
		zap.Duration("duration", time.Since(dedup)))

	return e.writeSnapshotAndCommit(log, closedFiles, snapshot)
}

// writeSnapshotAndCommit will write the passed cache to a new TSM file and remove the closed WAL segments.
func (e *Engine) writeSnapshotAndCommit(log *zap.Logger, closedFiles []string, snapshot *Cache) (err error) {
	defer func() {
		if err != nil {
			e.Cache.ClearSnapshot(false)
		}
	}()

	// write the new snapshot files
	newFiles, err := e.Compactor.WriteSnapshot(snapshot)
	if err != nil {
		log.Info("Error writing snapshot from compactor", zap.Error(err))
		return err
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	// update the file store with these new files
	if err := e.FileStore.Replace(nil, newFiles); err != nil {
		log.Info("Error adding new TSM files from snapshot", zap.Error(err))
		return err
	}

	// clear the snapshot from the in-memory cache, then the old WAL files
	e.Cache.ClearSnapshot(true)

	if err := e.WAL.Remove(closedFiles); err != nil {
		log.Info("Error removing closed WAL segments", zap.Error(err))
	}

	return nil
}

// compactCache continually checks if the WAL cache should be written to disk.
func (e *Engine) compactCache() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		e.mu.RLock()
		quit := e.snapDone
		e.mu.RUnlock()

		select {
		case <-quit:
			return

		case <-t.C:
			e.Cache.UpdateAge()
			if e.ShouldCompactCache(time.Now()) {
				start := time.Now()
				e.traceLogger.Info("Compacting cache", zap.String("path", e.path))
				err := e.WriteSnapshot()
				if err != nil && err != errCompactionsDisabled {
					e.logger.Info("Error writing snapshot", zap.Error(err))
				}
				e.compactionTracker.SnapshotAttempted(err == nil || err == errCompactionsDisabled, time.Since(start))
			}
		}
	}
}

// ShouldCompactCache returns true if the Cache is over its flush threshold
// or if the passed in lastWriteTime is older than the write cold threshold.
func (e *Engine) ShouldCompactCache(t time.Time) bool {
	sz := e.Cache.Size()

	if sz == 0 {
		return false
	}

	if sz > e.CacheFlushMemorySizeThreshold {
		return true
	}

	return t.Sub(e.Cache.LastWriteTime()) > e.CacheFlushWriteColdDuration
}

func (e *Engine) compact(wg *sync.WaitGroup) {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		e.mu.RLock()
		quit := e.done
		e.mu.RUnlock()

		select {
		case <-quit:
			return

		case <-t.C:

			// Find our compaction plans
			level1Groups := e.CompactionPlan.PlanLevel(1)
			level2Groups := e.CompactionPlan.PlanLevel(2)
			level3Groups := e.CompactionPlan.PlanLevel(3)
			level4Groups := e.CompactionPlan.Plan(e.FileStore.LastModified())
			e.compactionTracker.SetOptimiseQueue(uint64(len(level4Groups)))

			// If no full compactions are need, see if an optimize is needed
			if len(level4Groups) == 0 {
				level4Groups = e.CompactionPlan.PlanOptimize()
				e.compactionTracker.SetOptimiseQueue(uint64(len(level4Groups)))
			}

			// Update the level plan queue stats
			e.compactionTracker.SetQueue(1, uint64(len(level1Groups)))
			e.compactionTracker.SetQueue(2, uint64(len(level2Groups)))
			e.compactionTracker.SetQueue(3, uint64(len(level3Groups)))

			// Set the queue depths on the scheduler
			e.scheduler.setDepth(1, len(level1Groups))
			e.scheduler.setDepth(2, len(level2Groups))
			e.scheduler.setDepth(3, len(level3Groups))
			e.scheduler.setDepth(4, len(level4Groups))

			// Find the next compaction that can run and try to kick it off
			if level, runnable := e.scheduler.next(); runnable {
				switch level {
				case 1:
					if e.compactHiPriorityLevel(level1Groups[0], 1, false, wg) {
						level1Groups = level1Groups[1:]
					}
				case 2:
					if e.compactHiPriorityLevel(level2Groups[0], 2, false, wg) {
						level2Groups = level2Groups[1:]
					}
				case 3:
					if e.compactLoPriorityLevel(level3Groups[0], 3, true, wg) {
						level3Groups = level3Groups[1:]
					}
				case 4:
					if e.compactFull(level4Groups[0], wg) {
						level4Groups = level4Groups[1:]
					}
				}
			}

			// Release all the plans we didn't start.
			e.CompactionPlan.Release(level1Groups)
			e.CompactionPlan.Release(level2Groups)
			e.CompactionPlan.Release(level3Groups)
			e.CompactionPlan.Release(level4Groups)
		}
	}
}

// compactHiPriorityLevel kicks off compactions using the high priority policy. It returns
// true if the compaction was started
func (e *Engine) compactHiPriorityLevel(grp CompactionGroup, level compactionLevel, fast bool, wg *sync.WaitGroup) bool {
	s := e.levelCompactionStrategy(grp, fast, level)
	if s == nil {
		return false
	}

	// Try hi priority limiter, otherwise steal a little from the low priority if we can.
	if e.compactionLimiter.TryTake() {
		e.compactionTracker.IncActive(level)

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer e.compactionTracker.DecActive(level)
			defer e.compactionLimiter.Release()
			s.Apply()
			// Release the files in the compaction plan
			e.CompactionPlan.Release([]CompactionGroup{s.group})
		}()
		return true
	}

	// Return the unused plans
	return false
}

// compactLoPriorityLevel kicks off compactions using the lo priority policy. It returns
// the plans that were not able to be started
func (e *Engine) compactLoPriorityLevel(grp CompactionGroup, level compactionLevel, fast bool, wg *sync.WaitGroup) bool {
	s := e.levelCompactionStrategy(grp, fast, level)
	if s == nil {
		return false
	}

	// Try the lo priority limiter, otherwise steal a little from the high priority if we can.
	if e.compactionLimiter.TryTake() {
		e.compactionTracker.IncActive(level)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer e.compactionTracker.DecActive(level)
			defer e.compactionLimiter.Release()
			s.Apply()
			// Release the files in the compaction plan
			e.CompactionPlan.Release([]CompactionGroup{s.group})
		}()
		return true
	}
	return false
}

// compactFull kicks off full and optimize compactions using the lo priority policy. It returns
// the plans that were not able to be started.
func (e *Engine) compactFull(grp CompactionGroup, wg *sync.WaitGroup) bool {
	s := e.fullCompactionStrategy(grp, false)
	if s == nil {
		return false
	}

	// Try the lo priority limiter, otherwise steal a little from the high priority if we can.
	if e.compactionLimiter.TryTake() {
		e.compactionTracker.IncFullActive()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer e.compactionTracker.DecFullActive()
			defer e.compactionLimiter.Release()
			s.Apply()
			// Release the files in the compaction plan
			e.CompactionPlan.Release([]CompactionGroup{s.group})
		}()
		return true
	}
	return false
}

// compactionStrategy holds the details of what to do in a compaction.
type compactionStrategy struct {
	group CompactionGroup

	fast  bool
	level compactionLevel

	tracker *compactionTracker

	logger    *zap.Logger
	compactor *Compactor
	fileStore *FileStore

	engine *Engine
}

// Apply concurrently compacts all the groups in a compaction strategy.
func (s *compactionStrategy) Apply() {
	s.compactGroup()
}

// compactGroup executes the compaction strategy against a single CompactionGroup.
func (s *compactionStrategy) compactGroup() {
	now := time.Now()
	group := s.group
	log, logEnd := logger.NewOperation(s.logger, "TSM compaction", "tsm1_compact_group")
	defer logEnd()

	log.Info("Beginning compaction", zap.Int("tsm1_files_n", len(group)))
	for i, f := range group {
		log.Info("Compacting file", zap.Int("tsm1_index", i), zap.String("tsm1_file", f))
	}

	var (
		err   error
		files []string
	)

	if s.fast {
		files, err = s.compactor.CompactFast(group)
	} else {
		files, err = s.compactor.CompactFull(group)
	}

	if err != nil {
		_, inProgress := err.(errCompactionInProgress)
		if err == errCompactionsDisabled || inProgress {
			log.Info("Aborted compaction", zap.Error(err))

			if _, ok := err.(errCompactionInProgress); ok {
				time.Sleep(time.Second)
			}
			return
		}

		log.Info("Error compacting TSM files", zap.Error(err))
		s.tracker.Attempted(s.level, false, 0)
		time.Sleep(time.Second)
		return
	}

	if err := s.fileStore.ReplaceWithCallback(group, files, nil); err != nil {
		log.Info("Error replacing new TSM files", zap.Error(err))
		s.tracker.Attempted(s.level, false, 0)
		time.Sleep(time.Second)
		return
	}

	for i, f := range files {
		log.Info("Compacted file", zap.Int("tsm1_index", i), zap.String("tsm1_file", f))
	}
	log.Info("Finished compacting files", zap.Int("tsm1_files_n", len(files)))
	s.tracker.Attempted(s.level, true, time.Since(now))
}

// levelCompactionStrategy returns a compactionStrategy for the given level.
// It returns nil if there are no TSM files to compact.
func (e *Engine) levelCompactionStrategy(group CompactionGroup, fast bool, level compactionLevel) *compactionStrategy {
	return &compactionStrategy{
		group:     group,
		logger:    e.logger.With(zap.Int("tsm1_level", int(level)), zap.String("tsm1_strategy", "level")),
		fileStore: e.FileStore,
		compactor: e.Compactor,
		fast:      fast,
		engine:    e,
		level:     level,
		tracker:   e.compactionTracker,
	}
}

// fullCompactionStrategy returns a compactionStrategy for higher level generations of TSM files.
// It returns nil if there are no TSM files to compact.
func (e *Engine) fullCompactionStrategy(group CompactionGroup, optimize bool) *compactionStrategy {
	s := &compactionStrategy{
		group:     group,
		logger:    e.logger.With(zap.String("tsm1_strategy", "full"), zap.Bool("tsm1_optimize", optimize)),
		fileStore: e.FileStore,
		compactor: e.Compactor,
		fast:      optimize,
		engine:    e,
		level:     5,
		tracker:   e.compactionTracker,
	}

	if optimize {
		s.level = 4
	}
	return s
}

// reloadCache reads the WAL segment files and loads them into the cache.
func (e *Engine) reloadCache() error {
	now := time.Now()
	files, err := segmentFileNames(e.WAL.Path())
	if err != nil {
		return err
	}

	limit := e.Cache.MaxSize()
	defer func() {
		e.Cache.SetMaxSize(limit)
	}()

	// Disable the max size during loading
	e.Cache.SetMaxSize(0)

	loader := NewCacheLoader(files)
	loader.WithLogger(e.logger)
	if err := loader.Load(e.Cache); err != nil {
		return err
	}

	e.traceLogger.Info("Reloaded WAL cache", zap.String("path", e.WAL.Path()), zap.Duration("duration", time.Since(now)))
	return nil
}

// cleanup removes all temp files and dirs that exist on disk.  This is should only be run at startup to avoid
// removing tmp files that are still in use.
func (e *Engine) cleanup() error {
	allfiles, err := ioutil.ReadDir(e.path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}

	ext := fmt.Sprintf(".%s", TmpTSMFileExtension)
	for _, f := range allfiles {
		// Check to see if there are any `.tmp` directories that were left over from failed shard snapshots
		if f.IsDir() && strings.HasSuffix(f.Name(), ext) {
			if err := os.RemoveAll(filepath.Join(e.path, f.Name())); err != nil {
				return fmt.Errorf("error removing tmp snapshot directory %q: %s", f.Name(), err)
			}
		}
	}

	return e.cleanupTempTSMFiles()
}

func (e *Engine) cleanupTempTSMFiles() error {
	files, err := filepath.Glob(filepath.Join(e.path, fmt.Sprintf("*.%s", CompactionTempExtension)))
	if err != nil {
		return fmt.Errorf("error getting compaction temp files: %s", err.Error())
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("error removing temp compaction files: %v", err)
		}
	}
	return nil
}

// KeyCursor returns a KeyCursor for the given key starting at time t.
func (e *Engine) KeyCursor(ctx context.Context, key []byte, t int64, ascending bool) *KeyCursor {
	return e.FileStore.KeyCursor(ctx, key, t, ascending)
}

// IteratorCost produces the cost of an iterator.
func (e *Engine) IteratorCost(measurement string, opt query.IteratorOptions) (query.IteratorCost, error) {
	// Determine if this measurement exists. If it does not, then no shards are
	// accessed to begin with.
	if exists, err := e.index.MeasurementExists([]byte(measurement)); err != nil {
		return query.IteratorCost{}, err
	} else if !exists {
		return query.IteratorCost{}, nil
	}

	tagSets, err := e.index.TagSets([]byte(measurement), opt)
	if err != nil {
		return query.IteratorCost{}, err
	}

	// Attempt to retrieve the ref from the main expression (if it exists).
	var ref *influxql.VarRef
	if opt.Expr != nil {
		if v, ok := opt.Expr.(*influxql.VarRef); ok {
			ref = v
		} else if call, ok := opt.Expr.(*influxql.Call); ok {
			if len(call.Args) > 0 {
				ref, _ = call.Args[0].(*influxql.VarRef)
			}
		}
	}

	// Count the number of series concatenated from the tag set.
	cost := query.IteratorCost{NumShards: 1}
	for _, t := range tagSets {
		cost.NumSeries += int64(len(t.SeriesKeys))
		for i, key := range t.SeriesKeys {
			// Retrieve the cost for the main expression (if it exists).
			if ref != nil {
				c := e.seriesCost(key, ref.Val, opt.StartTime, opt.EndTime)
				cost = cost.Combine(c)
			}

			// Retrieve the cost for every auxiliary field since these are also
			// iterators that we may have to look through.
			// We may want to separate these though as we are unlikely to incur
			// anywhere close to the full costs of the auxiliary iterators because
			// many of the selected values are usually skipped.
			for _, ref := range opt.Aux {
				c := e.seriesCost(key, ref.Val, opt.StartTime, opt.EndTime)
				cost = cost.Combine(c)
			}

			// Retrieve the expression names in the condition (if there is a condition).
			// We will also create cursors for these too.
			if t.Filters[i] != nil {
				refs := influxql.ExprNames(t.Filters[i])
				for _, ref := range refs {
					c := e.seriesCost(key, ref.Val, opt.StartTime, opt.EndTime)
					cost = cost.Combine(c)
				}
			}
		}
	}
	return cost, nil
}

func (e *Engine) seriesCost(seriesKey, field string, tmin, tmax int64) query.IteratorCost {
	key := SeriesFieldKeyBytes(seriesKey, field)
	c := e.FileStore.Cost(key, tmin, tmax)

	// Retrieve the range of values within the cache.
	cacheValues := e.Cache.Values(key)
	c.CachedValues = int64(len(cacheValues.Include(tmin, tmax)))
	return c
}

// SeriesFieldKey combine a series key and field name for a unique string to be hashed to a numeric ID.
func SeriesFieldKey(seriesKey, field string) string {
	return seriesKey + keyFieldSeparator + field
}

func SeriesFieldKeyBytes(seriesKey, field string) []byte {
	b := make([]byte, len(seriesKey)+len(keyFieldSeparator)+len(field))
	i := copy(b[:], seriesKey)
	i += copy(b[i:], keyFieldSeparatorBytes)
	copy(b[i:], field)
	return b
}

var (
	blockToFieldType = [8]influxql.DataType{
		BlockFloat64:  influxql.Float,
		BlockInteger:  influxql.Integer,
		BlockBoolean:  influxql.Boolean,
		BlockString:   influxql.String,
		BlockUnsigned: influxql.Unsigned,
		5:             influxql.Unknown,
		6:             influxql.Unknown,
		7:             influxql.Unknown,
	}
)

func BlockTypeToInfluxQLDataType(typ byte) influxql.DataType { return blockToFieldType[typ&7] }

// SeriesAndFieldFromCompositeKey returns the series key and the field key extracted from the composite key.
func SeriesAndFieldFromCompositeKey(key []byte) ([]byte, []byte) {
	sep := bytes.Index(key, keyFieldSeparatorBytes)
	if sep == -1 {
		// No field???
		return key, nil
	}
	return key[:sep], key[sep+len(keyFieldSeparator):]
}
