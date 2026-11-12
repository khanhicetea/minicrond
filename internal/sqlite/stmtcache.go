package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// StmtCache keeps prepared statements for the few queries that run on every
// job run (run row insert, terminal transition, log archive upserts). The
// driver has no statement cache of its own, so a plain ExecContext parses and
// plans the SQL text each time; in the 300-job benchmark that was about a
// tenth of daemon CPU.
//
// Only constant query strings may be passed: the cache is bounded by the
// number of distinct call sites, not by input. database/sql re-prepares a
// statement on a replacement connection by itself.
type StmtCache struct {
	db     *sql.DB
	mu     sync.Mutex
	m      map[string]*sql.Stmt
	closed bool
}

// NewStmtCache returns an empty cache for db.
func NewStmtCache(db *sql.DB) *StmtCache { return &StmtCache{db: db} }

// Exec runs query with a cached statement, preparing it on first use. It must
// not be called while the caller holds the pool's only connection (inside a
// transaction or a pinned *sql.Conn): preparing would wait for that
// connection. Use Lookup there.
func (c *StmtCache) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	stmt, err := c.Prepare(ctx, query)
	if err != nil {
		return nil, err
	}
	return stmt.ExecContext(ctx, args...)
}

// Prepare returns the cached statement for query, preparing it if needed. A
// failed prepare is not cached.
func (c *StmtCache) Prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	if stmt := c.Lookup(query); stmt != nil {
		return stmt, nil
	}
	// Prepare without holding mu: it may wait for the writer connection.
	stmt, err := c.db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.Join(sql.ErrConnDone, stmt.Close())
	}
	if existing := c.m[query]; existing != nil {
		_ = stmt.Close() // lost a race with another first use
		return existing, nil
	}
	if c.m == nil {
		c.m = make(map[string]*sql.Stmt)
	}
	c.m[query] = stmt
	return stmt, nil
}

// Lookup returns the cached statement for query, or nil. It never prepares,
// so it is safe inside a transaction: tx.StmtContext reuses the statement on
// the transaction's connection, or prepares it there once.
func (c *StmtCache) Lookup(query string) *sql.Stmt {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[query]
}

// Close closes every cached statement. Later calls prepare nothing.
func (c *StmtCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	var err error
	for _, stmt := range c.m {
		err = errors.Join(err, stmt.Close())
	}
	c.m = nil
	return err
}
