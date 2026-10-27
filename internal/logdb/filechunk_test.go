package logdb

import (
	"errors"
	"os"
	"testing"
	"time"
)

// putFileChunk stores a file chunk row and writes its archive file.
func putFileChunk(t *testing.T, l *LogDB, run string, number int, size int, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(l.RunFilesDir(run), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.ChunkPath(run, number), randomBlob(t, size), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Chunk{Number: number, First: uint64(number), Last: uint64(number), RawBytes: int64(size), FileBytes: int64(size)}
	if err := l.PutChunks(t.Context(), run, "job", "job", at, []Chunk{c}); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// ADR-20: a file chunk row has no blob; reads see its size and the archive
// accounting counts its file.
func TestFileChunkRowsAndAccounting(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	putFileChunk(t, l, "big", 1, 5000, time.Now())
	if err := l.PutChunks(t.Context(), "big", "job", "job", time.Now(), []Chunk{chunk(2, 2, 2, []byte("tail"))}); err != nil {
		t.Fatal(err)
	}
	var got []Chunk
	if err := l.EachChunk(t.Context(), "big", 0, func(c Chunk) (bool, error) { got = append(got, c); return true, nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].FileBytes != 5000 || len(got[0].Blob) != 0 || got[1].FileBytes != 0 || string(got[1].Blob) != "tail" {
		t.Fatalf("chunks = %+v", got)
	}
	files, err := l.FileChunks(t.Context(), "big")
	if err != nil || len(files) != 1 || !files[1] {
		t.Fatalf("FileChunks = %v, %v", files, err)
	}
	if _, _, bytes, err := l.Stats(t.Context()); err != nil || bytes != 5004 {
		t.Fatalf("Stats bytes = %d, %v; want blob and file bytes", bytes, err)
	}
	if n := l.ChunkFileBytes(); n != 5000 {
		t.Fatalf("ChunkFileBytes = %d", n)
	}
}

// Every deletion path removes the files of the rows it deletes.
func TestDeletionsRemoveArchivedFiles(t *testing.T) {
	old, recent := time.Now().Add(-48*time.Hour), time.Now()
	t.Run("prune_by_age", func(t *testing.T) {
		l, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		putFileChunk(t, l, "old", 1, 100, old)
		putFileChunk(t, l, "new", 1, 100, recent)
		if _, err := l.Prune(t.Context(), time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if exists(t, l.RunFilesDir("old")) || !exists(t, l.ChunkPath("new", 1)) {
			t.Fatal("age prune did not remove exactly the expired file")
		}
	})
	t.Run("prune_oldest", func(t *testing.T) {
		l, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		putFileChunk(t, l, "old", 1, 4000, old)
		putFileChunk(t, l, "new", 1, 4000, recent)
		res, err := l.PruneOldest(t.Context(), 3000, nil)
		if err != nil || res.Chunks != 1 || res.Bytes != 4000 {
			t.Fatalf("PruneOldest = %+v, %v", res, err)
		}
		if exists(t, l.ChunkPath("old", 1)) || !exists(t, l.ChunkPath("new", 1)) {
			t.Fatal("pressure prune did not remove exactly the oldest file")
		}
	})
	t.Run("prune_to_size_counts_files", func(t *testing.T) {
		l, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		putFileChunk(t, l, "old", 1, 1<<20, old)
		putFileChunk(t, l, "new", 1, 1<<20, recent)
		used, err := l.usedBytes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// The database alone fits; with both files it does not.
		if _, err := l.PruneToSize(t.Context(), used+(1<<20)+(1<<19)); err != nil {
			t.Fatal(err)
		}
		if exists(t, l.ChunkPath("old", 1)) || !exists(t, l.ChunkPath("new", 1)) {
			t.Fatal("size prune ignored archived files")
		}
	})
	t.Run("delete_runs", func(t *testing.T) {
		l, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		putFileChunk(t, l, "a", 1, 100, recent)
		putFileChunk(t, l, "a", 2, 100, recent)
		putFileChunk(t, l, "b", 1, 100, recent)
		if err := l.DeleteRuns(t.Context(), []string{"a"}); err != nil {
			t.Fatal(err)
		}
		if exists(t, l.RunFilesDir("a")) || !exists(t, l.ChunkPath("b", 1)) {
			t.Fatal("run deletion did not remove exactly the run's files")
		}
	})
}
