package logstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func manyLines(n int) []byte {
	var b bytes.Buffer
	for i := range n {
		fmt.Fprintf(&b, "line %06d\n", i)
	}
	return b.Bytes()
}

// A chatty child must not be paced by one fsync per line.
func TestPipeGroupsSyncsWhileInputIsBuffered(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const lines = 20000
	if err := w.Pipe(Stdout, bytes.NewReader(manyLines(lines))); err != nil {
		t.Fatal(err)
	}
	if syncs := w.syncs.Load(); syncs > lines/100 {
		t.Fatalf("%d fsyncs for %d buffered lines, want them grouped", syncs, lines)
	}
	w.mu.Lock()
	unsynced := w.unsynced
	w.mu.Unlock()
	if unsynced != 0 {
		t.Fatalf("%d bytes left unsynced after the pipe closed", unsynced)
	}
	frames, err := s.Read("run", 0, 5000)
	if err != nil || len(frames) != 5000 || frames[0].Sequence != 1 {
		t.Fatalf("got %d frames, error %v", len(frames), err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err = s.Read("run", lines-1, 10)
	if err != nil || len(frames) != 1 || string(frames[0].Payload) != fmt.Sprintf("line %06d", lines-1) {
		t.Fatalf("last frame %v, error %v", frames, err)
	}
}

// Output is synced before the pump waits for more, so an idle run never
// holds unsynced lines.
func TestPipeSyncsBeforeWaitingForInput(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- w.Pipe(Stdout, r) }()
	if _, err := pw.Write([]byte("one\ntwo\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		seq, unsynced := w.seq, w.unsynced
		w.mu.Unlock()
		if seq == 2 && unsynced == 0 && w.syncs.Load() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seq %d with %d unsynced bytes while the pipe is idle", seq, unsynced)
		}
		time.Sleep(time.Millisecond)
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFrameDurabilitySyncsEveryLine(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.SetFrameSync(true)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const lines = 200
	if err := w.Pipe(Stdout, bytes.NewReader(manyLines(lines))); err != nil {
		t.Fatal(err)
	}
	if syncs := w.syncs.Load(); syncs < lines {
		t.Fatalf("%d fsyncs for %d lines in frame mode", syncs, lines)
	}
}

func stopArchiver(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := s.StopArchiver(ctx); err != nil {
		t.Fatal(err)
	}
}

// Completing a run must not wait for the archive, even when every archive
// slot is busy with other runs.
func TestSealDoesNotWaitForArchival(t *testing.T) {
	s, ldb, dir := newArchiveStore(t)
	s.StartArchiver()
	for range cap(s.archiveSlots) {
		s.archiveSlots <- struct{}{}
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("a\nb\n"))); err != nil {
		t.Fatal(err)
	}
	sealed := make(chan error, 1)
	go func() { sealed <- s.Seal("run") }()
	select {
	case err := <-sealed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Seal waited for a busy archiver")
	}
	if s.Active("run") != nil {
		t.Fatal("sealed writer is still registered")
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil || len(frames) != 2 {
		t.Fatalf("sealed log before archival: %d frames, error %v", len(frames), err)
	}
	for range cap(s.archiveSlots) {
		<-s.archiveSlots
	}
	stopArchiver(t, s)
	runs, chunks, _, err := ldb.Stats(t.Context())
	if err != nil || runs != 1 || chunks != 1 {
		t.Fatalf("archive has %d runs, %d chunks, error %v", runs, chunks, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "run")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("buffer directory must be removed after background archival")
	}
	frames, err = s.Read("run", 0, 10)
	if err != nil || len(frames) != 2 {
		t.Fatalf("archived log: %d frames, error %v", len(frames), err)
	}
}

func TestSealWithoutArchiverArchivesInline(t *testing.T) {
	s, ldb, _ := newArchiveStore(t)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("a"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Seal("run"); err != nil {
		t.Fatal(err)
	}
	if runs, _, _, err := ldb.Stats(t.Context()); err != nil || runs != 1 {
		t.Fatalf("archive has %d runs, error %v", runs, err)
	}
}

// A run sealed while a worker checkpoint owns it is archived once the
// checkpoint finishes, not left for the next orphan sweep.
func TestSealedRunDeferredWhileOwnedIsArchivedAfterRelease(t *testing.T) {
	s, ldb, _ := newArchiveStore(t)
	s.StartArchiver()
	w, err := s.Open("run", "worker", model.KindWorker, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("a"), 0); err != nil {
		t.Fatal(err)
	}
	if !s.claim("run") {
		t.Fatal("claim failed")
	}
	if err := s.Seal("run"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		deferred := s.deferred["run"]
		s.mu.Unlock()
		if deferred {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("archiver did not defer the owned run")
		}
		time.Sleep(time.Millisecond)
	}
	s.release("run")
	stopArchiver(t, s)
	if runs, _, _, err := ldb.Stats(t.Context()); err != nil || runs != 1 {
		t.Fatalf("archive has %d runs, error %v", runs, err)
	}
}

func TestDeleteRunsSkipsRunBeingArchived(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("a"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	if !s.claim("run") {
		t.Fatal("claim failed")
	}
	deleted, err := s.DeleteRuns([]string{"run"})
	if err == nil || len(deleted) != 0 {
		t.Fatalf("deleted %v with error %v while archival owned the run", deleted, err)
	}
	s.release("run")
	deleted, err = s.DeleteRuns([]string{"run"})
	if err != nil || len(deleted) != 1 {
		t.Fatalf("deleted %v, error %v", deleted, err)
	}
}
