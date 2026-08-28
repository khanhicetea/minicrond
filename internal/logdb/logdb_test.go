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
