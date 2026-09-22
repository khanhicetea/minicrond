package logstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/model"
)

func TestReadByteBudgetAcrossTiers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		archive bool
		tail    bool
	}{
		{"archive_and_small_tail", true, true},
		{"files_and_small_tail", false, true},
		{"files_and_memory_tail", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newArchiveStore(t)
			if !tc.archive {
				s.AttachDB(nil)
			}
			w, err := s.Open("run", "job", model.KindJob, WriterOptions{MaxBytes: 100 << 20, MaxLine: 2 << 20})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close("run") })
			payload := bytes.Repeat([]byte("x"), 1<<20)
			for i := range 20 {
				if err := w.Write(Stdout, payload, 0); err != nil {
					t.Fatal(err)
				}
				if tc.archive && i == 17 {
					if err := w.Flush(s); err != nil {
						t.Fatal(err)
					}
				}
			}
			want := uint64(20)
			if tc.tail {
				if err := w.Write(Stdout, []byte("tail"), 0); err != nil {
					t.Fatal(err)
				}
				want++
			}
			var after uint64
			for {
				frames, err := s.Read("run", after, 5000)
				if err != nil {
					t.Fatal(err)
				}
				if len(frames) == 0 {
					break
				}
				size := 0
				for _, frame := range frames {
					if frame.Sequence != after+1 {
						t.Fatalf("sequence %d followed by %d", after, frame.Sequence)
					}
					after = frame.Sequence
					size += 24 + len(frame.Payload)
				}
				if size > 16<<20 {
					t.Fatalf("page uses %d bytes, exceeds 16 MiB", size)
				}
			}
			if after != want {
				t.Fatalf("last sequence = %d, want %d", after, want)
			}
		})
	}
}

func TestReadMaximumFrameMakesProgress(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{MaxLine: maxFramePayload})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, bytes.Repeat([]byte("x"), maxFramePayload), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("tail"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	for after := range uint64(2) {
		frames, err := s.Read("run", after, 5000)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) != 1 || frames[0].Sequence != after+1 {
			t.Fatalf("after %d: expected exactly frame %d, got %d frames", after, after+1, len(frames))
		}
	}
}

func TestArchivalReleasesBufferCapacity(t *testing.T) {
	for _, dropNew := range []bool{false, true} {
		t.Run(fmt.Sprintf("drop_new=%v", dropNew), func(t *testing.T) {
			s, _, _ := newArchiveStore(t)
			w, err := s.Open("run", "worker", model.KindWorker, WriterOptions{MaxBytes: 28, MaxLine: 1024, DropNew: dropNew})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close("run") })
			for i := range 3 {
				if err := w.Write(Stdout, []byte("abcd"), 0); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(s); err != nil {
					t.Fatal(err)
				}
				if w.buffered != 0 || len(w.idx.Chunks) != 0 || len(w.history) != 0 {
					t.Fatalf("archived buffer retained accounting: bytes=%d chunks=%d tail=%d", w.buffered, len(w.idx.Chunks), len(w.history))
				}
				total, truncated := w.Stats()
				if total != int64((i+1)*28) || truncated {
					t.Fatalf("stats = %d/%v", total, truncated)
				}
			}
			frames, err := s.Read("run", 0, 100)
			if err != nil || len(frames) != 3 {
				t.Fatalf("got %d frames, error %v; want 3", len(frames), err)
			}
		})
	}
}

// Pausing a context check keeps a read in flight without timing sleeps.
type pausedReadContext struct {
	context.Context
	once    sync.Once
	started chan struct{}
	resume  chan struct{}
}

func (c *pausedReadContext) Err() error {
	c.once.Do(func() {
		close(c.started)
		<-c.resume
	})
	return c.Context.Err()
}

func TestReadPinsRunDuringCheckpoint(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run", "worker", model.KindWorker, WriterOptions{MaxLine: 2 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close("run") })
	for range 20 {
		if err := w.Write(Stdout, bytes.Repeat([]byte("x"), 1<<20), 0); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &pausedReadContext{Context: t.Context(), started: make(chan struct{}), resume: make(chan struct{})}
	type result struct {
		frames []Frame
		err    error
	}
	read := make(chan result, 1)
	go func() {
		frames, err := s.ReadContext(ctx, "run", 0, 5000)
		read <- result{frames, err}
	}()
	<-ctx.started
	s.mu.Lock()
	guard := s.runLocks["run"]
	s.mu.Unlock()
	pinned := guard != nil
	if pinned && guard.mu.TryLock() {
		guard.mu.Unlock()
		pinned = false
	}
	flushed := make(chan error, 1)
	go func() { flushed <- w.Flush(s) }()
	close(ctx.resume)
	got := <-read
	flushErr := <-flushed
	if !pinned {
		t.Fatal("read did not hold the run layout against migration")
	}
	if got.err != nil || flushErr != nil {
		t.Fatalf("read error %v, flush error %v", got.err, flushErr)
	}
	var last uint64
	for _, f := range got.frames {
		if f.Sequence != last+1 {
			t.Fatalf("sequence %d followed by %d", last, f.Sequence)
		}
		last = f.Sequence
	}
	frames, err := s.Read("run", last, 5000)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		if f.Sequence != last+1 {
			t.Fatalf("next page: sequence %d followed by %d", last, f.Sequence)
		}
		last = f.Sequence
	}
	if last != 20 {
		t.Fatalf("last sequence = %d, want 20", last)
	}
	if len(s.runLocks) != 0 {
		t.Fatal("idle run locks were not released")
	}
}

