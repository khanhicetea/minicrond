package logstore

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"

	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/logdb"
)

const Version byte = 1
const chunkLimit = 1 << 20
const maxFramePayload = 16 << 20

// In batch durability mode (ADR-8 1B) every accepted frame is written through
// to the chunk file before the write returns, so a daemon crash loses nothing.
// The fsync is grouped: one shared timer, armed only while some writer holds
// unsynced bytes, syncs every dirty writer after DefaultSyncInterval, and a
// writer whose dirty bytes reach DefaultSyncMaxDirty syncs at once. Sealing a
// chunk, pipe EOF and an orderly shutdown always sync. Only an OS crash or
// power loss can lose output newer than the last sync; a stalled disk can
// stretch that window beyond the nominal interval.
const (
	DefaultSyncInterval = 2 * time.Second
	DefaultSyncMaxDirty = 1 << 20
)

// Capture recovery after a log write failure (ADR-8 2A) is retried lazily, on
// the next line, with a doubling delay. There are no recovery timers.
var (
	captureRetryMin = time.Second
	captureRetryMax = 30 * time.Second
)

var errWriterClosed = errors.New("log writer closed")

// archiveWorkers bounds concurrent archive transfers, and so the batch memory
// they hold, without making unrelated runs wait for one another.
const archiveWorkers = 2

type Stream byte

const (
	Stdout Stream = 1
	Stderr Stream = 2
	System Stream = 3
)

type Flags byte

const (
	FlagPartial Flags = 1 << iota
	FlagInvalidUTF8
	FlagTruncated
)

type Frame struct {
	Sequence  uint64    `json:"sequence"`
	Timestamp time.Time `json:"timestamp"`
	Stream    Stream    `json:"stream"`
	Flags     Flags     `json:"flags"`
	Payload   []byte    `json:"payload"`
}
type chunkMeta struct {
	Number   int    `json:"number"`
	First    uint64 `json:"first"`
	Last     uint64 `json:"last"`
	Bytes    int64  `json:"bytes"`
	Raw      int64  `json:"raw"`
	Archived bool   `json:"archived,omitempty"`
}
type index struct {
	Job       string      `json:"job,omitempty"`
	Kind      string      `json:"kind,omitempty"`
	Version   int         `json:"version"`
	Chunks    []chunkMeta `json:"chunks"`
	Final     bool        `json:"final"`
	Truncated bool        `json:"truncated"`
	// Dropped* record output discarded while log storage was failing
	// (ADR-8 2A) and SyncFailures the fsync errors seen; they are evidence
	// that capture was incomplete or its durability unconfirmed.
	DroppedFrames int64 `json:"dropped_frames,omitempty"`
	DroppedBytes  int64 `json:"dropped_bytes,omitempty"`
	SyncFailures  int64 `json:"sync_failures,omitempty"`
}

// Store keeps live run logs in compressed chunk files under root. When a
// logdb archive is attached, sealed chunks are also copied into the separate
// SQLite log database: finished runs are sealed and archived in the
// background, long-running workers archive incrementally via FlushActive, and
// leftover buffers from a crash or a failed archival are swept into the
// archive by ArchiveOrphans. Reads merge the database and the buffer files
// (including the active chunk, which is written through on every frame)
// transparently, so callers never need to know where a frame currently lives.
// No payload is retained in memory for viewers.
type Store struct {
	root     string
	db       *logdb.LogDB
	mu       sync.Mutex
	writers  map[string]*Writer
	runLocks map[string]*runLock
	// archiveSlots bounds concurrent archive transfers. Acquire it before any
	// per-run lock; readers and writers never take it.
	archiveSlots chan struct{}
	// owners marks runs whose buffer is being archived or deleted, so two
	// transfers never interleave on one run. Guarded by mu; released is
	// broadcast whenever an owner finishes.
	owners   map[string]bool
	released *sync.Cond // broadcast when an owner finishes or a waiter's context ends
	// deferred records sealed runs the archiver found owned by someone else;
	// they are queued again when that owner finishes. Guarded by mu.
	deferred map[string]bool
	// orphanFailures counts consecutive non-database archive failures per
	// orphaned buffer. Guarded by mu.
	orphanFailures map[string]int
	// frameSync selects per-frame fsync for writers opened afterwards.
	frameSync atomic.Bool
	// Group sync policy. syncMu guards dirty and syncTimer and is a leaf lock.
	syncInterval atomic.Int64 // nanoseconds
	syncMaxDirty atomic.Int64
	syncMu       sync.Mutex
	dirty        map[*Writer]struct{}
	syncTimer    *time.Timer
	// syncFile performs a chunk fsync; tests replace it to count or fail syncs.
	syncFile        func(*os.File) error
	captureFailures atomic.Int64 // writers that entered degraded capture
	archiver        archiver
}

// archiver moves sealed run buffers into the log database in the background,
// keeping SQLite off the run-completion path. Fields are guarded by Store.mu.
type archiver struct {
	running bool
	queue   []string
	queued  map[string]bool
	wake    chan struct{}
	stop    chan struct{}
	// ctx is canceled when shutdown runs out of time; every archive step
	// (database transaction, claim wait, slot wait, sweep) observes it.
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		root:           root,
		writers:        make(map[string]*Writer),
		runLocks:       make(map[string]*runLock),
		archiveSlots:   make(chan struct{}, archiveWorkers),
		owners:         make(map[string]bool),
		deferred:       make(map[string]bool),
		orphanFailures: make(map[string]int),
		dirty:          make(map[*Writer]struct{}),
	}
	s.syncInterval.Store(int64(DefaultSyncInterval))
	s.syncMaxDirty.Store(DefaultSyncMaxDirty)
	s.released = sync.NewCond(&s.mu)
	return s, nil
}

// SetFrameSync selects the strict durability of writers opened afterwards.
// true syncs every accepted frame to disk before it is acknowledged; false
// (the default) groups syncs on a shared timer, see SetGroupSync.
func (s *Store) SetFrameSync(enabled bool) { s.frameSync.Store(enabled) }

// SetGroupSync sets the batch-durability window: dirty writers are synced
// once interval after the first unsynced write, and a writer syncs at once
// when it holds maxDirty unsynced bytes. Non-positive values select the
// defaults. It applies immediately to open writers.
func (s *Store) SetGroupSync(interval time.Duration, maxDirty int64) {
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	if maxDirty <= 0 {
		maxDirty = DefaultSyncMaxDirty
	}
	s.syncInterval.Store(int64(interval))
	s.syncMaxDirty.Store(maxDirty)
}

