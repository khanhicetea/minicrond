package logstore

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// A burst below the dirty-byte limit costs one fsync at EOF, whatever the line
// count; a smaller limit bounds the unsynced window by bytes.
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
	if syncs := w.syncs.Load(); syncs != 1 {
		t.Fatalf("burst under the dirty limit took %d fsyncs, want only the EOF sync", syncs)
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

// EOF, seal, and orderly shutdown always sync what is dirty.
func TestFinalSyncOnEOFSealAndShutdown(t *testing.T) {
	var syncs int
	s := newGroupSyncStore(t, time.Hour, 0)
	s.syncFile = func(f *os.File) error { syncs++; return f.Sync() }

	w, err := s.Open("eof", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("a\nb\n"))); err != nil {
		t.Fatal(err)
	}
	if syncs != 1 {
		t.Fatalf("pipe EOF: %d syncs, want 1", syncs)
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

	// Sealing syncs the chunk itself (finishChunk), so the group timer has
	// nothing left to do for a closed writer.
	w3, err := s.Open("seal", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w3.Write(Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("seal"); err != nil {
		t.Fatal(err)
	}
	s.SyncAll()
	if w3.syncs.Load() != 0 {
		t.Fatal("closed writer was synced twice")
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
