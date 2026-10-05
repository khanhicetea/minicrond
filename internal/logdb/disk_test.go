package logdb

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *LogDB {
	t.Helper()
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// putRun archives chunks of size bytes at a given archive time.
func putRun(t *testing.T, l *LogDB, id string, at time.Time, chunks, size int) {
	t.Helper()
	cs := make([]Chunk, chunks)
	for i := range cs {
		cs[i] = chunk(i+1, uint64(i+1), uint64(i+1), bytes.Repeat([]byte{'x'}, size))
	}
	if err := l.PutChunks(t.Context(), id, "job", "job", at, cs); err != nil {
		t.Fatal(err)
	}
}

func runIDs(t *testing.T, l *LogDB) []string {
	t.Helper()
	rows, err := l.rdb.QueryContext(t.Context(), "SELECT run_id FROM log_runs ORDER BY run_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// 3A: pressure pruning takes the oldest chunks first whatever their age, and
// stops once enough bytes are gone.
func TestPruneOldestDeletesOldestFirstUntilEnough(t *testing.T) {
	l := openTest(t)
	base := time.Now()
	putRun(t, l, "c-newest", base, 2, 1000)
	putRun(t, l, "a-oldest", base.Add(-3*time.Hour), 2, 1000)
	putRun(t, l, "b-middle", base.Add(-time.Hour), 2, 1000)

	// 2500 bytes wanted: the two chunks of the oldest run and one of the next.
	res, err := l.PruneOldest(t.Context(), 2500, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Chunks != 3 || res.Bytes != 3000 || res.Runs != 1 {
		t.Fatalf("result = %+v, want 3 chunks, 3000 bytes, 1 run", res)
	}
	if got := runIDs(t, l); !slices.Equal(got, []string{"b-middle", "c-newest"}) {
		t.Fatalf("runs left = %v", got)
	}
	_, chunks, _, err := l.Stats(t.Context())
	if err != nil || chunks != 3 {
		t.Fatalf("chunks left = %d, %v", chunks, err)
	}
	// The surviving chunk of the middle run is its newest one.
	var numbers []int
	if err := l.EachChunk(t.Context(), "b-middle", 0, func(c Chunk) (bool, error) { numbers = append(numbers, c.Number); return true, nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(numbers, []int{2}) {
		t.Fatalf("b-middle kept chunks %v, want [2]", numbers)
	}
}

// Chunks of protected runs are skipped wherever they sit in the age order, and
// are not counted toward the amount reclaimed.
func TestPruneOldestNeverTouchesProtectedRuns(t *testing.T) {
	l := openTest(t)
	base := time.Now()
	putRun(t, l, "active", base.Add(-10*time.Hour), 3, 1000) // oldest, but protected
	putRun(t, l, "done-1", base.Add(-2*time.Hour), 1, 1000)
	putRun(t, l, "done-2", base.Add(-time.Hour), 1, 1000)

	res, err := l.PruneOldest(t.Context(), 1<<30, func(id string) bool { return id == "active" })
	if err != nil {
		t.Fatal(err)
	}
	if res.Chunks != 2 || res.Bytes != 2000 {
		t.Fatalf("result = %+v, want the two unprotected chunks", res)
	}
	if got := runIDs(t, l); !slices.Equal(got, []string{"active"}) {
		t.Fatalf("runs left = %v", got)
	}
	_, chunks, _, _ := l.Stats(t.Context())
	if chunks != 3 {
		t.Fatalf("protected run lost chunks: %d left", chunks)
	}
}

// More protected rows than one batch, interleaved with eligible ones, must not
// stall the scan or make it revisit rows.
func TestPruneOldestScansPastManyProtectedChunks(t *testing.T) {
	l := openTest(t)
	base := time.Now().Add(-100 * time.Hour)
	putRun(t, l, "protected", base, 3*pruneBatch, 10)
	putRun(t, l, "eligible", base.Add(time.Hour), 4, 10)
	res, err := l.PruneOldest(t.Context(), 1<<20, func(id string) bool { return id == "protected" })
	if err != nil {
		t.Fatal(err)
	}
	if res.Chunks != 4 {
		t.Fatalf("deleted %d chunks, want 4", res.Chunks)
	}
	_, chunks, _, _ := l.Stats(t.Context())
	if chunks != 3*pruneBatch {
		t.Fatalf("protected chunks left = %d, want %d", chunks, 3*pruneBatch)
	}
}

func TestPruneOldestStopsWhenContextEnds(t *testing.T) {
	l := openTest(t)
	putRun(t, l, "run", time.Now(), 10, 100)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.PruneOldest(ctx, 1<<20, nil); err == nil {
		t.Fatal("PruneOldest ignored a canceled context")
	}
	_, chunks, _, _ := l.Stats(t.Context())
	if chunks != 10 {
		t.Fatalf("a canceled prune deleted chunks: %d left", chunks)
	}
}

func TestPruneOldestWithNothingEligible(t *testing.T) {
	l := openTest(t)
	res, err := l.PruneOldest(t.Context(), 1000, nil)
	if err != nil || res != (PruneResult{}) {
		t.Fatalf("empty archive: %+v, %v", res, err)
	}
}

func TestFileBytesCountsDatabaseAndWAL(t *testing.T) {
	l := openTest(t)
	db0, _, err := l.FileBytes()
	if err != nil || db0 <= 0 {
		t.Fatalf("FileBytes = %d, %v", db0, err)
	}
	if _, err := l.db.ExecContext(t.Context(), "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		putRun(t, l, fmt.Sprintf("run-%d", i), time.Now(), 1, 32<<10)
	}
	db, wal, err := l.FileBytes()
	if err != nil || db <= 0 || wal < 20*(32<<10) {
		t.Fatalf("FileBytes = db %d wal %d, %v; want the WAL to hold the written blobs", db, wal, err)
	}
	if !strings.HasSuffix(l.path, "minicron-logs.db") {
		t.Fatalf("path = %s", l.path)
	}
}
