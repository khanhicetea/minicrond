package logstore

import (
	"bytes"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func sealSmallRun(t *testing.T, s *Store, id string, lines int) {
	t.Helper()
	w, err := s.Open(id, "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader(numberedLines(id, lines))); err != nil {
		t.Fatal(err)
	}
	if err := s.Seal(id); err != nil {
		t.Fatal(err)
	}
}

// Short runs sealed within one sync interval wait for the group sync pass,
// which has them archived together before it syncs anything; their buffers
// are removed without ever being fsynced.
func TestQueuedRunsWaitForTheGroupSyncPass(t *testing.T) {
	s, ldb, dir := newArchiveStore(t)
	s.SetGroupSync(300*time.Millisecond, 0)
	var fsyncs atomic.Int64
	s.syncFile = func(f *os.File) error { fsyncs.Add(1); return f.Sync() }
	s.StartArchiver()
	t.Cleanup(func() { _ = s.StopArchiver(t.Context()) })
	const runs = 5
	for i := range runs {
		id := fmt.Sprintf("run-%d", i)
		sealSmallRun(t, s, id, 3)
	}
	if n := s.ArchiveBacklog(); n != runs {
		t.Fatalf("archive backlog = %d right after sealing, want %d runs waiting for the pass", n, runs)
	}
	for i := range runs {
		id := fmt.Sprintf("run-%d", i)
		waitArchived(t, dir, id)
		if chunks := archivedChunks(t, ldb, id); len(chunks) != 1 {
			t.Fatalf("run %s: %d archived chunks, want 1", id, len(chunks))
		}
		checkSequence(t, readAll(t, s, id), 3)
	}
	if n := s.archivedFromImage.Load(); n != runs {
		t.Fatalf("archived from image %d times, want %d", n, runs)
	}
	s.flushDirty() // whatever the pass still holds
	if n := fsyncs.Load(); n != 0 {
		t.Fatalf("%d buffer fsyncs for runs the archiver removed first, want 0", n)
	}
	s.syncMu.Lock()
	pending, dirty := len(s.sealed)+len(s.unsyncedSeals), len(s.dirty)
	s.syncMu.Unlock()
	if pending != 0 || dirty != 0 {
		t.Fatalf("%d deferred seals and %d dirty writers left for archived runs", pending, dirty)
	}
	if len(s.runLocks) != 0 {
		t.Fatalf("%d run locks left after the runs were archived", len(s.runLocks))
	}
}
