package logstore

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func newGroupSyncStore(t *testing.T, interval time.Duration, maxDirty int64) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.SetGroupSync(interval, maxDirty)
	return s
}

func (s *Store) timerArmed() (armed bool, dirty int) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	return s.syncTimer != nil, len(s.dirty)
}

// ADR-8 1B: a sparse writer is synced by the shared timer without another line
// arriving, and nothing keeps running once it is clean.
func TestSparseWriterSyncsWithoutAnotherLineAndLeavesNoTimer(t *testing.T) {
	const interval = 200 * time.Millisecond
	s := newGroupSyncStore(t, interval, 0)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if armed, dirty := s.timerArmed(); armed || dirty != 0 {
		t.Fatalf("an idle writer armed a timer (armed=%v dirty=%d)", armed, dirty)
	}
	if err := w.Write(Stdout, []byte("only line"), 0); err != nil {
		t.Fatal(err)
	}
	if syncs := w.syncs.Load(); syncs != 0 {
		t.Fatalf("%d fsyncs before the group interval; the frame must only be written through", syncs)
	}
	// Written through: visible to a reader and daemon-crash safe already.
	if frames, err := s.Read("run", 0, 10); err != nil || len(frames) != 1 {
		t.Fatalf("write-through frame unreadable: %v, %v", frames, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.syncs.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a dirty sparse writer was never synced")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(3 * interval)
	if syncs := w.syncs.Load(); syncs != 1 {
		t.Fatalf("%d fsyncs for one sparse line, want 1", syncs)
	}
	if armed, dirty := s.timerArmed(); armed || dirty != 0 {
		t.Fatalf("timer or dirty set left behind by a clean writer (armed=%v dirty=%d)", armed, dirty)
	}
	w.mu.Lock()
	unsynced := w.unsynced
	w.mu.Unlock()
	if unsynced != 0 {
		t.Fatalf("%d bytes unsynced after the group sync", unsynced)
	}
}

// Many sparse writers share one timer and one sync pass.
func TestSparseWritersShareOneTimer(t *testing.T) {
	s := newGroupSyncStore(t, time.Hour, 0)
	var writers []*Writer
	for i := range 8 {
		w, err := s.Open(fmt.Sprintf("run-%d", i), "job", model.KindJob, WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(Stdout, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
		writers = append(writers, w)
	}
	if armed, dirty := s.timerArmed(); !armed || dirty != 8 {
		t.Fatalf("armed=%v dirty=%d, want one timer covering 8 writers", armed, dirty)
	}
	s.SyncAll()
	for i, w := range writers {
		if syncs := w.syncs.Load(); syncs != 1 {
			t.Fatalf("writer %d: %d fsyncs", i, syncs)
		}
	}
	if armed, dirty := s.timerArmed(); armed || dirty != 0 {
		t.Fatalf("after SyncAll armed=%v dirty=%d", armed, dirty)
	}
}

// A burst below the dirty-byte limit costs one fsync, by the group sync after
// EOF, whatever the line count; a smaller limit bounds the unsynced window by
// bytes.
func TestBurstySyncCountFollowsDirtyByteLimit(t *testing.T) {
	const lines = 20000 // about 700 KB of frames
	input := manyLines(lines)

	s := newGroupSyncStore(t, time.Hour, 0)
	w, err := s.Open("burst", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if syncs := w.syncs.Load(); syncs != 0 {
		t.Fatalf("burst under the dirty limit took %d fsyncs before the group sync", syncs)
	}
	s.SyncAll()
	if syncs := w.syncs.Load(); syncs != 1 {
		t.Fatalf("burst under the dirty limit took %d fsyncs, want one group sync", syncs)
	}

	const limit = 64 << 10
	s = newGroupSyncStore(t, time.Hour, limit)
	w, err = s.Open("limited", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	raw := int64(lines * (24 + len("line 000000")))
	if syncs, max := w.syncs.Load(), raw/limit+2; syncs < raw/limit || syncs > max {
		t.Fatalf("%d fsyncs for %d bytes with a %d byte limit, want %d..%d", syncs, raw, limit, raw/limit, max)
	}
}

// Strict mode keeps its per-frame guarantee and never uses the shared timer.
func TestStrictModeSyncsEveryFrameWithoutTimer(t *testing.T) {
	s := newGroupSyncStore(t, time.Hour, 0)
	s.SetFrameSync(true)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := w.Write(Stdout, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if syncs := w.syncs.Load(); syncs != 10 {
		t.Fatalf("%d fsyncs for 10 strict frames", syncs)
	}
	if armed, dirty := s.timerArmed(); armed || dirty != 0 {
		t.Fatalf("strict writer used the group timer (armed=%v dirty=%d)", armed, dirty)
	}
}

// EOF and seal join the group sync (ADR-20); orderly shutdown always syncs
// what is dirty or sealed.
func TestFinalSyncOnEOFSealAndShutdown(t *testing.T) {
	var syncs int
	s := newGroupSyncStore(t, time.Hour, 0)
	s.syncFile = func(f *os.File) error { hookMu.Lock(); syncs++; hookMu.Unlock(); return f.Sync() }

	w, err := s.Open("eof", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("a\nb\n"))); err != nil {
		t.Fatal(err)
	}
	if syncs != 0 {
		t.Fatalf("pipe EOF: %d syncs, want them left to the group sync", syncs)
	}
	if armed, dirty := s.timerArmed(); !armed || dirty != 1 {
		t.Fatalf("pipe EOF: armed=%v dirty=%d, want the writer queued", armed, dirty)
	}
	s.SyncAll()
	if syncs != 1 {
		t.Fatalf("group sync after pipe EOF: %d syncs, want 1", syncs)
	}

	w2, err := s.Open("shutdown", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Write(Stdout, []byte("late"), 0); err != nil {
		t.Fatal(err)
	}
	before := syncs
	stopArchiver(t, s) // orderly shutdown path used by the daemon
	if syncs != before+1 {
		t.Fatalf("shutdown did not sync the dirty writer (syncs %d -> %d)", before, syncs)
	}

	// Sealing queues the chunk and its index for the group sync; the closed
	// writer itself has nothing left to sync.
	w3, err := s.Open("seal", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w3.Write(Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	before = syncs
	if err := s.Close("seal"); err != nil {
		t.Fatal(err)
	}
	if syncs != before {
		t.Fatalf("seal synced inline (%d -> %d), want it deferred", before, syncs)
	}
	if armed, _ := s.timerArmed(); !armed {
		t.Fatal("seal did not arm the group sync")
	}
	s.SyncAll()
	if w3.syncs.Load() != 0 {
		t.Fatal("closed writer was synced twice")
	}
	if syncs != before+2 {
		t.Fatalf("group sync after seal: %d syncs, want chunk and index (2)", syncs-before)
	}
}

// A sealed run the archiver removes before the group sync is never fsynced
// in the buffer: the archive commit made it durable (ADR-20).
func TestArchivedSealSkipsBufferSync(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	s.SetGroupSync(time.Hour, 0)
	var syncs int
	s.syncFile = func(f *os.File) error { hookMu.Lock(); syncs++; hookMu.Unlock(); return f.Sync() }
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("a\nb\n"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil { // archives inline
		t.Fatal(err)
	}
	s.SyncAll()
	if syncs != 0 {
		t.Fatalf("%d buffer fsyncs for a run archived before the group sync", syncs)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil || len(frames) != 2 {
		t.Fatalf("got %d frames, error %v", len(frames), err)
	}
}

// Strict per-frame durability keeps syncing at seal.
func TestStrictModeSealSyncsInline(t *testing.T) {
	var syncs int
	s := newGroupSyncStore(t, time.Hour, 0)
	s.SetFrameSync(true)
	s.syncFile = func(f *os.File) error { hookMu.Lock(); syncs++; hookMu.Unlock(); return f.Sync() }
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	if armed, _ := s.timerArmed(); armed {
		t.Fatal("strict seal used the group sync")
	}
	s.syncMu.Lock()
	pending := len(s.sealed)
	s.syncMu.Unlock()
	if pending != 0 {
		t.Fatalf("%d strict seals deferred", pending)
	}
}

// The accepted OS-loss window versus daemon-crash recovery: output newer than
// the last group sync is not fsynced, yet a daemon crash (the process dies with
// the OS page cache intact) loses none of it.
func TestDaemonCrashLosesNothingInsideTheUnsyncedWindow(t *testing.T) {
	recovering, _, dir := newArchiveStore(t)
	crashed, err := New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	crashed.SetGroupSync(time.Hour, 0)
	w, err := crashed.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, io.MultiReader(bytes.NewReader(manyLines(100)), blockedReader{})); err == nil {
		t.Fatal("expected the pipe to end with the injected read error")
	}
	// The pipe error path syncs; write more so the window is open at the crash.
	for i := range 5 {
		if err := w.Write(Stdout, fmt.Appendf(nil, "unsynced %d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	w.mu.Lock()
	unsynced := w.unsynced
	w.mu.Unlock()
	if unsynced == 0 {
		t.Fatal("expected unsynced bytes inside the OS-loss window")
	}
	// The daemon dies here: no Close, no final sync, no index. A new daemon
	// sweeps the buffer into its archive.
	t.Cleanup(crashed.flushDirty)
	if err := recovering.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	frames, err := recovering.Read("run", 0, 1000)
	if err != nil || len(frames) != 105 {
		t.Fatalf("recovered %d of 105 frames, error %v", len(frames), err)
	}
	if got := string(frames[104].Payload); got != "unsynced 4" {
		t.Fatalf("last recovered frame %q", got)
	}
}

type blockedReader struct{}

func (blockedReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

// Review S3: Sync released w.mu before its fsync and zeroed the dirty count, so
// a second strict-mode Write saw nothing unsynced and was acknowledged while
// the first fsync that covered its neighbouring bytes was still in flight.
func TestStrictWriteWaitsForInFlightFsync(t *testing.T) {
	s := newGroupSyncStore(t, time.Hour, 0)
	s.SetFrameSync(true)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	var mu sync.Mutex
	s.syncFile = func(f *os.File) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
		return f.Sync()
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- w.Write(Stdout, []byte("a"), 0) }()
	<-entered // A's fsync is in flight, A's frame is claimed by it
	// B's bytes land while A's fsync is stuck. Its Write must not be
	// acknowledged before an fsync has covered it.
	second := make(chan error, 1)
	go func() { second <- w.Write(Stdout, []byte("b"), 0) }()
	select {
	case err := <-second:
		t.Fatalf("strict Write returned (%v) while an fsync was in flight and its own was not done", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	for i, ch := range []chan error{first, second} {
		if err := <-ch; err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
}

// A timer that fires while a group sync pass is still running (a stalled
// fsync) re-arms instead of starting a second, blocked pass, and the re-armed
// timer really runs once the pass ends: dirty writers keep being synced.
func TestGroupSyncTimerDoesNotStackPasses(t *testing.T) {
	const interval = 50 * time.Millisecond
	s := newGroupSyncStore(t, interval, 0)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close("run") })
	s.flushMu.Lock() // a pass in progress
	if err := w.Write(Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	// The timer fires (several times) during the pass and must re-arm.
	time.Sleep(4 * interval)
	deadline := time.Now().Add(5 * time.Second)
	if w.syncs.Load() != 0 {
		t.Fatal("synced while a pass held the lock")
	}
	s.flushMu.Unlock()
	for w.syncs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(interval)
	}
	if w.syncs.Load() != 1 {
		t.Fatal("dirty writer never synced after the blocked pass ended")
	}
	// Later writes still arm a working timer.
	if err := w.Write(Stdout, []byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	for w.syncs.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(interval)
	}
	if w.syncs.Load() != 2 {
		t.Fatal("group sync timer stopped working")
	}
}

// A seal whose index cannot be written still queues its chunk for the group
// sync, so the chunk is not left unsynced.
func TestSealWithFailedIndexStillQueuesSync(t *testing.T) {
	var synced []string
	s := newGroupSyncStore(t, time.Hour, 0)
	s.syncFile = func(f *os.File) error {
		hookMu.Lock()
		synced = append(synced, f.Name())
		hookMu.Unlock()
		return f.Sync()
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	// A directory where the index's temporary file goes makes writeIndex fail.
	if err := os.Mkdir(filepath.Join(w.dir, "index.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err == nil {
		t.Fatal("expected the index failure")
	}
	s.SyncAll()
	if want := filepath.Join(w.dir, "000001.zst"); !slices.Contains(synced, want) {
		t.Fatalf("chunk not synced after a failed index: %v", synced)
	}
}

// hookMu guards counters of test fsync hooks: a group sync pass calls the
// hook from several goroutines. Tests read the counters after the pass.
var hookMu sync.Mutex
