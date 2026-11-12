package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestStmtCacheReusesAndSurvivesTransactions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(v INTEGER)"); err != nil {
		t.Fatal(err)
	}
	c := NewStmtCache(db)
	const ins = "INSERT INTO t(v) VALUES(?)"
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			if _, err := c.Exec(ctx, ins, i); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	first := c.Lookup(ins)
	if first == nil {
		t.Fatal("statement not cached")
	}
	if again, _ := c.Prepare(ctx, ins); again != first {
		t.Fatal("second Prepare did not reuse the cached statement")
	}
	// Inside a transaction on the single writer connection, Lookup plus
	// tx.StmtContext must not wait for another connection.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.StmtContext(ctx, c.Lookup(ins)).ExecContext(ctx, 99); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil || n != 17 {
		t.Fatalf("rows = %d, %v", n, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Lookup(ins) != nil {
		t.Fatal("closed cache still returns statements")
	}
	if _, err := c.Exec(ctx, ins, 1); err == nil {
		t.Fatal("closed cache prepared a statement")
	}
}
