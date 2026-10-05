package logdb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// SQLite's busy handler ignores context cancellation, so writes retry in short
// slices: a held write lock must not keep a canceled archive or deletion
// waiting for the full five-second busy timeout (audit A04).
func TestWriteTransactionsStopWaitingWhenContextEnds(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	control, err := sql.Open("sqlite", filepath.Join(dir, "minicron-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	conn, err := control.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	for name, op := range map[string]func(context.Context) error{
		"PutChunks": func(ctx context.Context) error {
			return l.PutChunks(ctx, "run", "job", "job", time.Now(), []Chunk{{Number: 1, First: 1, Last: 1, RawBytes: 1, Blob: []byte("x")}})
		},
		"DeleteRuns": func(ctx context.Context) error { return l.DeleteRuns(ctx, []string{"run"}) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := op(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want the context's deadline", err)
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Fatalf("waited %v on the database lock after the context ended", took)
			}
		})
	}
	// The slice-sized wait must not leak into later writes on the connection.
	if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := l.PutChunks(t.Context(), "run", "job", "job", time.Now(), []Chunk{{Number: 1, First: 1, Last: 1, RawBytes: 1, Blob: []byte("x")}}); err != nil {
		t.Fatalf("write after the lock was released: %v", err)
	}
}

// Review S6: Prune, PruneToSize and Compact (the daily maintenance sweep) used
// SQLite's plain 5 s busy wait per statement, so shutdown could wait on them.
func TestMaintenanceStatementsStopWaitingWhenContextEnds(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	blob := bytes.Repeat([]byte("x"), 64<<10)
	for i := 1; i <= 3; i++ {
		if err := l.PutChunks(t.Context(), "run", "job", "job", time.Now(), []Chunk{{Number: i, First: uint64(i), Last: uint64(i), RawBytes: 1, Blob: blob}}); err != nil {
			t.Fatal(err)
		}
	}
	// Free pages give Compact something to vacuum, which needs the write lock.
	if err := l.PutChunks(t.Context(), "gone", "job", "job", time.Now(), []Chunk{{Number: 1, First: 1, Last: 1, RawBytes: 1, Blob: bytes.Repeat([]byte("y"), 256<<10)}}); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteRuns(t.Context(), []string{"gone"}); err != nil {
		t.Fatal(err)
	}
	control, err := sql.Open("sqlite", filepath.Join(dir, "minicron-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	conn, err := control.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	for name, op := range map[string]func(context.Context) error{
		"Prune":       func(ctx context.Context) error { _, err := l.Prune(ctx, time.Now().Add(time.Hour)); return err },
		"PruneToSize": func(ctx context.Context) error { _, err := l.PruneToSize(ctx, 1); return err },
		"Compact":     func(ctx context.Context) error { return l.Compact(ctx) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := op(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want the context's deadline", err)
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Fatalf("waited %v on the database lock after the context ended", took)
			}
		})
	}
}
