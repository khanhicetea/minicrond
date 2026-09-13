package logstore

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/model"
)

func TestFramesSurviveChunkStorage(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "test", "job", WriterOptions{MaxBytes: 1 << 20, MaxLine: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("hello\npartial"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames", len(frames))
	}
	if string(frames[0].Payload) != "hell" || frames[0].Flags&FlagTruncated == 0 {
		t.Fatalf("first frame = %#v", frames[0])
	}
	if frames[1].Flags&FlagPartial == 0 || frames[1].Sequence != 2 {
		t.Fatalf("second frame = %#v", frames[1])
	}
}

func TestBacklogToLiveSubscriptionHasStableSequence(t *testing.T) {
	s, _ := New(t.TempDir())
	w, _ := s.Open("run", "test", "job", WriterOptions{MaxBytes: 1 << 20, MaxLine: 1024})
	if err := w.Write(Stdout, []byte("backlog"), 0); err != nil {
		t.Fatal(err)
	}
	live, _, unsubscribe := w.Subscribe(0)
	defer unsubscribe()
	if err := w.Write(Stderr, []byte("handoff"), 0); err != nil {
		t.Fatal(err)
	}
	backlog, err := s.Read("run", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog) != 2 || backlog[0].Sequence != 1 || backlog[1].Sequence != 2 {
		t.Fatalf("active backlog = %v", backlog)
	}
	select {
	case frame := <-live:
		if frame.Sequence != 2 {
			t.Fatalf("live sequence = %d", frame.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("live frame lost")
	}
}

func TestInvalidUTF8IsFlaggedAndPreserved(t *testing.T) {
	s, _ := New(t.TempDir())
	w, _ := s.Open("run", "test", "job", WriterOptions{MaxBytes: 1024, MaxLine: 1024})
	payload := []byte{0xff, '\n'}
	if err := w.Pipe(Stderr, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Flags&FlagInvalidUTF8 == 0 || !bytes.Equal(frames[0].Payload, []byte{0xff}) {
		t.Fatalf("frame = %#v", frames)
	}
}

func newArchiveStore(t *testing.T) (*Store, *logdb.LogDB, string) {
	t.Helper()
	dir := t.TempDir()
	ldb, err := logdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ldb.Close() })
	s, err := New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	s.AttachDB(ldb)
	return s, ldb, dir
}

// Job logs: the file buffer is the hot path; once the run finishes the whole
// log moves into the SQLite archive and the buffer directory disappears.
func TestJobLogArchivedIntoDatabaseOnClose(t *testing.T) {
	s, ldb, dir := newArchiveStore(t)
	w, err := s.Open("run1", "hello", model.KindJob, WriterOptions{MaxBytes: 1 << 20, MaxLine: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Pipe(Stdout, bytes.NewReader([]byte("line one\nline two\n"))); err != nil {
		t.Fatal(err)
	}
	if err = s.Close("run1"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "logs", "run1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("buffer directory must be removed after archival")
	}
	runs, chunks, _, err := ldb.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 || chunks != 1 {
		t.Fatalf("stats = %d runs, %d chunks", runs, chunks)
	}
	frames, err := s.Read("run1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || string(frames[0].Payload) != "line one" || string(frames[1].Payload) != "line two" {
		t.Fatalf("archived frames = %#v", frames)
	}
	var raw bytes.Buffer
	if err = s.Raw("run1", &raw); err != nil {
		t.Fatal(err)
	}
	if raw.String() != "line one\nline two\n" {
		t.Fatalf("raw = %q", raw.String())
	}
}

// Worker logs: a long-running run is flushed in rounds; frames keep flowing
// across the archive boundary with a stable, gap-free sequence.
func TestWorkerLogFlushRoundsKeepSequenceStable(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run1", "worker", model.KindWorker, WriterOptions{MaxBytes: 1 << 30, MaxLine: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err = w.Write(Stdout, []byte{byte('a' + i)}, 0); err != nil {
			t.Fatal(err)
		}
		s.FlushActive()
		frames, err := s.Read("run1", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) != i+1 {
			t.Fatalf("after flush %d: %d frames", i, len(frames))
		}
		for j, f := range frames {
			if f.Sequence != uint64(j+1) {
				t.Fatalf("frame %d has sequence %d", j, f.Sequence)
			}
		}
	}
	// Unflushed tail still lands in the archive when the run finishes.
	if err = w.Write(Stdout, []byte("tail"), 0); err != nil {
		t.Fatal(err)
	}
	if err = s.Close("run1"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4 || string(frames[3].Payload) != "tail" {
		t.Fatalf("final frames = %#v", frames)
	}
}

// Pagination across the archive/file/memory tiers must not skip or repeat.
func TestReadAcrossTiersPaginates(t *testing.T) {
	s, ldb, _ := newArchiveStore(t)
	w, _ := s.Open("run1", "job", model.KindJob, WriterOptions{MaxBytes: 1 << 30, MaxLine: 1024})
	for i := range 10 {
		if err := w.Write(Stdout, []byte{byte('0' + i)}, 0); err != nil {
			t.Fatal(err)
		}
		if i < 6 {
			s.FlushActive() // frames 1..7 end up in the database
		}
	}
	var got []byte
	var after uint64
	for {
		frames, err := s.Read("run1", after, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) == 0 {
			break
		}
		for _, f := range frames {
			got = append(got, f.Payload...)
			if f.Sequence <= after {
				t.Fatalf("sequence went backwards: %d after %d", f.Sequence, after)
			}
			after = f.Sequence
		}
	}
	if string(got) != "0123456789" {
		t.Fatalf("paginated payload = %q", got)
	}
	// Sanity: the archive really holds the early chunks.
	var archived int
	err := ldb.EachChunk(t.Context(), "run1", 0, func(logdb.Chunk) (bool, error) {
		archived++
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if archived == 0 {
		t.Fatal("expected archived chunks")
	}
}

// A crash can leave sealed-but-unarchived buffers behind; the startup sweep
// archives the sealed chunk verbatim and salvages the torn in-progress chunk
// up to the last intact frame.
func TestOrphanBuffersAreSalvagedIntoArchive(t *testing.T) {
	dir := t.TempDir()
	buffer, err := New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := buffer.Open("crashed", "hello", model.KindJob, WriterOptions{MaxBytes: 1 << 30, MaxLine: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// ~1.2 MiB of frames forces a rotation: chunk 1 is sealed and indexed,
	// chunk 2 is the in-progress one when the crash hits.
	line := bytes.Repeat([]byte("x"), 30000)
	for range 40 {
		if err = w.Write(Stdout, line, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the crash: writer never closes, files stay on disk.
	buffer.mu.Lock()
	delete(buffer.writers, "crashed")
	buffer.mu.Unlock()

	files, err := filepath.Glob(filepath.Join(dir, "logs", "crashed", "*.zst"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	if len(files) < 2 {
		t.Fatalf("expected at least 2 chunks, got %v", files)
	}
	torn := files[len(files)-1]
	info, err := os.Stat(torn)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(torn, info.Size()/2); err != nil {
		t.Fatal(err)
	}

	s, ldb, _ := newArchiveStore(t)
	s.root = filepath.Join(dir, "logs")
	if err = s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("crashed", 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 30 {
		t.Fatalf("salvaged only %d frames", len(frames))
	}
	for i, f := range frames {
		if f.Sequence != uint64(i+1) {
			t.Fatalf("sequence %d at position %d", f.Sequence, i)
		}
		if len(f.Payload) != 30000 {
			t.Fatalf("frame %d payload %d bytes", i, len(f.Payload))
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "logs", "crashed")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("orphan directory must be removed after archival")
	}
	runs, _, _, err := ldb.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("archive holds %d runs", runs)
	}
}

// Per-run deletion must clear both tiers (retention path).
func TestDeleteRemovesArchiveAndFiles(t *testing.T) {
	s, ldb, dir := newArchiveStore(t)
	w, _ := s.Open("run1", "job", model.KindJob, WriterOptions{MaxBytes: 1 << 20, MaxLine: 1024})
	_ = w.Write(Stdout, []byte("x"), 0)
	if err := s.Close("run1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("run1"); err != nil {
		t.Fatal(err)
	}
	runs, _, _, err := ldb.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("archive still holds %d runs", runs)
	}
	if _, err = os.Stat(filepath.Join(dir, "logs", "run1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("buffer directory still present")
	}
	frames, err := s.Read("run1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("deleted run still readable: %#v", frames)
	}
}

// The drop_old ring buffer keeps working with an attached archive: writing
// past log_max evicts from the accounting and stays bounded.
func TestRingBufferStillEnforcesMaxBytesWithArchive(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run1", "job", model.KindJob, WriterOptions{MaxBytes: 24 + 4, MaxLine: 1024}) // one small frame
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err = w.Write(Stdout, []byte("abcd"), 0); err != nil {
			t.Fatal(err)
		}
	}
	total, truncated := w.Stats()
	if total != 28 || !truncated {
		t.Fatalf("total = %d truncated = %v", total, truncated)
	}
	if err = s.Close("run1"); err != nil {
		t.Fatal(err)
	}
}

// ParseBytes must resolve suffixes deterministically: "10KiB" used to match
// the bare "B" suffix whenever map iteration ordered it first, silently
// discarding the intended size.
func TestParseBytesIsDeterministic(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"10KiB", 10 << 10},
		{"10MiB", 10 << 20},
		{"10GiB", 10 << 30},
		{"512B", 512},
		{"512", 512},
		{" 1MiB ", 1 << 20},
	} {
		got, err := ParseBytes(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	for _, invalid := range []string{"10Ki", "xB", ""} {
		if got, err := ParseBytes(invalid); err == nil || got != 0 {
			t.Errorf("ParseBytes(%q) = %d, %v; want 0, error", invalid, got, err)
		}
	}
}

// log_on_full = drop_new keeps the retained history and refuses new frames
// once log_max is reached, instead of evicting old chunks.
func TestDropNewKeepsHistoryWhenFull(t *testing.T) {
	s, _ := New(t.TempDir())
	w, err := s.Open("run", "test", model.KindJob, WriterOptions{MaxBytes: 24 + 4, MaxLine: 1024, DropNew: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := w.Write(Stdout, []byte("abcd"), 0); err != nil {
			t.Fatal(err)
		}
	}
	total, truncated := w.Stats()
	if total != 28 || !truncated {
		t.Fatalf("total = %d truncated = %v; want 28, true", total, truncated)
	}
	frames, err := s.Read("run", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || string(frames[0].Payload) != "abcd" {
		t.Fatalf("drop_new must keep the first frame, got %#v", frames)
	}
}

// The in-memory tail is bounded: writing far past historyLimit must not grow
// the snapshot window (and must not renumber sequences).
func TestHistoryTailStaysBounded(t *testing.T) {
	s, _ := New(t.TempDir())
	w, err := s.Open("run", "test", model.KindJob, WriterOptions{MaxBytes: 1 << 40, MaxLine: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for i := range historyLimit*3 + 100 {
		if err := w.Write(Stdout, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
		if i%997 == 0 { // spot-check the bound while writing
			if got := len(w.Snapshot(0, 1<<30)); got > historyLimit+historyLimit/4 {
				t.Fatalf("history grew to %d during writes", got)
			}
		}
	}
	frames := w.Snapshot(0, 1<<30)
	if len(frames) > historyLimit+historyLimit/4 {
		t.Fatalf("history tail = %d, want <= %d", len(frames), historyLimit+historyLimit/4)
	}
	last := frames[len(frames)-1]
	if last.Sequence != uint64(historyLimit*3+100) {
		t.Fatalf("last sequence = %d", last.Sequence)
	}
}