// CaptureFailures reports how many run writers entered degraded capture
// (output discarded because log storage failed) since the store was created.
func (s *Store) CaptureFailures() int64 { return s.captureFailures.Load() }

// AttachDB enables SQLite archival into the given log database.
func (s *Store) AttachDB(db *logdb.LogDB) { s.db = db }

// WriterOptions configures one run's log buffer.
type WriterOptions struct {
	// MaxBytes caps raw frame bytes in the file buffer, not the archive.
	// Successful archival frees capacity. 0 means unbounded.
	MaxBytes int64
	// MaxLine truncates single lines longer than this many bytes. 0 disables.
	MaxLine int
	// DropNew selects the log_on_full policy: true refuses new frames once
	// MaxBytes is reached; false (drop_old) evicts the oldest sealed chunks
	// instead.
	DropNew bool
}

func (s *Store) Open(runID, job, kind string, opt WriterOptions) (*Writer, error) {
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	dir := filepath.Join(s.root, runID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	w := &Writer{store: s, runID: runID, job: job, kind: kind, dir: dir, maxBytes: opt.MaxBytes, maxLine: opt.MaxLine, dropNew: opt.DropNew, frameSync: s.frameSync.Load()}
	if err := w.rotate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.writers[runID] = w
	s.mu.Unlock()
	return w, nil
}
func (s *Store) Active(runID string) *Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writers[runID]
}

// Close finalizes a run's buffer and, with an attached archive, moves every
// chunk into the SQLite log database before returning. The log stays
// queryable through Read afterwards. Without an archive the files remain on
// disk (pure file backend).
func (s *Store) Close(runID string) error {
	return s.finalize(runID, false)
}

// Seal finalizes a run's buffer like Close, but when the background archiver
// is running it only queues the move into the database. The sealed chunks
// stay readable from the buffer meanwhile, so completing a run never waits on
// the log database or on another run's archival.
func (s *Store) Seal(runID string) error {
	return s.finalize(runID, true)
}

func (s *Store) finalize(runID string, async bool) error {
	w := s.Active(runID)
	if w == nil {
		return nil
	}
	// Closing writes the final index, so the buffer is a complete orphan from
	// here on. Failed finalization leaves a durable buffer for the next
	// orphan sweep.
	err := w.Close()
	lock := s.lockRun(runID, true)
	s.mu.Lock()
	delete(s.writers, runID)
	s.mu.Unlock()
	s.unlockRun(runID, lock, true)
	if err != nil || s.db == nil {
		return err
	}
	if async && s.enqueue(runID) {
		return nil
	}
	s.claimWait(runID)
	defer s.release(runID)
	return s.archiveOwned(context.Background(), runID)
}

// claim takes exclusive archive/delete ownership of a run without waiting.
func (s *Store) claim(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[runID] {
		return false
	}
	s.owners[runID] = true
	return true
}

// claimWait takes ownership of a run, waiting for any current owner.
func (s *Store) claimWait(runID string) {
	_ = s.claimWaitContext(context.Background(), runID)
}

// claimWaitContext is claimWait that gives up when ctx ends. The condition
// variable cannot select on a context, so the context's end wakes waiters.
func (s *Store) claimWaitContext(ctx context.Context, runID string) error {
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.released.Broadcast()
		s.mu.Unlock()
	})
	defer stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.owners[runID] {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.released.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.owners[runID] = true
	return nil
}

func (s *Store) release(runID string) {
	s.mu.Lock()
	delete(s.owners, runID)
	again := s.deferred[runID]
	delete(s.deferred, runID)
	s.released.Broadcast()
	s.mu.Unlock()
	if again {
		s.enqueue(runID)
	}
}

// acquireArchiveSlot waits for an archive slot unless ctx ends first.
func (s *Store) acquireArchiveSlot(ctx context.Context) error {
	select {
	case s.archiveSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// archiveOwned archives an inactive buffer. Callers own the run.
func (s *Store) archiveOwned(ctx context.Context, runID string) error {
	if err := s.acquireArchiveSlot(ctx); err != nil {
		return err
	}
	defer func() { <-s.archiveSlots }()
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	if s.Active(runID) != nil {
		return nil
	}
	return s.archiveOrphan(ctx, runID, filepath.Join(s.root, runID))
}

// StartArchiver starts the background workers that archive sealed runs.
// Without it, Seal archives inline like Close.
func (s *Store) StartArchiver() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archiver.running || s.db == nil {
		return
	}
	a := &s.archiver
	a.running = true
	a.queue, a.queued = nil, make(map[string]bool)
	a.wake, a.stop = make(chan struct{}, 1), make(chan struct{})
	a.ctx, a.cancel = context.WithCancel(context.Background())
	for range archiveWorkers {
		a.workers.Go(s.archiveLoop)
	}
}

// StopArchiver drains queued archival and stops the workers; it also runs the
// final group sync of an orderly shutdown, under the same deadline: fsync
// cannot be interrupted, so on a stalled disk the sync is abandoned (it only
// touches buffer files, never the archive database) and ctx's error is
// returned. If ctx ends first, the archiver's own context is canceled: the
// transaction, claim wait, slot wait or sweep in progress aborts and the
// workers exit, so no archive operation touches the database after
// StopArchiver returns. Remaining buffers are archived by the next orphan
// sweep.
func (s *Store) StopArchiver(ctx context.Context) error {
	var syncErr error
	syncDone := make(chan struct{})
	go func() {
		s.SyncAll()
		close(syncDone)
	}()
	select {
	case <-syncDone:
	case <-ctx.Done():
		syncErr = ctx.Err()
	}
	s.mu.Lock()
	a := &s.archiver
	if !a.running {
		s.mu.Unlock()
		return syncErr
	}
	a.running = false
	close(a.stop)
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		a.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		a.cancel()
		return syncErr
	case <-ctx.Done():
		a.cancel()
		<-done
		return ctx.Err()
	}
}

// ArchivePoolStats reports the archive database's connection waits.
func (s *Store) ArchivePoolStats() (writer, reader sql.DBStats, ok bool) {
	if s.db == nil {
		return writer, reader, false
	}
	writer, reader = s.db.PoolStats()
	return writer, reader, true
}

// ArchiveBacklog reports how many sealed runs are waiting for archival.
func (s *Store) ArchiveBacklog() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.archiver.queue)
}

