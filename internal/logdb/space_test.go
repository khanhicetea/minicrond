package logdb

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/sqlite"
)

func randomBlob(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func putRuns(t *testing.T, l *LogDB, runs, chunksPerRun, blobSize int, at time.Time) {
	t.Helper()
	for r := range runs {
		chunks := make([]Chunk, chunksPerRun)
		for c := range chunks {
			chunks[c] = chunk(c+1, uint64(c+1), uint64(c+1), randomBlob(t, blobSize))
		}
		if err := l.PutChunks(t.Context(), fmt.Sprintf("run-%s-%d", at.Format("150405.000"), r), "job", "job", at, chunks); err != nil {
			t.Fatal(err)
		}
	}
}

func pragma(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Pruning more chunks than one batch still removes all expired chunks and
// only the runs left without any.
func TestPruneDeletesInBatches(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	old := time.Now().Add(-48 * time.Hour)
	putRuns(t, l, 3, pruneBatch, 16, old)
	putRuns(t, l, 1, 2, 16, time.Now())
	n, err := l.Prune(t.Context(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	runs, chunks, _, err := l.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || runs != 1 || chunks != 2 {
		t.Fatalf("pruned %d runs; %d runs and %d chunks remain", n, runs, chunks)
	}
}

func TestPruneToSizeRemovesOldestFirst(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	putRuns(t, l, 4, 4, 64<<10, time.Now().Add(-3*time.Hour))
	newest := time.Now()
	putRuns(t, l, 1, 4, 64<<10, newest)
	const budget = 640 << 10 // room for the newest run (256 KiB) and a little more
	n, err := l.PruneToSize(t.Context(), budget)
	if err != nil {
		t.Fatal(err)
	}
	used, err := l.usedBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 || used > budget {
		t.Fatalf("removed %d runs, %d bytes used over a %d budget", n, used, budget)
	}
	var newestChunks int
	if err := l.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM log_chunks WHERE run_id=?", fmt.Sprintf("run-%s-0", newest.Format("150405.000"))).Scan(&newestChunks); err != nil {
		t.Fatal(err)
	}
	if newestChunks != 4 {
		t.Fatalf("newest run kept %d of 4 chunks", newestChunks)
	}
}

func TestCompactReturnsPrunedSpace(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if mode := pragma(t, l.db, "auto_vacuum"); mode != 2 {
		t.Fatalf("new archive auto_vacuum = %d, want incremental (2)", mode)
	}
	putRuns(t, l, 8, 4, 64<<10, time.Now().Add(-48*time.Hour))
	if _, err := l.Prune(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	before := pragma(t, l.db, "page_count")
	if err := l.Compact(t.Context()); err != nil {
		t.Fatal(err)
	}
	if free := pragma(t, l.db, "freelist_count"); free != 0 {
		t.Fatalf("%d free pages left after compaction", free)
	}
	if after := pragma(t, l.db, "page_count"); after*4 > before {
		t.Fatalf("page count %d -> %d after pruning everything", before, after)
	}
	if info, err := os.Stat(filepath.Join(dir, "minicron-logs.db-wal")); err == nil && info.Size() != 0 {
		t.Fatalf("WAL is %d bytes after a truncating checkpoint", info.Size())
	}
}

// API reads use the read-only pool, so they cannot write by mistake.
func TestReadPoolIsReadOnly(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.rdb.ExecContext(t.Context(), "DELETE FROM log_runs"); err == nil {
		t.Fatal("read pool accepted a write")
	}
}

// A pre-0.6 archive (user_version set, no application_id) is refused.
func TestPreReleaseSchemaRefused(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "minicron-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE log_runs (run_id TEXT PRIMARY KEY); PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(dir); !errors.Is(err, sqlite.ErrUnsupportedSchema) {
		t.Fatalf("expected unsupported-schema error, got %v", err)
	}
}

func TestFreshSchemaIdentity(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if app, version := pragma(t, l.db, "application_id"), pragma(t, l.db, "user_version"); app != ApplicationID || version != SchemaVersion {
		t.Fatalf("application_id = %#x, user_version = %d", app, version)
	}
	if mode := pragma(t, l.db, "auto_vacuum"); mode != 2 {
		t.Fatalf("auto_vacuum = %d, want incremental", mode)
	}
}
