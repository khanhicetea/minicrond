package logdb

import (
	"testing"
	"time"
)

func TestRetryPreservesArchiveAgeAndRunMetadata(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := time.Now().Add(-48 * time.Hour)
	chunks := []Chunk{chunk(1, 1, 1, []byte("blob"))}
	if err := db.PutChunks(t.Context(), "run", "worker", "worker", old, chunks); err != nil {
		t.Fatal(err)
	}
	// An old buffer may lack job/kind metadata. Replaying it must not erase
	// known metadata or extend retention every time cleanup is retried.
	if err := db.PutChunks(t.Context(), "run", "", "", time.Now(), chunks); err != nil {
		t.Fatal(err)
	}
	var job, kind string
	if err := db.db.QueryRowContext(t.Context(), "SELECT job,kind FROM log_runs WHERE run_id='run'").Scan(&job, &kind); err != nil {
		t.Fatal(err)
	}
	if job != "worker" || kind != "worker" {
		t.Fatalf("metadata = %q/%q", job, kind)
	}
	n, err := db.Prune(t.Context(), time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("retry renewed retention: pruned %d runs, error %v", n, err)
	}
}