func TestFailedArchiveRetainsCapacityUntilRetry(t *testing.T) {
	s, db, dir := newArchiveStore(t)
	w, err := s.Open("run", "worker", model.KindWorker, WriterOptions{MaxBytes: 28, DropNew: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("abcd"), 0); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(s); err == nil {
		t.Fatal("expected archive failure")
	}
	if w.buffered != 28 {
		t.Fatalf("failed archive released capacity: %d", w.buffered)
	}
	reopened, err := logdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	s.AttachDB(reopened)
	if err := w.Flush(s); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("efgh"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 100)
	if err != nil || len(frames) != 2 {
		t.Fatalf("got %d frames, error %v", len(frames), err)
	}
}

func TestFailedFinalArchiveRetriedWithoutRestart(t *testing.T) {
	s, db, dir := newArchiveStore(t)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("retained"), 0); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err == nil {
		t.Fatal("expected final archive failure")
	}
	if s.Active("run") != nil {
		t.Fatal("closed writer still active")
	}
	buffer := filepath.Join(dir, "logs", "run")
	if _, err := os.Stat(buffer); err != nil {
		t.Fatal("failed archive lost buffer:", err)
	}
	reopened, err := logdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	s.AttachDB(reopened)
	// Same Store instance and maintenance operations as the daemon loop.
	s.FlushActive()
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(buffer); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("buffer remains after retry: %v", err)
	}
	frames, err := s.Read("run", 0, 100)
	if err != nil || len(frames) != 1 || string(frames[0].Payload) != "retained" {
		t.Fatalf("retry lost output: %d frames, error %v", len(frames), err)
	}
}

func TestArchiveBatchBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		count   int
		payload int
	}{
		{"chunk_count", archiveBatchChunks + 2, 1},
		{"blob_bytes", 12, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db, dir := newArchiveStore(t)
			buffer := filepath.Join(dir, "logs", "run")
			if err := os.Mkdir(buffer, 0o700); err != nil {
				t.Fatal(err)
			}
			w := &Writer{store: s, runID: "run", job: "job", kind: model.KindJob, dir: buffer}
			rng := rand.NewChaCha8([32]byte{1})
			payload := make([]byte, tc.payload)
			if _, err := rng.Read(payload); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= tc.count; i++ {
				blob, err := encodeFrames([]Frame{{Sequence: uint64(i), Timestamp: time.Now(), Stream: Stdout, Payload: payload}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(w.chunkPath(i), blob, 0o600); err != nil {
					t.Fatal(err)
				}
				raw := int64(24 + len(payload))
				w.idx.Chunks = append(w.idx.Chunks, chunkMeta{Number: i, First: uint64(i), Last: uint64(i), Bytes: int64(len(blob)), Raw: raw})
				w.buffered += raw
			}
			s.archiveMu.Lock()
			unlock := s.lockRun("run", true)
			w.mu.Lock()
			more, err := s.archiveBatchLocked(w, tc.count)
			w.mu.Unlock()
			unlock()
			s.archiveMu.Unlock()
			if err != nil || !more {
				t.Fatalf("first batch: more=%v, error=%v", more, err)
			}
			_, chunks, size, err := db.Stats(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if chunks == 0 || chunks > archiveBatchChunks || size > archiveBatchBytes {
				t.Fatalf("unbounded batch: %d chunks / %d bytes", chunks, size)
			}
			// The remaining buffer is recoverable independently, even after
			// some committed batches have already removed their files.
			if err := s.ArchiveOrphans(); err != nil {
				t.Fatal(err)
			}
			frames, err := s.Read("run", 0, 5000)
			if err != nil || len(frames) != tc.count {
				t.Fatalf("recovery: got %d frames, error %v; want %d", len(frames), err, tc.count)
			}
		})
	}
}

func TestArchivePruneDoesNotResurrectMemoryTail(t *testing.T) {
	s, db, _ := newArchiveStore(t)
	w, err := s.Open("run", "worker", model.KindWorker, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close("run") })
	if err := w.Write(Stdout, []byte("old"), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(s); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Prune(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 100)
	if err != nil || len(frames) != 0 {
		t.Fatalf("pruned output reappeared: %d frames, error %v", len(frames), err)
	}
}

func TestChunkFilesUseNumericOrder(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"999999.zst", "1000000.zst"} {
		blob, err := encodeFrames([]Frame{{Sequence: uint64(i + 1), Stream: Stdout, Payload: []byte("x")}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	frames, err := s.Read("run", 0, 100)
	if err != nil || len(frames) != 2 || frames[0].Sequence != 1 || frames[1].Sequence != 2 {
		t.Fatalf("incorrect chunk order: %v, error %v", frames, err)
	}
}
