package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestOrphanArchiveResumesAfterPartialBatchFailure(t *testing.T) {
	s, db, dir := newArchiveStore(t)
	buffer := filepath.Join(dir, "logs", "run")
	if err := os.Mkdir(buffer, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= archiveBatchChunks+1; i++ {
		blob, err := encodeFrames([]Frame{{Sequence: uint64(i), Stream: Stdout, Payload: []byte("x")}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(buffer, fmt.Sprintf("%06d.zst", i)), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Inject a real transaction failure only after the first bounded batch.
	control, err := sql.Open("sqlite", filepath.Join(dir, "minicron-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_later_chunks BEFORE INSERT ON log_chunks
		WHEN NEW.number > %d BEGIN SELECT RAISE(ABORT, 'injected archive failure'); END`, archiveBatchChunks)
	if _, err := control.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveOrphans(); err == nil {
		t.Fatal("expected injected failure")
	}
	_, chunks, _, err := db.Stats(t.Context())
	if err != nil || chunks != archiveBatchChunks {
		t.Fatalf("committed prefix = %d chunks, error %v", chunks, err)
	}
	files, err := chunkFiles(buffer)
	if err != nil || len(files) != 1 || files[0].number != archiveBatchChunks+1 {
		t.Fatalf("uncommitted files not preserved: %v, error %v", files, err)
	}
	if _, err := control.ExecContext(t.Context(), "DROP TRIGGER reject_later_chunks"); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 5000)
	if err != nil || len(frames) != archiveBatchChunks+1 {
		t.Fatalf("retry: %d frames, error %v", len(frames), err)
	}
	for i, frame := range frames {
		if frame.Sequence != uint64(i+1) {
			t.Fatalf("frame %d has sequence %d", i, frame.Sequence)
		}
	}
}

func TestConcurrentWritesReadsFlushesAndClose(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run", "worker", "worker", WriterOptions{MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var writeErr error
	const count = 200
	go func() {
		defer close(done)
		writeErr = func() error {
			for i := range count {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := w.Write(Stdout, []byte("output"), 0); err != nil {
					return err
				}
				if i%5 == 0 {
					if err := w.Flush(s); err != nil {
						return err
					}
				}
			}
			return s.Close("run")
		}()
	}()
	t.Cleanup(func() { cancel(); <-done; s.Close("run") })
	var after uint64
	finished := false
	for after < count {
		frames, err := s.ReadContext(ctx, "run", after, 7)
		if err != nil {
			t.Fatal(err)
		}
		if finished && len(frames) == 0 {
			t.Fatalf("completed run ended at sequence %d, want %d", after, count)
		}
		for _, frame := range frames {
			if frame.Sequence != after+1 {
				t.Fatalf("sequence %d followed by %d", after, frame.Sequence)
			}
			after = frame.Sequence
		}
		select {
		case <-done:
			finished = true
			if writeErr != nil {
				t.Fatal(writeErr)
			}
		default:
		}
	}
	<-done
	if writeErr != nil {
		t.Fatal(writeErr)
	}
}