// enqueue hands a sealed run to the archiver. It reports false when the
// archiver is not running.
func (s *Store) enqueue(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &s.archiver
	if !a.running {
		return false
	}
	if !a.queued[runID] {
		a.queued[runID] = true
		a.queue = append(a.queue, runID)
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return true
}

func (s *Store) archiveLoop() {
	a := &s.archiver
	for {
		s.mu.Lock()
		var runID string
		ctx := a.ctx
		if len(a.queue) > 0 && ctx.Err() == nil {
			runID = a.queue[0]
			a.queue[0] = ""
			a.queue = a.queue[1:]
			delete(a.queued, runID)
		}
		stop := a.stop
		s.mu.Unlock()
		if runID == "" {
			select {
			case <-a.wake:
				continue
			case <-stop:
				// Drain what is left unless shutdown ran out of time.
				s.mu.Lock()
				empty := len(a.queue) == 0
				s.mu.Unlock()
				if empty || ctx.Err() != nil {
					return
				}
				continue
			}
		}
		if err := fault.Call(func() error { return s.archiveSealed(ctx, runID) }); err != nil {
			slog.Error("log archival failed; the orphan sweep will retry", "run", runID, "error", err)
		}
	}
}

// archiveSealed archives one queued run. A run owned by a worker flush or a
// deletion is deferred until that owner releases it.
func (s *Store) archiveSealed(ctx context.Context, runID string) error {
	s.mu.Lock()
	if s.owners[runID] {
		s.deferred[runID] = true
		s.mu.Unlock()
		return nil
	}
	s.owners[runID] = true
	s.mu.Unlock()
	defer s.release(runID)
	err := s.archiveOwned(ctx, runID)
	s.recordOrphanResult(runID, err)
	return err
}

// FlushActive seals and archives the on-disk chunks of every open writer.
// It is the periodic checkpoint for long-running worker logs: the live buffer
// stays bounded while older output remains readable from the archive.
func (s *Store) FlushActive() { _ = s.FlushActiveContext(context.Background()) }

// FlushActiveContext is FlushActive that stops between and within runs when
// ctx ends, returning ctx's error. Per-run flush failures are logged and the
// sweep continues; unfinished chunks stay in their buffers.
func (s *Store) FlushActiveContext(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.writers))
	s.mu.Unlock()
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		w := s.Active(id)
		if w == nil {
			continue
		}
		if err := w.FlushContext(ctx, s); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("log archive flush failed", "run", id, "error", err)
		}
	}
	return nil
}

// ArchiveOrphans sweeps buffers without an active writer into the archive.
// It handles both crash recovery and retries of failed background archival.
// Chunk files that were mid-write are salvaged up to the last intact frame.
// Buffers another goroutine is archiving or deleting are left to it.
func (s *Store) ArchiveOrphans() error { return s.ArchiveOrphansContext(context.Background()) }

// ArchiveOrphansContext is ArchiveOrphans that aborts the sweep, including the
// archive transaction in progress, when ctx ends.
func (s *Store) ArchiveOrphansContext(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		// Hidden entries, including the quarantine, are not run buffers.
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		runID := e.Name()
		if s.Active(runID) != nil || !s.claim(runID) {
			continue
		}
		err := s.archiveOwned(ctx, runID)
		s.recordOrphanResult(runID, err)
		s.release(runID)
		if err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", runID, err))
		}
	}
	return errors.Join(errs...)
}

// orphanQuarantineAfter is how many consecutive sweeps may fail on a buffer's
// own contents before it is moved aside. Archive database failures do not
// count: they are expected to clear, and the buffer stays in place for them.
const orphanQuarantineAfter = 3

// QuarantineDir names the directory under the log root that keeps buffers
// which cannot be archived, preserved for manual repair.
const QuarantineDir = ".quarantine"

// recordOrphanResult moves a buffer that keeps failing into the quarantine,
// so it stops being retried on every sweep. Callers own the run.
func (s *Store) recordOrphanResult(runID string, cause error) {
	s.mu.Lock()
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		// An interrupted sweep says nothing about the buffer's contents.
		s.mu.Unlock()
		return
	}
	if cause == nil || errors.Is(cause, errArchiveDB) || errors.Is(cause, errChunkQuarantined) {
		delete(s.orphanFailures, runID)
		s.mu.Unlock()
		return
	}
	s.orphanFailures[runID]++
	failures := s.orphanFailures[runID]
	if failures >= orphanQuarantineAfter {
		delete(s.orphanFailures, runID)
	}
	s.mu.Unlock()
	if failures < orphanQuarantineAfter {
		return
	}
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	quarantine := filepath.Join(s.root, QuarantineDir)
	target := filepath.Join(quarantine, runID)
	err := os.MkdirAll(quarantine, 0o700)
	if err == nil {
		err = os.Rename(filepath.Join(s.root, runID), target)
	}
	if err != nil {
		slog.Error("quarantining unarchivable log buffer failed", "run", runID, "error", err)
		return
	}
	slog.Error("log buffer could not be archived and was quarantined for repair", "run", runID, "path", target, "attempts", orphanQuarantineAfter, "error", cause)
}

type Writer struct {
	mu         sync.Mutex
	store      *Store
	runID      string
	job        string
	kind       string
	dir        string
	file       *os.File
	enc        *zstd.Encoder
	chunk      int
	chunkRaw   int
	chunkFirst uint64
	seq        uint64
	total      int64 // accepted raw bytes, less output evicted before archival
	buffered   int64 // raw bytes still occupying the file buffer
	maxBytes   int64
	maxLine    int
	dropNew    bool
	frameSync  bool
	unsynced   int // bytes written to the hot chunk since its last fsync
	syncs      atomic.Int64
	truncated  bool
	idx        index
	header     [24]byte
	closed     bool
	closeErr   error
	chunkErr   error // finalization failures leave the stream unusable until recovered
	// chunkIndexOnly marks a chunkErr that is only a failed index install: the
	// chunk itself is sealed and retrying the index can clear the error.
	chunkIndexOnly bool

	// Degraded capture (ADR-8 2A). While log storage fails the pipe pumps keep
	// draining the child's output and discard it; recovery is attempted lazily
	// from the next line, never from a timer.
	degraded      bool
	degradedErr   error
	retryAt       time.Time
	retryDelay    time.Duration
	episodeFrames int64 // discarded since capture last worked
	episodeBytes  int64
	droppedFrames int64 // discarded over the whole run
	droppedBytes  int64
	syncFailures  int64
	syncFailing   atomic.Bool
	// syncMu serializes fsyncs (see Sync); lastSyncErr is the latest fsync's
	// outcome and is guarded by it. Lock order: syncMu, then mu.
	syncMu      sync.Mutex
	lastSyncErr error
}

