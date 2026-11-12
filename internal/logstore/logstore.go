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

// encoderWindow is the zstd match window of each open run's encoder. The
// encoder stays allocated for the whole run, so it is the main per-run heap
// cost (5A: 10 workers). Measured 2026-10-24 on journal text/JSON and syslog
// corpora: 128 KiB costs 0.58 MiB per encoder against 1.46 MiB for 1 MiB (the
// previous minimum, raised to 8 MiB for large max_line), for 1-2% lower
// compression. A frame larger than the window still encodes; matches just
// cannot reach further back.
const encoderWindow = 128 << 10

// Adaptive capture. A run's output starts in a plain chunk: a standard zstd
// frame made of raw (stored) blocks, written without an encoder. Most runs
// print less than plainLimit, and an encoder costs about 630 KB of allocation
// and 110 µs to create, while a one-line run does not shrink when compressed.
// A writer whose output reaches plainLimit is promoted: it seals the plain
// chunk and continues in compressed chunks until the next worker checkpoint,
// which releases the encoder again. Plain chunks are compressed once, when
// archived, by the store's shared archive encoder. Every chunk is valid zstd,
// so readers and crash salvage treat both kinds alike.
var plainLimit = 16 << 10 // a variable only so tests can change it

// plainFrameHeader opens a plain chunk: zstd magic, a frame header descriptor
// without content size, checksum or dictionary, and a 128 KiB window, which
// bounds raw blocks (each at most plainLimit plus one frame) well below the
// 128 KiB block limit.
var plainFrameHeader = []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 7 << 3}

// maxSealedImages bounds the plain chunk images handed from sealed writers to
// the archiver so it can skip reading them back from disk.
const maxSealedImages = 8 << 20
const maxFramePayload = 16 << 20

// In batch durability mode (ADR-8 1B) every accepted frame is written through
// to the chunk file before the write returns, so a daemon crash loses nothing.
// The fsync is grouped: one shared timer, armed only while some writer holds
// unsynced bytes, syncs every dirty writer after DefaultSyncInterval, and a
// writer whose dirty bytes reach DefaultSyncMaxDirty syncs at once. Sealing a
// chunk and pipe EOF join the same group (ADR-20): the sealed chunk, its index
// and directory are synced by the next group sync, unless the archiver has
// already committed the run and removed the buffer, in which case they are
// never synced at all. An orderly shutdown always syncs. Only an OS crash or
// power loss can lose output newer than the last sync, a finished run's tail
// included; a stalled disk can stretch that window beyond the nominal interval.
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

// ErrNoChunk is a write to a writer without an open chunk (degraded capture,
// already reported once per episode).
var ErrNoChunk = errors.New("log writer has no open chunk")

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
	// Plain marks a chunk stored as raw zstd blocks (adaptive capture): the
	// archive compresses it once, with the shared archive encoder.
	Plain bool `json:"plain,omitempty"`
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
	sealed       []sealedChunk // chunks sealed since the last group sync
	// unsyncedSeals holds the paths of deferred seals whose fsync has not
	// completed (queued or in a running pass). The archiver uses it to skip
	// fsyncs of chunks already synced and to keep the rest queued at their
	// archive path. started tells chunks written by an earlier process,
	// whose sync state is unknown.
	unsyncedSeals map[string]bool
	started       time.Time
	syncTimer     *time.Timer
	// flushMu lets one group sync pass run at a time: on a stalled disk a
	// timer that fires during a pass re-arms instead of starting another.
	flushMu sync.Mutex
	// syncFile performs a chunk fsync; tests replace it to count or fail syncs.
	syncFile        func(*os.File) error
	captureFailures atomic.Int64 // writers that entered degraded capture
	archiver        archiver
	// archiveEnc compresses plain chunks once, at archive time (EncodeAll is
	// safe for concurrent use by the archive workers).
	archiveEncOnce sync.Once
	archiveEnc     *zstd.Encoder
	// archivedFromImage counts runs archived from their handed-over plain
	// chunk image, without reading the buffer back.
	archivedFromImage atomic.Int64

	// Observability and disk budget (see budget.go and diagnostics.go).
	disk                    diskState
	lockWaits               lockWaitStats
	maint                   maintenanceStats
	droppedFrames           atomic.Int64 // output discarded under 2A, all runs
	droppedBytes            atomic.Int64
	syncFailures            atomic.Int64 // failed fsyncs, all runs
	quarantinePurgedEntries atomic.Int64
	quarantinePurgedBytes   atomic.Int64
}

// sealedChunk is a chunk sealed without its own fsync (ADR-20), waiting for
// the group sync together with its run's index and directory. number 0 queues
// only the directory (a rename into the archive directory).
type sealedChunk struct {
	dir    string
	number int
}

func (c sealedChunk) path() string { return filepath.Join(c.dir, fmt.Sprintf("%06d.zst", c.number)) }

// maxDeferredSeals bounds the sealed chunks waiting for a group sync; beyond
// it a seal syncs inline as before, so a disk slower than the seal rate costs
// latency, not memory.
const maxDeferredSeals = 4096

// archiver moves sealed run buffers into the log database in the background,
// keeping SQLite off the run-completion path. Fields are guarded by Store.mu.
type archiver struct {
	running bool
	queue   []string
	queued  map[string]bool
	// overflow is set when a sealed run was not queued because the queue held
	// maxArchiveQueue ids; sweeping marks that an orphan sweep is in progress.
	// The idle archiver then rediscovers sealed buffers from the directory
	// listing, so no id is ever lost, only deferred. overflowTotal counts them.
	overflow      bool
	sweeping      bool
	overflowTotal int64
	wake          chan struct{}
	stop          chan struct{}
	// ctx is canceled when shutdown runs out of time; every archive step
	// (database transaction, claim wait, slot wait, sweep) observes it.
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	// sealed holds the plain chunk images of queued small runs, so the
	// archiver need not read them back from disk; sealedBytes is bounded by
	// maxSealedImages. Entries are dropped when taken.
	sealed      map[string]*sealedRun
	sealedBytes int64
}

