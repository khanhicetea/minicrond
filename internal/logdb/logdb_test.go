package logdb

import (
	"bytes"
	"testing"
	"time"
)

func chunk(number int, first, last uint64, blob []byte) Chunk {
	return Chunk{Number: number, First: first, Last: last, RawBytes: int64(len(blob)), Blob: blob}
}

func TestPutChunksIsIdempotentAcrossRetries(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := t.Context()
	at := time.Now()
	if err = l.PutChunks(ctx, "run", "job", "job", at, []Chunk{chunk(1, 1, 2, []byte("a")), chunk(2, 3, 4, []byte("b"))}); err != nil {
		t.Fatal(err)
	}
	// A crash between the database write and the file cleanup replays the
	// same chunks plus new ones; upsert must not duplicate rows.
	if err = l.PutChunks(ctx, "run", "job", "job", at, []Chunk{chunk(1, 1, 2, []byte("a")), chunk(2, 3, 4, []byte("b")), chunk(3, 5, 6, []byte("c"))}); err != nil {
		t.Fatal(err)
	}
	var numbers []int
	err = l.EachChunk(ctx, "run", 0, func(c Chunk) (bool, error) {
		numbers = append(numbers, c.Number)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(numbers) != 3 {
		t.Fatalf("chunk numbers = %v", numbers)
	}
	runs, chunks, blobBytes, err := l.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 || chunks != 3 || blobBytes != 3 {
		t.Fatalf("stats = %d/%d/%d", runs, chunks, blobBytes)
	}
}

func TestEachChunkSkipsChunksBelowAfter(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := t.Context()
	if err = l.PutChunks(ctx, "run", "job", "job", time.Now(), []Chunk{chunk(1, 1, 10, []byte("first")), chunk(2, 11, 20, []byte("second")), chunk(3, 21, 30, []byte("third"))}); err != nil {
		t.Fatal(err)
	}
	var seen []byte
	err = l.EachChunk(ctx, "run", 10, func(c Chunk) (bool, error) {
		seen = append(seen, c.Blob...)
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seen, []byte("secondthird")) {
		t.Fatalf("seen = %q", seen)
	}
	// Stopping early must not error.
	stopped := false
	err = l.EachChunk(ctx, "run", 0, func(Chunk) (bool, error) {
		stopped = true
		return false, nil
	})
	if err != nil || !stopped {
		t.Fatalf("early stop: stopped=%v err=%v", stopped, err)
	}
}

func TestDeleteRunAndPrune(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := t.Context()
	now := time.Now()
	if err = l.PutChunks(ctx, "old", "a", "job", now.Add(-48*time.Hour), []Chunk{chunk(1, 1, 2, []byte("x"))}); err != nil {
		t.Fatal(err)
	}
	if err = l.PutChunks(ctx, "new", "b", "job", now, []Chunk{chunk(1, 1, 2, []byte("y"))}); err != nil {
		t.Fatal(err)
	}
	if err = l.PutChunks(ctx, "gone", "c", "job", now, []Chunk{chunk(1, 1, 2, []byte("z"))}); err != nil {
		t.Fatal(err)
	}
	if err = l.DeleteRun(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	runs, chunks, _, err := l.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runs != 2 || chunks != 2 {
		t.Fatalf("stats after delete = %d/%d", runs, chunks)
	}
	n, err := l.Prune(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d runs", n)
	}
	runs, chunks, _, err = l.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 || chunks != 1 {
		t.Fatalf("stats after prune = %d/%d", runs, chunks)
	}
}

func TestDeleteRunsRollsBackOnFailure(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, id := range []string{"first", "blocked", "last"} {
		if err := l.PutChunks(t.Context(), id, "job", "job", time.Now(), []Chunk{chunk(1, 1, 1, []byte("blob"))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.db.ExecContext(t.Context(), `CREATE TRIGGER block_delete BEFORE DELETE ON log_runs
		WHEN OLD.run_id='blocked' BEGIN SELECT RAISE(ABORT, 'injected archive failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteRuns(t.Context(), []string{"first", "blocked", "last"}); err == nil {
		t.Fatal("batch should fail")
	}
	runs, chunks, _, err := l.Stats(t.Context())
	if err != nil || runs != 3 || chunks != 3 {
		t.Fatalf("failed batch changed archive: runs=%d chunks=%d err=%v", runs, chunks, err)
	}
	if _, err := l.db.ExecContext(t.Context(), "DROP TRIGGER block_delete"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteRuns(t.Context(), []string{"first", "blocked", "last"}); err != nil {
		t.Fatal(err)
	}
	runs, chunks, _, err = l.Stats(t.Context())
	if err != nil || runs != 0 || chunks != 0 {
		t.Fatalf("archive remains after batch: runs=%d chunks=%d err=%v", runs, chunks, err)
	}
}

func TestDeleteRunRollsBackOnFailure(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.PutChunks(t.Context(), "blocked", "job", "job", time.Now(), []Chunk{chunk(1, 1, 1, []byte("blob"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.ExecContext(t.Context(), `CREATE TRIGGER block_single_delete BEFORE DELETE ON log_runs
		BEGIN SELECT RAISE(ABORT, 'injected archive failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteRun(t.Context(), "blocked"); err == nil {
		t.Fatal("delete should fail")
	}
	runs, chunks, size, err := l.Stats(t.Context())
	if err != nil || runs != 1 || chunks != 1 || size != 4 {
		t.Fatalf("failed delete lost archive: %d/%d/%d: %v", runs, chunks, size, err)
	}
}

func TestConnectionSettingsSurviveReplacement(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.db.SetMaxIdleConns(0) // Drop the initialized connection.
	l.db.SetMaxIdleConns(1)
	for _, setting := range []struct {
		name string
		want int
	}{{"foreign_keys", 1}, {"synchronous", 2}, {"busy_timeout", 5000}} {
		var got int
		if err := l.db.QueryRowContext(t.Context(), "PRAGMA "+setting.name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != setting.want {
			t.Errorf("replacement connection %s = %d, want %d", setting.name, got, setting.want)
		}
	}
}

func TestPutChunksRollsBackOnFailure(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.PutChunks(t.Context(), "run", "original", "job", time.Now(), []Chunk{chunk(1, 1, 1, []byte("original"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.ExecContext(t.Context(), `CREATE TRIGGER block_chunk BEFORE INSERT ON log_chunks
		WHEN NEW.number=2 BEGIN SELECT RAISE(ABORT, 'injected chunk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := l.PutChunks(t.Context(), "run", "changed", "worker", time.Now(),
		[]Chunk{chunk(1, 1, 1, []byte("changed")), chunk(2, 2, 2, []byte("new"))}); err == nil {
		t.Fatal("batch should fail")
	}
	var job string
	if err := l.db.QueryRowContext(t.Context(), "SELECT job FROM log_runs WHERE run_id='run'").Scan(&job); err != nil || job != "original" {
		t.Fatalf("failed batch changed metadata: %q: %v", job, err)
	}
	var blobs [][]byte
	if err := l.EachChunk(t.Context(), "run", 0, func(c Chunk) (bool, error) {
		blobs = append(blobs, c.Blob)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 || !bytes.Equal(blobs[0], []byte("original")) {
		t.Fatalf("failed batch changed chunks: %q", blobs)
	}
}