func (w *Writer) chunkPath(n int) string { return filepath.Join(w.dir, fmt.Sprintf("%06d.zst", n)) }

// Flush seals the current chunk (if it has data) and archives all sealed
// chunks of this writer. It waits for another archival of this run to finish.
func (w *Writer) Flush(s *Store) error { return w.FlushContext(context.Background(), s) }

// FlushContext is Flush that gives up, including mid-transaction, when ctx
// ends. Chunks not yet archived stay in the buffer for the next checkpoint.
func (w *Writer) FlushContext(ctx context.Context, s *Store) error {
	if err := s.claimWaitContext(ctx, w.runID); err != nil {
		return err
	}
	defer s.release(w.runID)
	through, err := w.sealForArchive()
	if err != nil || through == 0 || s.db == nil {
		return err
	}
	return s.archiveWriter(ctx, w, through)
}

func (w *Writer) sealForArchive() (int, error) {
	lock := w.store.lockRun(w.runID, true)
	defer w.store.unlockRun(w.runID, lock, true)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.store.db == nil {
		return 0, nil
	}
	if w.chunkRaw > 0 {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	return w.chunk - 1, nil
}

func (w *Writer) rotate() error {
	if w.chunkErr != nil {
		return w.chunkErr
	}
	// Acquire the replacement before closing the current stream. A full disk,
	// permission error, or conflicting path must leave the current chunk usable.
	next := w.chunk + 1
	path := w.chunkPath(next)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("open log chunk %d: %w", next, err)
	}
	// A nil file with an encoder means the previous chunk was abandoned after
	// a storage failure; it stays on disk, unindexed, for the orphan sweep.
	if w.enc != nil && w.file != nil {
		if err := w.finishChunk(); err != nil {
			w.chunkErr = errors.Join(err, f.Close(), os.Remove(path))
			return w.chunkErr
		}
	}
	enc := w.enc
	if enc == nil {
		// Most chunks are at most 1 MiB. Reserve a larger match window only
		// when the configured line limit permits an oversized single frame.
		window := chunkLimit
		if w.maxLine <= 0 || w.maxLine > 8<<20 {
			window = 8 << 20
		} else {
			for window < w.maxLine {
				window *= 2
			}
		}
		enc, err = zstd.NewWriter(f,
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(window),
			zstd.WithLowerEncoderMem(true),
		)
		if err != nil {
			return errors.Join(fmt.Errorf("create log encoder: %w", err), f.Close(), os.Remove(path))
		}
	} else {
		// Close seals the old stream; Reset starts an independent chunk while
		// retaining the encoder's match window and compression buffers.
		enc.Reset(f)
	}
	w.chunk = next
	w.file, w.enc, w.chunkRaw, w.chunkFirst = f, enc, 0, w.seq+1
	// finishChunk synced everything written to the previous chunk.
	w.unsynced = 0
	return nil
}
func (w *Writer) finishChunk() error {
	if w.file == nil {
		if w.chunkErr == nil && w.enc != nil {
			// The chunk was abandoned after a storage failure: still record the
			// final index so truncation and drop evidence reaches disk.
			if err := w.writeIndex(); err != nil {
				w.chunkErr = fmt.Errorf("index log chunk %d: %w", w.chunk, err)
				w.chunkIndexOnly = true
			}
		}
		return w.chunkErr
	}
	encErr := w.enc.Close()
	syncErr := w.file.Sync()
	info, statErr := w.file.Stat()
	closeErr := w.file.Close()
	w.file = nil
	if err := errors.Join(encErr, syncErr, statErr, closeErr); err != nil {
		w.chunkErr = fmt.Errorf("finalize log chunk %d: %w", w.chunk, err)
		w.chunkIndexOnly = false
		return w.chunkErr
	}
	if w.seq >= w.chunkFirst {
		w.idx.Chunks = append(w.idx.Chunks, chunkMeta{w.chunk, w.chunkFirst, w.seq, info.Size(), int64(w.chunkRaw), false})
	}
	if err := w.writeIndex(); err != nil {
		w.chunkErr = fmt.Errorf("index log chunk %d: %w", w.chunk, err)
		w.chunkIndexOnly = true
	}
	return w.chunkErr
}
func (w *Writer) writeIndex() error {
	w.idx.Job, w.idx.Kind = w.job, w.kind
	w.idx.Version = int(Version)
	w.idx.Truncated = w.truncated
	w.idx.DroppedFrames, w.idx.DroppedBytes, w.idx.SyncFailures = w.droppedFrames, w.droppedBytes, w.syncFailures
	b, err := json.Marshal(w.idx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(w.dir, "index.json.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(w.dir, "index.json")); err != nil {
		return err
	}
	dir, err := os.Open(w.dir)
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Write appends one frame and returns storage errors to the caller; it is
// meant for the daemon's own system lines. In batch durability the frame has
// reached the chunk file (daemon-crash safe) but is fsynced with the next
// group; with strict "frame" durability a successful Write is synced.
func (w *Writer) Write(stream Stream, payload []byte, flags Flags) error {
	lock := w.store.lockRun(w.runID, true)
	w.mu.Lock()
	needSync, err := w.writeLocked(stream, payload, flags)
	w.mu.Unlock()
	w.store.unlockRun(w.runID, lock, true)
	if needSync {
		if syncErr := w.Sync(); syncErr != nil && err == nil && w.frameSync {
			err = syncErr
		}
	}
	return err
}

// capture stores one line of child output for Pipe. Storage failures never
// reach the pump: capture degrades, discards, and recovers (ADR-8 2A). Only a
// closed writer is reported.
func (w *Writer) capture(stream Stream, payload []byte, flags Flags) error {
	lock := w.store.lockRun(w.runID, true)
	w.mu.Lock()
	needSync, err := w.captureLocked(stream, payload, flags)
	w.mu.Unlock()
	w.store.unlockRun(w.runID, lock, true)
	if needSync {
		_ = w.Sync() // a failure is recorded as capture evidence
	}
	return err
}

func (w *Writer) captureLocked(stream Stream, payload []byte, flags Flags) (bool, error) {
	if w.closed {
		return false, errWriterClosed
	}
	if w.degraded {
		now := time.Now()
		if now.Before(w.retryAt) || !w.recoverLocked(now) {
			w.discard(len(payload))
			return false, nil
		}
	}
	needSync, err := w.writeLocked(stream, payload, flags)
	if err != nil {
		w.enterDegraded(err)
		w.discard(len(payload))
		return false, nil
	}
	return needSync, nil
}

// discard counts output that could not be stored. Callers hold w.mu.
func (w *Writer) discard(payload int) {
	size := int64(payload + 24)
	w.droppedFrames++
	w.droppedBytes += size
	w.episodeFrames++
	w.episodeBytes += size
	w.truncated = true
}

// enterDegraded records a storage failure and schedules the next recovery
// attempt. The first failure of an episode is logged once; later discarded
// lines are only counted, so a dead disk cannot cause an error storm.
func (w *Writer) enterDegraded(err error) {
	now := time.Now()
	if !w.degraded {
		w.degraded = true
		w.retryDelay = captureRetryMin
		w.store.captureFailures.Add(1)
		slog.Error("log storage failed; run output is discarded until capture recovers", "run", w.runID, "error", err)
	}
	w.degradedErr = err
	w.truncated = true
	w.retryAt = now.Add(w.retryDelay)
	w.retryDelay = min(2*w.retryDelay, captureRetryMax)
}

// recoverLocked tries to resume capture on a fresh chunk and, on success,
// records how much output was lost in a system line. Callers hold w.mu.
func (w *Writer) recoverLocked(now time.Time) bool {
	if err := w.reopenChunk(); err != nil {
		w.degradedErr = err
		w.retryAt = now.Add(w.retryDelay)
		w.retryDelay = min(2*w.retryDelay, captureRetryMax)
		return false
	}
	summary := fmt.Sprintf("log capture recovered: %d lines (%d bytes) of output were not stored: %v", w.episodeFrames, w.episodeBytes, w.degradedErr)
	if _, err := w.writeLocked(System, []byte(summary), FlagTruncated); err != nil {
		w.enterDegraded(err)
		return false
	}
	slog.Warn("log capture recovered", "run", w.runID, "discarded_lines", w.episodeFrames, "discarded_bytes", w.episodeBytes)
	w.degraded, w.degradedErr = false, nil
	w.episodeFrames, w.episodeBytes = 0, 0
	return true
}

// reopenChunk abandons a chunk whose stream may be damaged and starts a new
// one. The abandoned file stays on disk, unindexed, and is salvaged up to its
// last intact frame by the orphan sweep. Callers hold w.mu.
func (w *Writer) reopenChunk() error {
	if w.chunkErr != nil {
		if w.chunkIndexOnly {
			// The chunk is sealed; only its index is missing. Keep the original
			// error identity while the index still cannot be installed.
			if err := w.writeIndex(); err != nil {
				return w.chunkErr
			}
		}
		w.chunkErr, w.chunkIndexOnly = nil, false
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	return w.rotate()
}

// writeLocked appends one frame. The frame always reaches the chunk file
// before it returns, so a daemon crash cannot lose it. It reports whether the
// writer should fsync now (strict mode, or the dirty-byte limit was reached);
// otherwise the shared timer syncs it. The caller syncs after unlocking.
// Callers hold the run lock and w.mu.
func (w *Writer) writeLocked(stream Stream, payload []byte, flags Flags) (bool, error) {
	if w.closed {
		return false, errWriterClosed
	}
	if w.chunkErr != nil {
		return false, w.chunkErr
	}
	// Pipe truncates before calling; its flag must reach the run-level one too.
	if flags&FlagTruncated != 0 {
		w.truncated = true
	}
	if len(payload) > w.maxLine && w.maxLine > 0 {
		payload = payload[:w.maxLine]
		flags |= FlagTruncated
		w.truncated = true
	}
	frameSize := 24 + len(payload)
	if w.maxBytes > 0 && w.buffered+int64(frameSize) > w.maxBytes {
		if w.dropNew || int64(frameSize) > w.maxBytes {
			w.truncated = true
			return false, nil
		}
		// Make the current segment evictable before dropping old data. This is
		// essential when maxBytes is smaller than the normal chunk threshold.
		if w.chunkRaw > 0 {
			if err := w.rotate(); err != nil {
				return false, err
			}
		}
		w.truncated = true
		for len(w.idx.Chunks) > 0 && w.buffered+int64(frameSize) > w.maxBytes {
			oldest := w.idx.Chunks[0]
			if err := os.Remove(w.chunkPath(oldest.Number)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
			w.total -= oldest.Raw
			w.buffered -= oldest.Raw
			w.idx.Chunks = w.idx.Chunks[1:]
		}
	}
	if w.chunkRaw+frameSize > chunkLimit && w.chunkRaw > 0 {
		if err := w.rotate(); err != nil {
			return false, err
		}
	}
	w.seq++
	f := Frame{w.seq, time.Now().UTC(), stream, flags, payload}
	if err := encodeWithHeader(w.enc, f, w.header[:]); err != nil {
		return false, err
	}
	// Flush compressed bytes to the file on every frame so a daemon crash
	// cannot lose accepted output. Readers see it through the page cache.
	if err := w.enc.Flush(); err != nil {
		return false, err
	}
	before := w.unsynced
	w.unsynced += frameSize
	w.chunkRaw += frameSize
	w.total += int64(frameSize)
	w.buffered += int64(frameSize)
	if w.frameSync {
		return true, nil
	}
	if before == 0 {
		w.store.markDirty(w)
	}
	return int64(w.unsynced) >= w.store.syncMaxDirty.Load(), nil
}

// markDirty registers a writer holding unsynced bytes and arms the shared
// sync timer if it is not already pending. The timer exists only while some
// writer is dirty, so idle writers and an idle daemon cost nothing.
func (s *Store) markDirty(w *Writer) {
	s.syncMu.Lock()
	s.dirty[w] = struct{}{}
	if s.syncTimer == nil {
		s.syncTimer = time.AfterFunc(time.Duration(s.syncInterval.Load()), s.flushDirty)
	}
	s.syncMu.Unlock()
}

// flushDirty syncs every dirty writer once and disarms the timer. Writes that
// arrive meanwhile re-arm it.
func (s *Store) flushDirty() {
	s.syncMu.Lock()
	if s.syncTimer != nil {
		s.syncTimer.Stop()
		s.syncTimer = nil
	}
	batch := slices.Collect(maps.Keys(s.dirty))
	clear(s.dirty)
	s.syncMu.Unlock()
	for _, w := range batch {
		_ = w.Sync() // failures are recorded on the writer
	}
}

// SyncAll fsyncs every writer that has unsynced bytes now. The daemon calls it
// (through StopArchiver) at orderly shutdown.
func (s *Store) SyncAll() { s.flushDirty() }

func (s *Store) fsync(f *os.File) error {
	if s.syncFile != nil {
		return s.syncFile(f)
	}
	return f.Sync()
}

// Sync makes every accepted frame durable. The fsync runs without the
// writer's locks, so readers and the other stream's pump are not held up, but
// concurrent Syncs are serialized: a caller whose bytes were already claimed by
// an fsync still in flight waits for it, and reports its failure, instead of
// seeing nothing unsynced and returning early. A failure is returned and also
// recorded as capture evidence (the run is marked truncated): after a failed
// fsync the kernel may have dropped pages.
func (w *Writer) Sync() error {
	w.syncMu.Lock()
	defer w.syncMu.Unlock()
	w.mu.Lock()
	if w.closed || w.file == nil || w.unsynced == 0 {
		w.mu.Unlock()
		// Nothing new to sync; the previous fsync (which we waited for) may
		// have failed on bytes this caller wrote.
		return w.lastSyncErr
	}
	f := w.file
	w.unsynced = 0
	w.mu.Unlock()
	w.syncs.Add(1)
	err := w.store.fsync(f)
	if errors.Is(err, os.ErrClosed) {
		// A closed file was sealed concurrently, and sealing syncs it.
		return nil
	}
	w.lastSyncErr = err
	if err == nil {
		w.syncFailing.Store(false)
		return nil
	}
	w.mu.Lock()
	w.syncFailures++
	w.truncated = true
	w.mu.Unlock()
	if !w.syncFailing.Swap(true) {
		slog.Error("log fsync failed; recent output may not be durable", "run", w.runID, "error", err)
	}
	return err
}

// Pipe copies r into the log line by line, draining r to EOF even when the
// log cannot be stored (ADR-8 2A): lines that fail to store are discarded and
// accounted for through Capture, Stats and the run's index, and recovery is
// retried lazily. Storage failures are therefore never returned; Pipe returns
// only read errors and a closed writer. Output is synced as a group (see
// SetGroupSync), and once more when r ends.
func (w *Writer) Pipe(stream Stream, r io.Reader) error {
	limit := w.maxLine
	if limit <= 0 || limit > maxFramePayload {
		limit = maxFramePayload
	}
	br := bufio.NewReaderSize(r, min(limit+1, 64<<10))
	line := make([]byte, 0, min(limit, 64<<10))
	truncated := false
	for {
		fragment, err := br.ReadSlice('\n')
		hasNewline := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if hasNewline {
			fragment = fragment[:len(fragment)-1]
		}
		if !truncated {
			remaining := limit - len(line)
			if len(fragment) > remaining {
				line = append(line, fragment[:max(remaining, 0)]...)
				truncated = true
			} else {
				line = append(line, fragment...)
			}
		}
		if hasNewline || (errors.Is(err, io.EOF) && len(line) > 0) {
			flags := Flags(0)
			if !hasNewline {
				flags |= FlagPartial
			}
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if truncated {
				flags |= FlagTruncated
			}
			if !utf8.Valid(line) {
				flags |= FlagInvalidUTF8
			}
			if writeErr := w.capture(stream, line, flags); writeErr != nil {
				return writeErr
			}
			line = line[:0]
			truncated = false
		}
		if errors.Is(err, io.EOF) {
			_ = w.Sync()
			return nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			_ = w.Sync()
			return err
		}
	}
}

// Sequence returns the sequence of the last accepted frame (0 if none). It is
// a cheap hint for pollers: nothing newer exists while it is unchanged.
func (w *Writer) Sequence() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

// CaptureStatus describes how completely a writer captured its output.
type CaptureStatus struct {
	// Degraded is true while log storage is failing and output is discarded.
	Degraded bool
	// DroppedFrames and DroppedBytes count output discarded over the run.
	DroppedFrames, DroppedBytes int64
	// SyncFailures counts failed fsyncs, after which recent output may not be
	// durable.
	SyncFailures int64
	// Err is the latest storage error while Degraded.
	Err error
}

// Capture reports the writer's capture health. Any dropped output or failed
// sync also sets the truncated flag returned by Stats.
func (w *Writer) Capture() CaptureStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return CaptureStatus{Degraded: w.degraded, DroppedFrames: w.droppedFrames, DroppedBytes: w.droppedBytes, SyncFailures: w.syncFailures, Err: w.degradedErr}
}

func (w *Writer) Close() error {
	lock := w.store.lockRun(w.runID, true)
	defer w.store.unlockRun(w.runID, lock, true)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	if w.degraded {
		// One last attempt, so the loss summary reaches the log if storage
		// came back; otherwise the index still records the dropped counts.
		w.recoverLocked(time.Now())
		if w.degraded {
			slog.Error("run finished with log output missing", "run", w.runID, "discarded_lines", w.droppedFrames, "discarded_bytes", w.droppedBytes, "error", w.degradedErr)
		}
	}
	w.closed = true
	w.idx.Final = true
	w.closeErr = w.finishChunk()
	return w.closeErr
}

func (w *Writer) Stats() (int64, bool) { w.mu.Lock(); defer w.mu.Unlock(); return w.total, w.truncated }
func encode(dst io.Writer, f Frame) error {
	var h [24]byte
	return encodeWithHeader(dst, f, h[:])
}

func encodeWithHeader(dst io.Writer, f Frame, h []byte) error {
	h[0] = Version
	h[1] = byte(f.Stream)
	h[2] = byte(f.Flags)
	h[3] = 0
	binary.BigEndian.PutUint64(h[4:12], f.Sequence)
	binary.BigEndian.PutUint64(h[12:20], uint64(f.Timestamp.UnixMicro()))
	binary.BigEndian.PutUint32(h[20:24], uint32(len(f.Payload)))
	if _, err := dst.Write(h); err != nil {
		return err
	}
	_, err := dst.Write(f.Payload)
	return err
}
func decode(src io.Reader) (Frame, error) {
	f, n, err := decodeHeader(src)
	if err != nil {
		return f, err
	}
	f.Payload = make([]byte, n)
	_, err = io.ReadFull(src, f.Payload)
	if errors.Is(err, io.EOF) {
		// The header was intact but its payload is missing: a torn frame, not
		// a clean end of stream.
		err = io.ErrUnexpectedEOF
	}
	return f, err
}

func decodeHeader(src io.Reader) (Frame, int, error) {
	var h [24]byte
	return decodeHeaderWithBuffer(src, h[:])
}

func decodeHeaderWithBuffer(src io.Reader, h []byte) (Frame, int, error) {
	var f Frame
	if _, err := io.ReadFull(src, h); err != nil {
		return f, 0, err
	}
	if h[0] != Version {
		return f, 0, fmt.Errorf("unsupported log frame version %d", h[0])
	}
	f.Stream = Stream(h[1])
	f.Flags = Flags(h[2])
	f.Sequence = binary.BigEndian.Uint64(h[4:12])
	f.Timestamp = time.UnixMicro(int64(binary.BigEndian.Uint64(h[12:20])))
	n := binary.BigEndian.Uint32(h[20:24])
	if n > maxFramePayload {
		return f, 0, fmt.Errorf("log frame payload %d exceeds maximum %d", n, maxFramePayload)
	}
	return f, int(n), nil
}

// salvageFrames decodes frames from a possibly torn chunk stream, stopping at
// the first problem and returning everything recovered before it. The error
// is nil only when the stream ended cleanly at a frame boundary (a legitimate
// empty chunk included). A stream cut short mid-frame, as after a crash, is
// reported as an error wrapping io.ErrUnexpectedEOF; anything else (bad
// magic, checksum failure, unsupported frame version, oversized payload) is
// corruption. Callers must not treat a nonempty blob that yields an error
// and no frames as empty.
func salvageFrames(blob []byte) ([]Frame, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	dec, err := zstd.NewReader(bytes.NewReader(blob), zstd.WithDecoderMaxMemory(32<<20), zstd.WithDecoderMaxWindow(16<<20))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	var frames []Frame
	for {
		f, err := decode(dec)
		if errors.Is(err, io.EOF) {
			return frames, nil
		}
		if err != nil {
			return frames, err
		}
		frames = append(frames, f)
	}
}

func encodeFrames(frames []Frame) ([]byte, error) {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return nil, err
	}
	for _, f := range frames {
		if err := encode(enc, f); err != nil {
			enc.Close()
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Read returns up to limit frames with a sequence greater than after, merged
// across the storage tiers: archived chunks in the log database and chunk
// files in the buffer directory (sealed, and the active writer's written-through
// chunk).
func (s *Store) Read(runID string, after uint64, limit int) ([]Frame, error) {
	return s.ReadContext(context.Background(), runID, after, limit)
}

func (s *Store) ReadContext(ctx context.Context, runID string, after uint64, limit int) ([]Frame, error) {
	return s.readContext(ctx, runID, after, limit, 16<<20, nil)
}

// ReadStreamContext uses a 1 MiB page budget for streaming consumers.
// Like ReadContext, one oversized valid frame is returned to advance the cursor.
func (s *Store) ReadStreamContext(ctx context.Context, runID string, after uint64, limit int) ([]Frame, error) {
	return s.readContext(ctx, runID, after, limit, 1<<20, nil)
}

// StreamReader reuses a decoder across backlog pages of one stream. It is
// owned by one caller and must be closed when that stream ends.
type StreamReader struct {
	store     *Store
	runID     string
	pageBytes int
	decoder   *zstd.Decoder
}

func (s *Store) NewStreamReader(runID string) *StreamReader {
	return s.newStreamReader(runID, 1<<20)
}

func (s *Store) newStreamReader(runID string, pageBytes int) *StreamReader {
	return &StreamReader{store: s, runID: runID, pageBytes: pageBytes}
}

func (r *StreamReader) ReadContext(ctx context.Context, after uint64, limit int) ([]Frame, error) {
	return r.store.readContext(ctx, r.runID, after, limit, r.pageBytes, &r.decoder)
}

func (r *StreamReader) Close() {
	if r.decoder != nil {
		r.decoder.Close()
		r.decoder = nil
	}
}

func (s *Store) readContext(ctx context.Context, runID string, after uint64, limit, maxResponseBytes int, decoder **zstd.Decoder) ([]Frame, error) {
	if decoder == nil {
		var owned *zstd.Decoder
		decoder = &owned
		defer func() {
			if owned != nil {
				owned.Close()
			}
		}()
	}
	// Pin the run's tier layout for the entire page. Migration, retention,
	// finalization and writes cannot move the cursor past unseen frames.
	lock := s.lockRun(runID, false)
	defer s.unlockRun(runID, lock, false)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit = min(max(limit, 1), 5000)
	out := make([]Frame, 0, min(limit, 100))
	last, responseBytes := after, 0
	appendFrame := func(frame Frame) bool {
		if frame.Sequence <= last {
			return true
		}
		// Allow one maximum-size frame (including its header) so every
		// valid frame can make progress. Never skip it for a smaller one.
		if len(out) > 0 && responseBytes+len(frame.Payload)+24 > maxResponseBytes {
			return false
		}
		out = append(out, frame)
		responseBytes += len(frame.Payload) + 24
		last = frame.Sequence
		return len(out) < limit && responseBytes < maxResponseBytes
	}
	// io.ReadFull passes its buffer through an interface. Reusing one header
	// per page avoids a heap allocation for every decoded or skipped frame.
	var header [24]byte
	var skipped io.LimitedReader
	consume := func(src io.Reader) (bool, error) {
		if *decoder == nil {
			var err error
			*decoder, err = zstd.NewReader(src, zstd.WithDecoderConcurrency(2), zstd.WithDecoderMaxMemory(32<<20), zstd.WithDecoderMaxWindow(16<<20))
			if err != nil {
				return false, err
			}
		} else if err := (*decoder).Reset(src); err != nil {
			return false, err
		}
		for {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			frame, payloadSize, err := decodeHeaderWithBuffer(*decoder, header[:])
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if frame.Sequence <= last {
				// Cursor paging can skip many frames in its first chunk. Reuse
				// CopyN's limiting reader instead of allocating one per skip.
				skipped.R, skipped.N = *decoder, int64(payloadSize)
				_, err = io.Copy(io.Discard, &skipped)
				if skipped.N == 0 {
					err = nil
				} else if err == nil {
					err = io.EOF
				}
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return true, nil
				}
				if err != nil {
					return false, err
				}
				continue
			}
			// Stop after the header, before allocating a payload that cannot
			// fit in this page. The next call restarts at the same sequence.
			if len(out) > 0 && responseBytes+payloadSize+24 > maxResponseBytes {
				return false, nil
			}
			frame.Payload = make([]byte, payloadSize)
			_, err = io.ReadFull(*decoder, frame.Payload)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if !appendFrame(frame) {
				return false, nil
			}
		}
	}
	if s.db != nil {
		more := true
		err := s.db.EachChunk(ctx, runID, after, func(c logdb.Chunk) (bool, error) {
			var err error
			more, err = consume(bytes.NewReader(c.Blob))
			return more, err
		})
		if err != nil {
			return nil, err
		}
		if !more {
			return out, nil
		}
	}
	entries, err := chunkFiles(filepath.Join(s.root, runID))
	if err != nil {
		return nil, err
	}
	// The index lists only sealed chunks. Missing, stale, or invalid entries
	// still take the normal decode path so unsealed and crash-salvaged files
	// remain visible.
	var sealedLast map[int]uint64
	if after > 0 && len(entries) > 1 {
		if data, err := os.ReadFile(filepath.Join(s.root, runID, "index.json")); err == nil {
			var idx index
			if json.Unmarshal(data, &idx) == nil && idx.Version == int(Version) {
				sealedLast = make(map[int]uint64, len(idx.Chunks))
				for _, chunk := range idx.Chunks {
					if chunk.Number > 0 && chunk.First > 0 && chunk.Last >= chunk.First {
						sealedLast[chunk.Number] = chunk.Last
					}
				}
			}
		}
	}
	for _, entry := range entries {
		if lastSequence, ok := sealedLast[entry.number]; ok && lastSequence <= after {
			continue
		}
		f, err := os.Open(entry.path)
		if err != nil {
			return nil, err
		}
		more, err := consume(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		if !more {
			return out, nil
		}
	}
	// The active chunk is among the buffer files: every accepted frame is
	// written through to it, so there is no separate in-memory tail to merge.
	return out, nil
}
func (s *Store) Raw(runID string, w io.Writer) error {
	return s.RawContext(context.Background(), runID, w)
}

func (s *Store) RawContext(ctx context.Context, runID string, w io.Writer) error {
	reader := s.newStreamReader(runID, 4<<20)
	defer reader.Close()
	// Reuse the separator instead of allocating a byte slice for each frame.
	newline := []byte{'\n'}
	var after uint64
	for {
		frames, err := reader.ReadContext(ctx, after, 5000)
		if err != nil {
			return err
		}
		for _, f := range frames {
			if f.Stream == Stderr {
				if _, err := io.WriteString(w, "[err] "); err != nil {
					return err
				}
			}
			if f.Stream == System {
				if _, err := io.WriteString(w, "[minicron] "); err != nil {
					return err
				}
			}
			if _, err := w.Write(f.Payload); err != nil {
				return err
			}
			if _, err := w.Write(newline); err != nil {
				return err
			}
			after = f.Sequence
		}
		if len(frames) == 0 {
			return nil
		}
	}
}

// Delete removes a run's logs from both the archive and the buffer directory.
func (s *Store) Delete(runID string) error { return s.DeleteContext(context.Background(), runID) }

// DeleteContext is Delete that gives up, including mid-transaction, when ctx
// ends.
func (s *Store) DeleteContext(ctx context.Context, runID string) error {
	if err := s.claimWaitContext(ctx, runID); err != nil {
		return err
	}
	defer s.release(runID)
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	if s.Active(runID) != nil {
		return errors.New("cannot delete logs of an active writer")
	}
	if s.db != nil {
		if err := s.db.DeleteRun(ctx, runID); err != nil {
			return err
		}
	}
	return os.RemoveAll(filepath.Join(s.root, runID))
}

// DeleteRuns removes a retention page of inactive runs. An archive batch
// failure falls back to individual deletion, allowing unaffected runs to
// progress. The returned IDs have had both archive and file buffers removed.
// Runs being archived are skipped and reported; the next sweep retries them.
func (s *Store) DeleteRuns(runIDs []string) ([]string, error) {
	return s.DeleteRunsContext(context.Background(), runIDs)
}

// DeleteRunsContext is DeleteRuns that stops, including mid-transaction, when
// ctx ends: runs not yet deleted are left for the next retention sweep and
// ctx's error is joined into the result.
func (s *Store) DeleteRunsContext(ctx context.Context, runIDs []string) ([]string, error) {
	seen := make(map[string]bool, len(runIDs))
	valid := make([]string, 0, len(runIDs))
	type heldRunLock struct {
		id   string
		lock *runLock
	}
	var locks []heldRunLock
	var errs []error
	for _, id := range runIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if !s.claim(id) {
			errs = append(errs, fmt.Errorf("run %s: logs are being archived", id))
			continue
		}
		lock := s.lockRun(id, true)
		if s.Active(id) != nil {
			s.unlockRun(id, lock, true)
			s.release(id)
			errs = append(errs, fmt.Errorf("run %s: cannot delete logs of an active writer", id))
			continue
		}
		locks = append(locks, heldRunLock{id: id, lock: lock})
		valid = append(valid, id)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			s.unlockRun(locks[i].id, locks[i].lock, true)
			s.release(locks[i].id)
		}
	}()
	batchFailed := false
	if s.db != nil && len(valid) > 0 {
		if err := s.db.DeleteRuns(ctx, valid); err != nil {
			batchFailed = true
			errs = append(errs, fmt.Errorf("archive batch deletion: %w", err))
		}
	}
	deleted := make([]string, 0, len(valid))
	for _, id := range valid {
		if batchFailed {
			// Once the batch committed, finish the cheap file removal even if ctx
			// ended: leaving files behind would let the orphan sweep re-archive
			// logs whose database rows are gone.
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				break
			}
			if err := s.db.DeleteRun(ctx, id); err != nil {
				errs = append(errs, fmt.Errorf("run %s: %w", id, err))
				continue
			}
		}
		if err := os.RemoveAll(filepath.Join(s.root, id)); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", id, err))
			continue
		}
		deleted = append(deleted, id)
	}
	return deleted, errors.Join(errs...)
}