// sealedRun is a sealed run whose only unarchived chunk is plain, with the
// chunk's file image. The buffer on disk stays the durable record; the image
// is a copy taken after the writer closed, so it never changes.
type sealedRun struct {
	job, kind string
	meta      chunkMeta
	image     []byte
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
		unsyncedSeals:  make(map[string]bool),
		started:        time.Now(),
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

// OpenDegraded returns a writer for a run whose log storage could not be
// opened (cause). ADR-8 2A / ADR-16: the run still executes. The writer starts
// in degraded capture, discarding and counting output, and recovers lazily on
// a later line (creating the run directory then), exactly like a writer whose
// storage failed mid-run. It is registered so Seal and readers treat it as an
// ordinary active run.
func (s *Store) OpenDegraded(runID, job, kind string, opt WriterOptions, cause error) *Writer {
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	w := &Writer{store: s, runID: runID, job: job, kind: kind, dir: filepath.Join(s.root, runID), maxBytes: opt.MaxBytes, maxLine: opt.MaxLine, dropNew: opt.DropNew, frameSync: s.frameSync.Load()}
	w.mu.Lock()
	w.enterDegraded(cause)
	w.mu.Unlock()
	s.mu.Lock()
	s.writers[runID] = w
	s.mu.Unlock()
	return w
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
	var sealed *sealedRun
	if err == nil {
		sealed = w.sealedPlain()
	}
	lock := s.lockRun(runID, true)
	s.mu.Lock()
	delete(s.writers, runID)
	s.mu.Unlock()
	s.unlockRun(runID, lock, true)
	// A sealed buffer is a disk-pressure candidate and a finished run is the
	// moment its footprint changed; the hook only raises a coalesced hint.
	s.signalPressure()
	if err != nil || s.db == nil {
		return err
	}
	if async && s.enqueueSealed(runID, sealed) {
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
	clear(a.sealed) // queued runs not archived now are found by the next sweep
	a.sealedBytes = 0
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

// maxArchiveQueue bounds the archiver's in-memory list of sealed run ids.
// Beyond it the buffer simply stays on disk until the idle archiver's orphan
// sweep (or the periodic one) finds it, so the cap costs delay, not data.
var maxArchiveQueue = 4096 // a variable only so tests can shrink it

// maxArchiveStatDirs bounds the directory walk behind ArchiveStats.
const maxArchiveStatDirs = 10000

// ArchiveStats describes the archiver backlog and the sealed buffers still on
// disk. The directory walk runs per call (diagnostics requests), not in the
// background.
type ArchiveStats struct {
	Queued        int   `json:"queued"`
	Overflow      bool  `json:"overflow"`
	OverflowTotal int64 `json:"overflow_total"`
	SealedRuns    int   `json:"sealed_runs"`
	SealedBytes   int64 `json:"sealed_bytes"`
	Truncated     bool  `json:"truncated,omitempty"`
}

// ArchiveStats reports queued ids, overflow state, and the number and chunk
// bytes of buffers with no active writer (sealed runs awaiting archival,
// including ones the queue cap deferred).
func (s *Store) ArchiveStats() ArchiveStats {
	s.mu.Lock()
	st := ArchiveStats{Queued: len(s.archiver.queue), Overflow: s.archiver.overflow, OverflowTotal: s.archiver.overflowTotal}
	active := make(map[string]bool, len(s.writers))
	for id := range s.writers {
		active[id] = true
	}
	s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return st
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || active[e.Name()] {
			continue
		}
		if st.SealedRuns >= maxArchiveStatDirs {
			st.Truncated = true
			break
		}
		st.SealedRuns++
		files, err := os.ReadDir(filepath.Join(s.root, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".zst") {
				continue
			}
			if info, err := f.Info(); err == nil {
				st.SealedBytes += info.Size()
			}
		}
	}
	return st
}

// enqueueSealed is enqueue that also hands over the run's plain chunk image,
// while maxSealedImages allows. An image is kept only for a run that is
// actually queued: the queue is always drained through takeSealed, and
// stopping the archiver clears what is left.
func (s *Store) enqueueSealed(runID string, r *sealedRun) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &s.archiver
	if !a.running {
		return false
	}
	if s.enqueueLocked(runID) && r != nil && a.sealed[runID] == nil &&
		a.sealedBytes+int64(len(r.image)) <= maxSealedImages {
		if a.sealed == nil {
			a.sealed = make(map[string]*sealedRun)
		}
		a.sealed[runID] = r
		a.sealedBytes += int64(len(r.image))
	}
	return true
}

// takeSealed removes and returns a queued run's plain chunk image, if any.
func (s *Store) takeSealed(runID string) *sealedRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &s.archiver
	r := a.sealed[runID]
	if r != nil {
		delete(a.sealed, runID)
		a.sealedBytes -= int64(len(r.image))
	}
	return r
}

func (s *Store) hasSealed(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.archiver.sealed[runID] != nil
}

// enqueue hands a sealed run to the archiver. It reports false when the
// archiver is not running.
func (s *Store) enqueue(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.archiver.running {
		return false
	}
	s.enqueueLocked(runID)
	return true
}

