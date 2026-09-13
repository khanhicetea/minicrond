// Package logdb owns the long-term log archive: a SQLite database kept in its
// own file (minicron-logs.db, separate from the main minicron.db). The file
// based logstore buffer copies sealed chunks here so logs survive daemon
// restarts in one queryable place while the live buffer stays small.
package logdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 1

// Chunk is one sealed, compressed stream of log frames as produced by the
// logstore file writer. Blobs are stored verbatim so archived chunks decode
// with the exact same framing as the on-disk buffer.
type Chunk struct {
	Number   int    // chunk sequence within the run
	First    uint64 // first frame sequence in the blob
	Last     uint64 // last frame sequence in the blob
	RawBytes int64  // uncompressed frame bytes (accounting only)
	Blob     []byte // compressed frame stream
}

type LogDB struct{ db *sql.DB }

// Open creates or opens <dataDir>/minicron-logs.db and applies migrations.
func Open(dataDir string) (*LogDB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "minicron-logs.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	l := &LogDB{db: db}
	if err := l.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, err
		}
	}
	return l, nil
}

func (l *LogDB) Close() error { return l.db.Close() }

func (l *LogDB) Ping(ctx context.Context) error { return l.db.PingContext(ctx) }

func (l *LogDB) migrate(ctx context.Context) error {
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err := l.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	var version int
	if err := l.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return fmt.Errorf("log database schema %d is newer than supported schema %d", version, SchemaVersion)
	}
	if version == 0 {
		if _, err := l.db.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("log database migration 1: %w", err)
		}
	}
	return nil
}

const schema = `
BEGIN;
CREATE TABLE log_runs (
 run_id TEXT PRIMARY KEY,
 job TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL DEFAULT '',
 created_us INTEGER NOT NULL,
 updated_us INTEGER NOT NULL
);
CREATE TABLE log_chunks (
 run_id TEXT NOT NULL REFERENCES log_runs(run_id) ON DELETE CASCADE,
 number INTEGER NOT NULL,
 first_seq INTEGER NOT NULL,
 last_seq INTEGER NOT NULL,
 raw_bytes INTEGER NOT NULL,
 blob BLOB NOT NULL,
 PRIMARY KEY(run_id, number)
);
CREATE INDEX idx_log_runs_time ON log_runs(created_us);
CREATE INDEX idx_log_chunks_seq ON log_chunks(run_id, last_seq);
PRAGMA user_version=1;
COMMIT;`

// PutChunks upserts chunks for a run. Re-archiving the same chunk after a
// crash between the database write and the buffer-file cleanup is safe: the
// row is replaced, never duplicated.
func (l *LogDB) PutChunks(ctx context.Context, runID, job, kind string, at time.Time, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := at.UnixMicro()
	if _, err = tx.ExecContext(ctx, `INSERT INTO log_runs(run_id,job,kind,created_us,updated_us) VALUES(?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET job=excluded.job, kind=excluded.kind, updated_us=excluded.updated_us`, runID, job, kind, now, now); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err = tx.ExecContext(ctx, `INSERT INTO log_chunks(run_id,number,first_seq,last_seq,raw_bytes,blob) VALUES(?,?,?,?,?,?)
			ON CONFLICT(run_id,number) DO UPDATE SET first_seq=excluded.first_seq,last_seq=excluded.last_seq,raw_bytes=excluded.raw_bytes,blob=excluded.blob`,
			runID, c.Number, c.First, c.Last, c.RawBytes, c.Blob); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EachChunk iterates archived chunks of a run in order. Chunks whose last
// frame is at or below after are skipped. Returning false from fn stops the
// iteration; the callback error (if any) is returned wrapped.
func (l *LogDB) EachChunk(ctx context.Context, runID string, after uint64, fn func(Chunk) (bool, error)) error {
	rows, err := l.db.QueryContext(ctx, "SELECT number,first_seq,last_seq,raw_bytes,blob FROM log_chunks WHERE run_id=? AND last_seq>? ORDER BY number", runID, after)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.Number, &c.First, &c.Last, &c.RawBytes, &c.Blob); err != nil {
			return err
		}
		ok, err := fn(c)
		if err != nil {
			return fmt.Errorf("chunk %d of run %s: %w", c.Number, runID, err)
		}
		if !ok {
			return nil
		}
	}
	return rows.Err()
}

// DeleteRun removes a run and all of its archived chunks.
func (l *LogDB) DeleteRun(ctx context.Context, runID string) error {
	_, err := l.db.ExecContext(ctx, "DELETE FROM log_chunks WHERE run_id=?", runID)
	if err != nil {
		return err
	}
	_, err = l.db.ExecContext(ctx, "DELETE FROM log_runs WHERE run_id=?", runID)
	return err
}

// Prune deletes archived runs whose first chunk was archived before the cutoff
// and reports how many runs were removed. Long-running worker runs are pruned
// by created_us as well: pruning old data never affects live buffers.
func (l *LogDB) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := l.db.ExecContext(ctx, "DELETE FROM log_runs WHERE created_us<?", before.UnixMicro())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// Chunks of deleted runs cascade, but rows inserted without the run row
	// (older versions / interrupted imports) are swept defensively.
	if _, err := l.db.ExecContext(ctx, "DELETE FROM log_chunks WHERE run_id NOT IN (SELECT run_id FROM log_runs)"); err != nil {
		return n, err
	}
	return n, nil
}

// Stats reports archive totals for logging and diagnostics.
func (l *LogDB) Stats(ctx context.Context) (runs, chunks, blobBytes int64, err error) {
	err = errors.Join(
		l.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM log_runs").Scan(&runs),
		l.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM log_chunks").Scan(&chunks),
		l.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(LENGTH(blob)),0) FROM log_chunks").Scan(&blobBytes),
	)
	return runs, chunks, blobBytes, err
}
