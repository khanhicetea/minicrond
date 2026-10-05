package logdb

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// legacyEachChunkQuery is the pre-A06 statement, kept so the plan checks below
// are shown to tell the two apart.
const legacyEachChunkQuery = "SELECT number,first_seq,last_seq,raw_bytes,blob FROM log_chunks WHERE run_id=? AND last_seq>? AND number>? ORDER BY number LIMIT 1"

func queryPlan(t *testing.T, l *LogDB, query string, args ...any) string {
	t.Helper()
	rows, err := l.rdb.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "; ")
}

// A06: the cursor query must seek on the sequence index, with no sort and no
// walk of the (run_id, number) index from the run's first chunk. Checked on
// populated data so the planner's choice is the one production makes.
func TestEachChunkQueryPlanSeeksOnSequenceIndex(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for run := range 3 {
		chunks := make([]Chunk, 500)
		for i := range chunks {
			chunks[i] = chunk(i+1, uint64(i*10+1), uint64(i*10+10), []byte("x"))
		}
		if err := l.PutChunks(t.Context(), fmt.Sprintf("run-%d", run), "job", "job", time.Now(), chunks); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.db.ExecContext(t.Context(), "ANALYZE"); err != nil {
		t.Fatal(err)
	}
	plan := queryPlan(t, l, eachChunkQuery, "run-1", 2500)
	if !strings.Contains(plan, "idx_log_chunks_seq (run_id=? AND last_seq>?)") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("cursor query plan = %q; want a seek on idx_log_chunks_seq without a sort", plan)
	}
	legacy := queryPlan(t, l, legacyEachChunkQuery, "run-1", 2500, -1)
	if strings.Contains(legacy, "idx_log_chunks_seq") {
		t.Fatalf("the plan check does not discriminate: the legacy query also plans as %q", legacy)
	}
}

// Every position of the cursor must return exactly the chunks that end beyond
// it, in chunk order, including a cursor inside a chunk, before the first one,
// at the last frame and past it, and runs whose first retained chunk is not 1.
func TestEachChunkCursorPositions(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Chunks 3..7 remain (older ones were evicted); sequence gaps are allowed.
	var chunks []Chunk
	for n := 3; n <= 7; n++ {
		chunks = append(chunks, chunk(n, uint64(n*10), uint64(n*10+5), []byte{byte(n)}))
	}
	at := time.Now()
	if err := l.PutChunks(t.Context(), "run", "job", "job", at, chunks); err != nil {
		t.Fatal(err)
	}
	// A neighbour must never leak in.
	if err := l.PutChunks(t.Context(), "other", "job", "job", at, []Chunk{chunk(1, 1, 1000, []byte("o"))}); err != nil {
		t.Fatal(err)
	}
	for after := uint64(0); after <= 80; after++ {
		var want []int
		for _, c := range chunks {
			if c.Last > after {
				want = append(want, c.Number)
			}
		}
		var got []int
		err := l.EachChunk(t.Context(), "run", after, func(c Chunk) (bool, error) {
			got = append(got, c.Number)
			return true, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("after=%d: chunks %v, want %v", after, got, want)
		}
	}
}

// Paging through a long history chunk by chunk visits each chunk exactly once.
func TestEachChunkPaginationVisitsEveryChunkOnce(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	const total = 2000
	var chunks []Chunk
	for n := 1; n <= total; n++ {
		chunks = append(chunks, chunk(n, uint64(n), uint64(n), []byte("x")))
	}
	for start := 0; start < total; start += 500 {
		if err := l.PutChunks(t.Context(), "run", "job", "job", time.Now(), chunks[start:start+500]); err != nil {
			t.Fatal(err)
		}
	}
	var seen []int
	var after uint64
	for {
		page := 0
		err := l.EachChunk(t.Context(), "run", after, func(c Chunk) (bool, error) {
			seen = append(seen, c.Number)
			after = c.Last
			page++
			return page < 7, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if page == 0 {
			break
		}
	}
	if len(seen) != total {
		t.Fatalf("visited %d chunks, want %d", len(seen), total)
	}
	for i, n := range seen {
		if n != i+1 {
			t.Fatalf("visit %d was chunk %d", i, n)
		}
	}
}