// enqueueLocked queues runID on the running archiver and wakes it. It reports
// whether the run is in the queue; false means the queue overflowed and the
// next discovery sweep will find the run. Callers hold s.mu.
func (s *Store) enqueueLocked(runID string) bool {
	a := &s.archiver
	queued := true
	if !a.queued[runID] {
		if len(a.queue) >= maxArchiveQueue {
			a.overflow = true
			a.overflowTotal++
			queued = false
		} else {
			a.queued[runID] = true
			a.queue = append(a.queue, runID)
		}
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return queued
}

func (s *Store) archiveLoop() {
	a := &s.archiver
	for {
		s.mu.Lock()
		var batch []string
		var sweep bool
		ctx := a.ctx
		if len(a.queue) > 0 && ctx.Err() == nil {
			// Take what is queued, up to a batch: under load one transaction
			// archives many short runs; a lone run is archived alone, at once.
			n := min(len(a.queue), archiveRunBatch)
			batch = slices.Clone(a.queue[:n])
			clear(a.queue[:n])
			a.queue = a.queue[n:]
			for _, id := range batch {
				delete(a.queued, id)
			}
		} else if a.overflow && !a.sweeping && ctx.Err() == nil && a.running {
			// Idle with deferred ids: rediscover them from the directory.
			a.overflow, a.sweeping, sweep = false, true, true
		}
		stop := a.stop
		s.mu.Unlock()
		if sweep {
			err := fault.Call(func() error { return s.ArchiveOrphansContext(ctx) })
			s.mu.Lock()
			a.sweeping = false
			s.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				slog.Error("log archive sweep after queue overflow failed; the periodic sweep will retry", "error", err)
			}
			continue
		}
		if len(batch) == 0 {
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
		if err := fault.Call(func() error { s.archiveSealedBatch(ctx, batch); return nil }); err != nil {
			slog.Error("log archival failed; the orphan sweep will retry", "runs", len(batch), "error", err)
		}
	}
}

// archiveSealedBatch archives queued runs: the short ones together in one
// transaction, the rest one by one. Runs owned by a worker flush or a
// deletion are deferred until that owner releases them.
func (s *Store) archiveSealedBatch(ctx context.Context, ids []string) {
	var owned []string
	s.mu.Lock()
	for _, id := range ids {
		if s.owners[id] {
			s.deferred[id] = true
			continue
		}
		s.owners[id] = true
		owned = append(owned, id)
	}
	s.mu.Unlock()
	defer func() {
		for _, id := range owned {
			s.release(id)
		}
	}()
	rest := owned
	if len(owned) > 1 || (len(owned) == 1 && s.hasSealed(owned[0])) {
		rest = s.archiveShortRuns(ctx, owned)
	}
	for _, id := range rest {
		err := s.archiveOwned(ctx, id)
		s.recordOrphanResult(id, err)
		if err != nil {
			slog.Error("log archival failed; the orphan sweep will retry", "run", id, "error", err)
		}
	}
}

// shortRun is a sealed buffer that can join a multi-run transaction.
type shortRun struct {
	id   string
	lock *runLock
	logdb.RunChunks
}

// archiveShortRuns archives, in one transaction, the owned runs whose buffers
// are complete and small: a valid index that lists exactly the chunk files on
// disk, all below fileChunkMin. It returns the runs it did not take, for the
// one-by-one path. Callers own every run in ids.
func (s *Store) archiveShortRuns(ctx context.Context, ids []string) (rest []string) {
	if err := s.acquireArchiveSlot(ctx); err != nil {
		return ids
	}
	defer func() { <-s.archiveSlots }()
	var batch []shortRun
	// Results are recorded after the run locks are released: recording can
	// quarantine a buffer, which takes the run's lock itself.
	results := make(map[string]error)
	defer func() {
		for id, err := range results {
			s.recordOrphanResult(id, err)
		}
	}()
	defer func() {
		for _, r := range batch {
			s.unlockRun(r.id, r.lock, true)
		}
	}()
	var bytes int64
	chunks := 0
	for _, id := range ids {
		lock := s.lockRun(id, true)
		sealed := s.takeSealed(id)
		if s.Active(id) != nil {
			s.unlockRun(id, lock, true)
			continue // reopened: no longer sealed
		}
		dir := filepath.Join(s.root, id)
		var r logdb.RunChunks
		var size int64
		ok := false
		if sealed != nil {
			r, size, ok = s.sealedShortRun(dir, sealed)
		}
		if !ok {
			r, size, ok = s.loadShortRun(dir)
		}
		if !ok || (len(batch) > 0 && (bytes+size > archiveBatchBytes || chunks+len(r.Chunks) > archiveBatchChunks)) {
			s.unlockRun(id, lock, true)
			rest = append(rest, id)
			continue
		}
		r.RunID = id
		batch = append(batch, shortRun{id: id, lock: lock, RunChunks: r})
		bytes += size
		chunks += len(r.Chunks)
	}
	if len(batch) == 0 {
		return rest
	}
	runs := make([]logdb.RunChunks, len(batch))
	for i, r := range batch {
		runs[i] = r.RunChunks
	}
	if err := s.db.PutRuns(ctx, time.Now(), runs); err != nil {
		err = fmt.Errorf("%w: %w", errArchiveDB, err)
		for _, r := range batch {
			results[r.id] = err
		}
		if ctx.Err() == nil {
			slog.Error("log archival failed; the orphan sweep will retry", "runs", len(batch), "error", err)
		}
		return rest
	}
	for _, r := range batch {
		err := os.RemoveAll(filepath.Join(s.root, r.id))
		if err != nil {
			slog.Error("removing an archived log buffer failed; the orphan sweep will retry", "run", r.id, "error", err)
		}
		results[r.id] = err
	}
	return rest
}

// sealedShortRun builds a run's archive rows from its handed-over plain chunk
// image instead of reading the buffer back. The caller owns the run (as every
// deleter must), so a buffer that still exists cannot vanish meanwhile; one
// that is gone was deleted while queued and is not archived.
func (s *Store) sealedShortRun(dir string, sealed *sealedRun) (r logdb.RunChunks, size int64, ok bool) {
	if _, err := os.Stat(dir); err != nil {
		return r, 0, false
	}
	m := sealed.meta
	blob := s.archiveBlob(m, sealed.image)
	r.Job, r.Kind = sealed.job, sealed.kind
	r.Chunks = []logdb.Chunk{{Number: m.Number, First: m.First, Last: m.Last, RawBytes: m.Raw, Blob: blob}}
	s.archivedFromImage.Add(1)
	return r, int64(len(blob)), true
}

// loadShortRun reads a sealed buffer for a multi-run transaction. ok is false
// for anything the one-by-one path must handle: no or invalid index, chunks
// that are not indexed (salvage) or indexed but missing, and file chunks.
func (s *Store) loadShortRun(dir string) (r logdb.RunChunks, size int64, ok bool) {
	idx, sealed, err := readOrphanIndex(dir)
	if err != nil || len(sealed) == 0 {
		return r, 0, false
	}
	files, err := chunkFiles(dir)
	if err != nil || len(files) != len(sealed) {
		return r, 0, false
	}
	for _, f := range files {
		m, indexed := sealed[f.number]
		if !indexed || m.Bytes >= fileChunkMin {
			return r, 0, false
		}
		blob, err := os.ReadFile(f.path)
		if err != nil || int64(len(blob)) >= fileChunkMin {
			return r, 0, false
		}
		blob = s.archiveBlob(m, blob)
		r.Chunks = append(r.Chunks, logdb.Chunk{Number: f.number, First: m.First, Last: m.Last, RawBytes: m.Raw, Blob: blob})
		size += int64(len(blob))
	}
	r.Job, r.Kind = idx.Job, idx.Kind
	return r, size, true
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
	lastStored uint64 // sequence of the last frame written to a chunk file
	// Frames handed to the encoder by Pipe but not yet flushed to the file.
	// Pipe flushes once per read from the child instead of once per line, so
	// these never outlive the processing of one read buffer (see Pipe).
	pendFrames int64
	pendRaw    int
	// Plain chunk state (adaptive capture). plainImg is the chunk's whole
	// file image (frame header, then blocks); plainOut bytes of it are in the
	// file and plainBlk is the offset of the open block's header, or -1. The
	// image doubles as the archiver's copy of a small sealed run.
	plain    bool
	plainImg []byte
	plainOut int
	plainBlk int
	// promoted: output outgrew plainLimit, so new chunks are compressed until
	// the next checkpoint.
	promoted  bool
	total     int64 // accepted raw bytes, less output evicted before archival
	buffered  int64 // raw bytes still occupying the file buffer
	maxBytes  int64
	maxLine   int
	dropNew   bool
	frameSync bool
	unsynced  int // bytes written to the hot chunk since its last fsync
	syncs     atomic.Int64
	truncated bool
	idx       index
	header    [24]byte
	closed    bool
	closeErr  error
	chunkErr  error // finalization failures leave the stream unusable until recovered
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
	// A checkpoint re-decides the capture mode: the next chunk starts plain
	// and the encoder is released, so a worker that has gone quiet stops
	// holding one. A still-chatty worker is promoted again after plainLimit.
	w.promoted = false
	if w.chunkRaw > 0 {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	} else if w.file != nil && !w.plain {
		_ = w.startChunk(w.file, false) // the open chunk is empty: switch in place
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
	// A nil file means the previous chunk was abandoned after a storage
	// failure (or none exists yet); an abandoned chunk stays on disk,
	// unindexed, for the orphan sweep.
	if w.file != nil {
		if err := w.finishChunk(); err != nil {
			w.chunkErr = errors.Join(err, f.Close(), os.Remove(path))
			return w.chunkErr
		}
	}
	if err := w.startChunk(f, w.promoted); err != nil {
		return errors.Join(err, f.Close(), os.Remove(path))
	}
	w.chunk = next
	w.file, w.chunkRaw, w.chunkFirst = f, 0, w.seq+1
	// finishChunk synced everything written to the previous chunk.
	w.unsynced = 0
	// A rotation sealed a chunk and started a file: the cheap moment to ask
	// whether disk pressure needs a pass (a coalesced hint, never inline work).
	w.store.signalPressure()
	return nil
}

// startChunk prepares the stream of a new chunk file f: plain, releasing any
// encoder, or compressed, reusing the encoder's buffers when it has one.
func (w *Writer) startChunk(f *os.File, compressed bool) error {
	if !compressed {
		w.enc = nil
		w.plain = true
		w.plainImg = append(w.plainImg[:0], plainFrameHeader...)
		w.plainOut, w.plainBlk = 0, -1
		return nil
	}
	w.plain, w.plainImg, w.plainOut, w.plainBlk = false, nil, 0, -1
	if w.enc != nil {
		// Close sealed the old stream; Reset starts an independent chunk while
		// retaining the encoder's match window and compression buffers.
		w.enc.Reset(f)
		return nil
	}
	enc, err := zstd.NewWriter(f,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(encoderWindow),
		zstd.WithLowerEncoderMem(true),
	)
	if err != nil {
		return fmt.Errorf("create log encoder: %w", err)
	}
	w.enc = enc
	return nil
}

// flushPlain writes the plain chunk's open block, and with last the end of
// its zstd frame. A chunk that never received a frame stays an empty file.
func (w *Writer) flushPlain(last bool) error {
	if w.plainBlk >= 0 {
		putRawBlockHeader(w.plainImg[w.plainBlk:], len(w.plainImg)-w.plainBlk-3, false)
		w.plainBlk = -1
	}
	if last {
		if len(w.plainImg) == len(plainFrameHeader) {
			return nil
		}
		w.plainImg = append(w.plainImg, 1, 0, 0) // empty last raw block
	}
	if w.plainOut == len(w.plainImg) {
		return nil
	}
	if _, err := w.file.Write(w.plainImg[w.plainOut:]); err != nil {
		return err
	}
	w.plainOut = len(w.plainImg)
	return nil
}

// putRawBlockHeader writes a zstd block header for a raw block of size bytes.
func putRawBlockHeader(dst []byte, size int, last bool) {
	v := uint32(size) << 3
	if last {
		v |= 1
	}
	dst[0], dst[1], dst[2] = byte(v), byte(v>>8), byte(v>>16)
}

func (w *Writer) finishChunk() error {
	if w.file == nil {
		if w.chunkErr == nil && w.chunk > 0 {
			// The chunk was abandoned after a storage failure: still record the
			// final index so truncation and drop evidence reaches disk.
			if err := w.writeIndex(); err != nil {
				w.chunkErr = fmt.Errorf("index log chunk %d: %w", w.chunk, err)
				w.chunkIndexOnly = true
			}
		}
		return w.chunkErr
	}
	var encErr error
	if w.plain {
		encErr = w.flushPlain(true)
	} else {
		encErr = w.enc.Close()
	}
	// Batch durability defers the seal's fsyncs to the group sync (ADR-20);
	// strict per-frame durability, or a full deferral list, syncs now.
	deferred := !w.frameSync && w.store.deferSealRoom()
	var syncErr error
	if !deferred {
		syncErr = w.file.Sync()
	}
	info, statErr := w.file.Stat()
	closeErr := w.file.Close()
	w.file = nil
	if err := errors.Join(encErr, syncErr, statErr, closeErr); err != nil {
		w.chunkErr = fmt.Errorf("finalize log chunk %d: %w", w.chunk, err)
		w.chunkIndexOnly = false
		return w.chunkErr
	}
	// Closing wrote any frames Pipe left pending in the stream.
	w.lastStored = w.seq
	w.pendFrames, w.pendRaw = 0, 0
	if w.seq >= w.chunkFirst {
		w.idx.Chunks = append(w.idx.Chunks, chunkMeta{Number: w.chunk, First: w.chunkFirst, Last: w.seq, Bytes: info.Size(), Raw: int64(w.chunkRaw), Plain: w.plain})
	}
	indexErr := w.writeIndexSync(!deferred)
	if deferred {
		// Queued after the index is in place, so the group sync covers the
		// new index; queued even when it failed, so the chunk is synced.
		w.store.deferSeal(w.dir, w.chunk)
	}
	if indexErr != nil {
		w.chunkErr = fmt.Errorf("index log chunk %d: %w", w.chunk, indexErr)
		w.chunkIndexOnly = true
	}
	return w.chunkErr
}
func (w *Writer) writeIndex() error { return w.writeIndexSync(true) }

// writeIndexSync replaces index.json. Without sync the new index is renamed
// into place unsynced; the caller queues it for the group sync (ADR-20).
func (w *Writer) writeIndexSync(sync bool) error {
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
	if _, err = f.Write(b); err == nil && sync {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(w.dir, "index.json")); err != nil || !sync {
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
	lock := w.store.lockRunTimed(w.runID)
	w.lockTimed()
	needSync, err := w.writeLocked(stream, payload, flags, true)
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
func (w *Writer) capture(stream Stream, payload []byte, flags Flags, flush bool) error {
	lock := w.store.lockRunTimed(w.runID)
	w.lockTimed()
	needSync, err := w.captureLocked(stream, payload, flags, flush)
	w.mu.Unlock()
	w.store.unlockRun(w.runID, lock, true)
	if needSync {
		_ = w.Sync() // a failure is recorded as capture evidence
	}
	return err
}

func (w *Writer) captureLocked(stream Stream, payload []byte, flags Flags, flush bool) (bool, error) {
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
	needSync, err := w.writeLocked(stream, payload, flags, flush)
	if err != nil {
		w.enterDegraded(err)
		w.discard(len(payload))
		return false, nil
	}
	return needSync, nil
}

// discard counts output that could not be stored. Callers hold w.mu.
func (w *Writer) discard(payload int) { w.discardFrames(1, int64(payload+24)) }

func (w *Writer) discardFrames(frames, size int64) {
	w.droppedFrames += frames
	w.droppedBytes += size
	w.episodeFrames += frames
	w.episodeBytes += size
	w.truncated = true
	w.store.droppedFrames.Add(frames)
	w.store.droppedBytes.Add(size)
}

// dropPending accounts for frames that were handed to the encoder but never
// reached the file because a later encode, flush or rotation failed. Callers
// hold w.mu.
func (w *Writer) dropPending() {
	if w.pendFrames > 0 {
		w.discardFrames(w.pendFrames, int64(w.pendRaw))
		w.chunkRaw -= w.pendRaw
		w.total -= int64(w.pendRaw)
		w.buffered -= int64(w.pendRaw)
	}
	w.pendFrames, w.pendRaw = 0, 0
	if w.plain {
		w.plainImg = w.plainImg[:max(w.plainOut, len(plainFrameHeader))]
		w.plainBlk = -1
	}
}

// flushPending writes frames Pipe left in the encoder to the file. Pipe calls
// it when it stops early; the normal path flushes with the last line of each
// read.
func (w *Writer) flushPending() {
	lock := w.store.lockRunTimed(w.runID)
	w.lockTimed()
	var needSync bool
	if w.pendFrames > 0 && !w.closed {
		var err error
		if needSync, err = w.flushLocked(0); err != nil {
			w.enterDegraded(err)
		}
	}
	w.mu.Unlock()
	w.store.unlockRun(w.runID, lock, true)
	if needSync {
		_ = w.Sync()
	}
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
		// A full disk is the likeliest cause: ask for a disk-pressure pass.
		w.store.signalPressure()
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
	if _, err := w.writeLocked(System, []byte(summary), FlagTruncated, true); err != nil {
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
	w.indexAbandonedChunk()
	// A writer opened degraded may not have its directory yet.
	if err := os.Mkdir(w.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return w.rotate()
}

// indexAbandonedChunk registers the chunk just abandoned after a storage
// failure in the index, with the frames known to have been written intact. It
// is then accounted by the log_max eviction, archived verbatim by the worker
// checkpoint (a torn tail decodes up to its last block, like any live chunk)
// and removed with the run, instead of lingering unindexed until the run ends.
// Callers hold w.mu; the chunk's file is already closed.
func (w *Writer) indexAbandonedChunk() {
	if w.chunk == 0 || w.chunkRaw == 0 || w.lastStored < w.chunkFirst {
		return
	}
	if n := len(w.idx.Chunks); n > 0 && w.idx.Chunks[n-1].Number == w.chunk {
		return // already indexed (only the index install had failed)
	}
	var size int64
	if info, err := os.Stat(w.chunkPath(w.chunk)); err == nil {
		size = info.Size()
	}
	w.idx.Chunks = append(w.idx.Chunks, chunkMeta{Number: w.chunk, First: w.chunkFirst, Last: w.lastStored, Bytes: size, Raw: int64(w.chunkRaw), Plain: w.plain})
	w.chunkRaw = 0 // the next rotate starts a fresh chunk; keep a retry from re-indexing this one
}

// writeLocked appends one frame. With flush (always in "frame" durability)
// the frame and any frames Pipe left pending reach the chunk file before it
// returns, so a daemon crash cannot lose them. Without flush the frame stays
// in the encoder until the caller's next flushing write; only Pipe does that,
// for lines it already holds in its read buffer. It reports whether the
// writer should fsync now (strict mode, or the dirty-byte limit was reached);
// otherwise the shared timer syncs it. The caller syncs after unlocking.
// On error, pending frames other than this one are counted as discarded.
// Callers hold the run lock and w.mu.
func (w *Writer) writeLocked(stream Stream, payload []byte, flags Flags, flush bool) (bool, error) {
	if w.closed {
		return false, errWriterClosed
	}
	if w.chunkErr != nil {
		return false, w.chunkErr
	}
	if w.file == nil || (!w.plain && w.enc == nil) {
		// No open chunk: a writer opened degraded, or a reopen that failed.
		return false, ErrNoChunk
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
	if w.plain && w.chunkRaw+frameSize > plainLimit {
		// Promote: this run's output outgrew a plain chunk. An empty chunk
		// switches in place (nothing is in its file yet).
		w.promoted = true
		if w.chunkRaw > 0 {
			if err := w.rotate(); err != nil {
				w.dropPending()
				return false, err
			}
		} else if err := w.startChunk(w.file, true); err != nil {
			return false, err
		}
	}
	if w.chunkRaw+frameSize > chunkLimit && w.chunkRaw > 0 {
		if err := w.rotate(); err != nil {
			w.dropPending()
			return false, err
		}
	}
	w.seq++
	f := Frame{w.seq, time.Now().UTC(), stream, flags, payload}
	if w.plain {
		if w.plainBlk < 0 {
			w.plainBlk = len(w.plainImg)
			w.plainImg = append(w.plainImg, 0, 0, 0)
		}
		w.plainImg = appendFrame(w.plainImg, f)
	} else if err := encodeWithHeader(w.enc, f, w.header[:]); err != nil {
		w.dropPending()
		return false, err
	}
	w.chunkRaw += frameSize
	w.total += int64(frameSize)
	w.buffered += int64(frameSize)
	if !flush && !w.frameSync {
		w.pendFrames++
		w.pendRaw += frameSize
		return false, nil
	}
	return w.flushLocked(frameSize)
}

// flushLocked writes the encoder's pending blocks to the chunk file, so a
// daemon crash cannot lose accepted output; readers see it through the page
// cache. frameSize is the frame just encoded, not counted in pendRaw.
func (w *Writer) flushLocked(frameSize int) (bool, error) {
	var err error
	if w.plain {
		err = w.flushPlain(false)
	} else {
		err = w.enc.Flush()
	}
	if err != nil {
		w.dropPending()
		return false, err
	}
	w.lastStored = w.seq
	before := w.unsynced
	w.unsynced += w.pendRaw + frameSize
	w.pendFrames, w.pendRaw = 0, 0
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
		s.syncTimer = time.AfterFunc(time.Duration(s.syncInterval.Load()), s.timerFlush)
	}
	s.syncMu.Unlock()
}

// timerFlush is the group sync timer's callback. If a pass is still running
// (a slow fsync), it re-arms instead of piling up blocked goroutines.
func (s *Store) timerFlush() {
	if !s.flushMu.TryLock() {
		// syncTimer is this timer, which has fired: replace it. Leaving it in
		// place would look armed forever and nothing would arm a new one.
		s.syncMu.Lock()
		if s.syncTimer != nil {
			s.syncTimer.Stop()
		}
		s.syncTimer = time.AfterFunc(time.Duration(s.syncInterval.Load()), s.timerFlush)
		s.syncMu.Unlock()
		return
	}
	defer s.flushMu.Unlock()
	s.flushDirtyLocked()
}

// deferSealRoom reports whether the next group sync can take another sealed
// chunk; when it is full the caller syncs inline.
func (s *Store) deferSealRoom() bool {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	return len(s.sealed) < maxDeferredSeals
}

// deferSeal queues a sealed chunk, written and indexed but not synced, for
// the next group sync and arms the timer.
func (s *Store) deferSeal(dir string, number int) {
	s.syncMu.Lock()
	s.deferSealLocked(sealedChunk{dir, number})
	s.syncMu.Unlock()
}

func (s *Store) deferSealLocked(c sealedChunk) {
	s.sealed = append(s.sealed, c)
	if c.number > 0 {
		s.unsyncedSeals[c.path()] = true
	}
	if s.syncTimer == nil {
		s.syncTimer = time.AfterFunc(time.Duration(s.syncInterval.Load()), s.timerFlush)
	}
}

// flushDirty syncs every dirty writer and every chunk sealed since the last
// pass once, and disarms the timer. Writes that arrive meanwhile re-arm it.
// It waits for a pass already in progress.
func (s *Store) flushDirty() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.flushDirtyLocked()
}

func (s *Store) flushDirtyLocked() {
	s.syncMu.Lock()
	if s.syncTimer != nil {
		s.syncTimer.Stop()
		s.syncTimer = nil
	}
	batch := slices.Collect(maps.Keys(s.dirty))
	clear(s.dirty)
	sealed := s.sealed
	s.sealed = nil
	s.syncMu.Unlock()
	for _, w := range batch {
		_ = w.Sync() // failures are recorded on the writer
	}
	s.syncSealed(sealed)
}

// syncSealed makes deferred seals durable: the chunk files and indexes, then
// the run directories. A buffer the archiver already committed and removed (or
// a deletion removed) is skipped: its output is durable in the archive, or gone
// on purpose. The fsyncs of each step run a few at a time so the filesystem
// can share journal commits between them, as it did for the concurrent inline
// seal syncs this replaces. Failures are counted and logged once per pass; the
// writer is gone, so they cannot mark the run.
func (s *Store) syncSealed(sealed []sealedChunk) {
	if len(sealed) == 0 {
		return
	}
	var failMu sync.Mutex
	var failed error
	fail := func(err error) {
		s.syncFailures.Add(1)
		failMu.Lock()
		if failed == nil {
			failed = err
		}
		failMu.Unlock()
	}
	syncFile := func(path string) bool {
		f, err := os.Open(path)
		if err == nil {
			if err = errors.Join(s.fsync(f), f.Close()); err != nil {
				fail(err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			fail(err)
		}
		s.syncMu.Lock()
		delete(s.unsyncedSeals, path)
		s.syncMu.Unlock()
		return err == nil
	}
	files := make([]string, 0, 2*len(sealed))
	seen := make(map[string]bool)
	var dirsMu sync.Mutex
	dirs := make(map[string]bool)
	for _, c := range sealed {
		if c.number == 0 {
			dirs[c.dir] = true
			continue
		}
		files = append(files, c.path())
		if !seen[c.dir] {
			seen[c.dir] = true
			files = append(files, filepath.Join(c.dir, "index.json"))
		}
	}
	parallelSync(files, func(path string) {
		if syncFile(path) {
			dirsMu.Lock()
			dirs[filepath.Dir(path)] = true
			dirsMu.Unlock()
		}
	})
	parallelSync(slices.Collect(maps.Keys(dirs)), func(dir string) {
		if err := syncDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fail(err)
		}
	})
	if failed != nil {
		slog.Error("log fsync of sealed chunks failed; recent output may not be durable", "error", failed)
	}
}

// sealSyncWorkers bounds the concurrent fsyncs of one group sync pass.
const sealSyncWorkers = 8

// parallelSync runs fn over paths with at most sealSyncWorkers goroutines,
// which exit when the pass ends.
func parallelSync(paths []string, fn func(string)) {
	if len(paths) <= 1 {
		for _, p := range paths {
			fn(p)
		}
		return
	}
	next := make(chan string)
	var wg sync.WaitGroup
	for range min(sealSyncWorkers, len(paths)) {
		wg.Go(func() {
			for p := range next {
				fn(p)
			}
		})
	}
	for _, p := range paths {
		next <- p
	}
	close(next)
	wg.Wait()
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
	w.store.syncFailures.Add(1)
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
// SetGroupSync); the end of r joins the group too (ADR-20).
//
// Lines are compressed one by one but flushed to the chunk file once per read
// from r: when the read buffer holds no further bytes, after pipeReadBuffer
// bytes of frames (or the dirty-byte sync limit, if smaller), at EOF, or on a read error. A burst of short lines
// therefore costs one zstd block and one write(2) instead of one per line. No
// output waits for more input: the last line of every read is flushed before
// Pipe blocks on r again.
func (w *Writer) Pipe(stream Stream, r io.Reader) error {
	limit := w.maxLine
	if limit <= 0 || limit > maxFramePayload {
		limit = maxFramePayload
	}
	br := bufio.NewReaderSize(r, min(limit+1, pipeReadBuffer))
	// line holds a line that spans more than one read buffer; it is allocated
	// on first use, so most runs never allocate it.
	var line []byte
	truncated := false
	batched := 0 // frame bytes captured since the last flush
	flushNow := func(n int) bool {
		batched += n + 24
		// The dirty-byte limit (logs.sync_max_dirty, possibly below the read
		// size) also bounds a batch, so it keeps its byte granularity.
		if br.Buffered() == 0 || batched >= min(pipeReadBuffer, int(w.store.syncMaxDirty.Load())) {
			batched = 0
			return true
		}
		return false
	}
	defer w.flushPending()
	for {
		fragment, err := br.ReadSlice('\n')
		hasNewline := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if hasNewline {
			fragment = fragment[:len(fragment)-1]
		}
		if hasNewline && len(line) == 0 && !truncated && len(fragment) <= limit {
			// The whole line is in the read buffer (ReadSlice found the
			// newline, so err is nil): capture it in place. The encoder copies
			// it before ReadSlice reuses the buffer.
			if writeErr := w.captureLine(stream, fragment, flushNow(len(fragment))); writeErr != nil {
				return writeErr
			}
			continue
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
			if writeErr := w.capture(stream, line, flags, flushNow(len(line)) || err != nil); writeErr != nil {
				return writeErr
			}
			line = line[:0]
			if cap(line) > pipeReadBuffer {
				line = nil // do not keep a long line's buffer for the run's lifetime
			}
			truncated = false
		}
		// The end of a stream does not sync on its own (ADR-20): the bytes are
		// in the file and the writer is already queued for the group sync,
		// which the seal right after joins.
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}

// pipeReadBuffer is the read size of each output pipe pump. A typical run
// prints a few hundred bytes; 16 KiB still takes a 500-line burst in a few
// reads. Longer lines are assembled across reads up to the line limit.
const pipeReadBuffer = 16 << 10

// captureLine stores one complete line held in the read buffer.
func (w *Writer) captureLine(stream Stream, line []byte, flush bool) error {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	flags := Flags(0)
	if !utf8.Valid(line) {
		flags |= FlagInvalidUTF8
	}
	return w.capture(stream, line, flags, flush)
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

// sealedPlain returns the closed writer's archive handoff: its only
// unarchived chunk when that chunk is plain and complete on disk, else nil.
func (w *Writer) sealedPlain() *sealedRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed || w.closeErr != nil || w.degraded || w.droppedFrames > 0 || len(w.idx.Chunks) != 1 {
		return nil
	}
	m := w.idx.Chunks[0]
	if !m.Plain || m.Number != w.chunk || w.plainOut != len(w.plainImg) {
		return nil
	}
	return &sealedRun{job: w.job, kind: w.kind, meta: m, image: w.plainImg}
}

func (w *Writer) Stats() (int64, bool) { w.mu.Lock(); defer w.mu.Unlock(); return w.total, w.truncated }
func encode(dst io.Writer, f Frame) error {
	var h [24]byte
	return encodeWithHeader(dst, f, h[:])
}

// appendFrame appends f in the record format encode writes.
func appendFrame(dst []byte, f Frame) []byte {
	dst = append(dst, Version, byte(f.Stream), byte(f.Flags), 0)
	dst = binary.BigEndian.AppendUint64(dst, f.Sequence)
	dst = binary.BigEndian.AppendUint64(dst, uint64(f.Timestamp.UnixMicro()))
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(f.Payload)))
	return append(dst, f.Payload...)
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

// archiveEncoder returns the store's shared encoder for archive-time
// compression of plain chunks.
func (s *Store) archiveEncoder() *zstd.Encoder {
	s.archiveEncOnce.Do(func() {
		s.archiveEnc, _ = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithEncoderConcurrency(archiveWorkers),
			zstd.WithWindowSize(encoderWindow),
			zstd.WithLowerEncoderMem(true),
		)
	})
	return s.archiveEnc
}

// archiveBlob returns what the archive stores for a sealed chunk: a plain
// chunk compressed in one pass when that is smaller, otherwise the chunk as
// it is (a plain chunk is valid zstd as well).
func (s *Store) archiveBlob(m chunkMeta, blob []byte) []byte {
	if !m.Plain {
		return blob
	}
	records, err := plainRecords(blob)
	if err != nil || len(records) == 0 {
		return blob
	}
	out := s.archiveEncoder().EncodeAll(records, make([]byte, 0, len(records)/2+64))
	if len(out) >= len(blob) {
		return blob
	}
	return out
}

// plainRecords returns the frame records of a complete plain chunk: the
// payloads of its raw blocks, through the last block.
func plainRecords(chunk []byte) ([]byte, error) {
	if !bytes.HasPrefix(chunk, plainFrameHeader) {
		return nil, errors.New("not a plain log chunk")
	}
	rest := chunk[len(plainFrameHeader):]
	var out []byte
	for len(rest) >= 3 {
		v := uint32(rest[0]) | uint32(rest[1])<<8 | uint32(rest[2])<<16
		size := int(v >> 3)
		if (v>>1)&3 != 0 || len(rest) < 3+size {
			return nil, errors.New("malformed plain log chunk")
		}
		out = append(out, rest[3:3+size]...)
		rest = rest[3+size:]
		if v&1 == 1 {
			if len(rest) != 0 {
				return nil, errors.New("data after the end of a plain log chunk")
			}
			return out, nil
		}
	}
	return nil, errors.New("plain log chunk without its last block")
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
			if c.FileBytes > 0 {
				f, err := s.openFileChunk(runID, c.Number)
				if errors.Is(err, fs.ErrNotExist) {
					return true, nil // pruned after its row was read
				}
				if err != nil {
					return false, err
				}
				defer f.Close()
				var consumeErr error
				more, consumeErr = consume(f)
				return more, consumeErr
			}
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

// openFileChunk opens an archived file chunk (ADR-20). Its row is committed
// before the buffer file is renamed into the archive directory, so after a
// crash in between the chunk is still at its buffer path until the next sweep.
func (s *Store) openFileChunk(runID string, number int) (*os.File, error) {
	f, err := os.Open(s.db.ChunkPath(runID, number))
	if errors.Is(err, fs.ErrNotExist) {
		f, err = os.Open(filepath.Join(s.root, runID, fmt.Sprintf("%06d.zst", number)))
	}
	return f, err
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
