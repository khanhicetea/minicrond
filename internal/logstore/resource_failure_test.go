package logstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRotationOpenFailureCanRetryWithoutLosingAcceptedFrames(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", "job", WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close("run") })
	payload := bytes.Repeat([]byte("x"), 600<<10)
	if err := w.Write(Stdout, payload, 0); err != nil {
		t.Fatal(err)
	}
	blocked := w.chunkPath(2)
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, payload, 0); !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected conflicting chunk path error, got %v", err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, payload, 0); err != nil {
		t.Fatalf("rotation retry: %v", err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil || len(frames) != 2 {
		t.Fatalf("accepted frames: count %d, error %v", len(frames), err)
	}
	for i, frame := range frames {
		if frame.Sequence != uint64(i+1) || !bytes.Equal(frame.Payload, payload) {
			t.Fatalf("frame %d was lost or changed", i)
		}
	}
}

func TestRotationFinalizationFailureRemainsStable(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", "job", WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 600<<10)
	if err := w.Write(Stdout, payload, 0); err != nil {
		t.Fatal(err)
	}
	// The chunk can be sealed, but its index cannot be atomically installed.
	if err := os.Mkdir(filepath.Join(w.dir, "index.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	failure := w.Write(Stdout, payload, 0)
	if failure == nil {
		t.Fatal("expected index replacement failure")
	}
	if err := w.Write(Stdout, []byte("later"), 0); !errors.Is(err, failure) {
		t.Fatalf("write after finalization failure: %v", err)
	}
	if err := s.Close("run"); !errors.Is(err, failure) {
		t.Fatalf("close after finalization failure: %v", err)
	}
	if err := w.Close(); !errors.Is(err, failure) {
		t.Fatalf("repeated close changed the error: %v", err)
	}
	if _, err := os.Stat(w.chunkPath(1)); err != nil {
		t.Fatalf("accepted output was not preserved: %v", err)
	}
	if _, err := os.Stat(w.chunkPath(2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed replacement was not cleaned up: %v", err)
	}
}

func TestOrphanArchivePreservesBuffersWithInvalidIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid JSON", "{"},
		{"unsupported version", `{"version":2}`},
		{"invalid metadata", `{"version":1,"chunks":[{"number":1,"first":2,"last":1}]}`},
		{"duplicate chunks", `{"version":1,"chunks":[{"number":1,"first":1,"last":1},{"number":1,"first":1,"last":1}]}`},
		{"unreadable index", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db, dir := newArchiveStore(t)
			buffer := filepath.Join(dir, "logs", "run")
			if err := os.Mkdir(buffer, 0o700); err != nil {
				t.Fatal(err)
			}
			blob, err := encodeFrames([]Frame{{Sequence: 1, Stream: Stdout, Payload: []byte("preserve")}})
			if err != nil {
				t.Fatal(err)
			}
			chunk := filepath.Join(buffer, "000001.zst")
			if err := os.WriteFile(chunk, blob, 0o600); err != nil {
				t.Fatal(err)
			}
			indexPath := filepath.Join(buffer, "index.json")
			if tc.body == "" {
				err = os.Mkdir(indexPath, 0o700)
			} else {
				err = os.WriteFile(indexPath, []byte(tc.body), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ArchiveOrphans(); err == nil {
				t.Fatal("expected invalid index error")
			}
			preserved, err := os.ReadFile(chunk)
			if err != nil || !bytes.Equal(preserved, blob) {
				t.Fatalf("buffer changed after failed archive: %v", err)
			}
			_, chunks, _, err := db.Stats(t.Context())
			if err != nil || chunks != 0 {
				t.Fatalf("invalid archive committed %d chunks, error %v", chunks, err)
			}
			if err := os.Remove(indexPath); err != nil {
				t.Fatal(err)
			}
			if err := s.ArchiveOrphans(); err != nil {
				t.Fatalf("archive after index repair: %v", err)
			}
			frames, err := s.Read("run", 0, 10)
			if err != nil || len(frames) != 1 || string(frames[0].Payload) != "preserve" {
				t.Fatalf("recovered frames: %v, error %v", frames, err)
			}
		})
	}
}
