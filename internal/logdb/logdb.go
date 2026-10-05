// Package logdb owns the long-term log archive: a SQLite database kept in its
// own file (minicron-logs.db, separate from the main minicron.db). The file
// based logstore buffer copies sealed chunks here so logs survive daemon
// restarts in one queryable place while the live buffer stays small.
package logdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/khanhicetea/minicrond/internal/sqlite"
)

const SchemaVersion = 3

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

// LogDB is the archive. db is the single writer connection; rdb is a
// read-only pool so log pages and downloads never queue behind archival or
// pruning, and vice versa.
type LogDB struct{ db, rdb *sql.DB }

// Open creates or opens <dataDir>/minicron-logs.db and applies migrations.
func Open(dataDir string, opt ...sqlite.Options) (*LogDB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create log database directory: %w", err)
	}
	path := filepath.Join(dataDir, "minicron-logs.db")
	db, err := sqlite.Open(path, opt...)
	if err != nil {
		return nil, fmt.Errorf("open log database: %w", err)
	}
	l := &LogDB{db: db}
	if err := l.migrate(context.Background()); err != nil {
		return nil, errors.Join(fmt.Errorf("migrate log database: %w", err), db.Close())
	}
	if l.rdb, err = sqlite.OpenReader(path); err != nil {
		return nil, errors.Join(fmt.Errorf("open log database read pool: %w", err), db.Close())
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Join(fmt.Errorf("set log database permissions: %w", err), l.Close())
		}
	}
	return l, nil
}

func (l *LogDB) Close() error { return errors.Join(l.rdb.Close(), l.db.Close()) }

// PoolStats reports connection waits for the writer and the read pool.
func (l *LogDB) PoolStats() (writer, reader sql.DBStats) { return l.db.Stats(), l.rdb.Stats() }

func (l *LogDB) Ping(ctx context.Context) error { return l.db.PingContext(ctx) }

func (l *LogDB) migrate(ctx context.Context) error {
	// Incremental auto-vacuum must be chosen before the file has a header,
	// which switching to WAL writes. On an existing file this only records
	// the wish; migration 3 applies it with VACUUM.
	if _, err := l.db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		return err
	}
	if _, err := l.db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return err
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
			return fmt.Errorf("log database migration: %w", err)
		}
		return nil
	}
	if version == 1 {
		if _, err := l.db.ExecContext(ctx, migration2); err != nil {
			return fmt.Errorf("log database migration 2: %w", err)
		}
		version = 2
	}
	if version == 2 {
		// Existing files were created without auto-vacuum, so pruned space was
		// never returned to the filesystem. Switching modes needs one VACUUM,
		// which rewrites the file and temporarily needs as much free space.
		slog.Info("enabling incremental vacuum on the log archive; this rewrites the file once")
		if _, err := l.db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
			return fmt.Errorf("log database migration 3: %w", err)
		}
		if _, err := l.db.ExecContext(ctx, "VACUUM"); err != nil {
			return fmt.Errorf("log database migration 3: %w", err)
		}
		if _, err := l.db.ExecContext(ctx, "PRAGMA user_version=3"); err != nil {
			return fmt.Errorf("log database migration 3: %w", err)
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
 archived_us INTEGER NOT NULL,
 blob BLOB NOT NULL,
 PRIMARY KEY(run_id, number)
);
CREATE INDEX idx_log_runs_time ON log_runs(created_us);
CREATE INDEX idx_log_chunks_seq ON log_chunks(run_id, last_seq);
CREATE INDEX idx_log_chunks_archived ON log_chunks(archived_us);
PRAGMA user_version=3;
COMMIT;`

const migration2 = `
BEGIN;
ALTER TABLE log_chunks ADD COLUMN archived_us INTEGER NOT NULL DEFAULT 0;
UPDATE log_chunks SET archived_us=COALESCE((SELECT updated_us FROM log_runs WHERE log_runs.run_id=log_chunks.run_id),0);
CREATE INDEX idx_log_chunks_archived ON log_chunks(archived_us);
PRAGMA user_version=2;
COMMIT;`

// PutChunks upserts chunks for a run. Re-archiving the same chunk after a
// crash between the database write and the buffer-file cleanup is safe: the
// row is replaced, never duplicated. Retries preserve its original archive
// time and do not erase run metadata unavailable during orphan recovery.
func (l *LogDB) PutChunks(ctx context.Context, runID, job, kind string, at time.Time, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	return l.writeTx(ctx, func(tx *sql.Tx) error { return putChunks(ctx, tx, runID, job, kind, at, chunks) })
}

func putChunks(ctx context.Context, tx *sql.Tx, runID, job, kind string, at time.Time, chunks []Chunk) error {
	now := at.UnixMicro()
	if _, err := tx.ExecContext(ctx, `INSERT INTO log_runs(run_id,job,kind,created_us,updated_us) VALUES(?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET job=COALESCE(NULLIF(excluded.job,''),log_runs.job),
		kind=COALESCE(NULLIF(excluded.kind,''),log_runs.kind), updated_us=excluded.updated_us`, runID, job, kind, now, now); err != nil {
		return err
	}
	const putChunk = `INSERT INTO log_chunks(run_id,number,first_seq,last_seq,raw_bytes,archived_us,blob) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(run_id,number) DO UPDATE SET first_seq=excluded.first_seq,last_seq=excluded.last_seq,raw_bytes=excluded.raw_bytes,blob=excluded.blob`
	// Preparing once per multi-chunk batch avoids repeated SQLite parsing while
	// keeping statement lifetime and memory bounded by this transaction.
	if len(chunks) > 1 {
		stmt, err := tx.PrepareContext(ctx, putChunk)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, c := range chunks {
			if _, err := stmt.ExecContext(ctx, runID, c.Number, c.First, c.Last, c.RawBytes, now, c.Blob); err != nil {
				return err
			}
		}
		if err := stmt.Close(); err != nil {
			return err
		}
	} else {
		c := chunks[0]
		if _, err := tx.ExecContext(ctx, putChunk,
			runID, c.Number, c.First, c.Last, c.RawBytes, now, c.Blob); err != nil {
			return err
		}
	}
	return nil
}

// Archive and deletion transactions wait for the SQLite write lock in short
// slices and retry, instead of one long busy wait: SQLite's own busy handler
// ignores context cancellation, so a stuck lock holder would otherwise keep a
// shutting-down caller (and the database it must close) busy for the whole
// busy_timeout. The overall wait stays the connection's usual five seconds.
const (
	busySlice = 100 * time.Millisecond
	busyTotal = 5 * time.Second
)

// writeTx runs fn in a transaction on the writer connection, retrying the
// whole transaction while SQLite reports the database busy, and giving up as
// soon as ctx ends.
func (l *LogDB) writeTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return l.writeConn(ctx, func(conn *sql.Conn) error { return runTx(ctx, conn, fn) })
}

// writeConn runs fn on the writer connection with the sliced busy wait, retrying
// it as a whole while SQLite reports the database busy. fn must be safe to
// repeat. Every statement that can wait for the write lock (archive, delete,
// prune, vacuum, checkpoint) goes through here so shutdown can interrupt it.
func (l *LogDB) writeConn(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busySlice.Milliseconds())); err != nil {
		return err
	}
	defer func() {
		if err := restoreBusyTimeout(context.WithoutCancel(ctx), conn); err != nil {
			// The pool has a single writer connection: leaving the short wait on
			// it would make every later writer fail with SQLITE_BUSY. Discard the
			// connection; a new one starts with the full busy_timeout.
			slog.Error("restoring the log database busy timeout failed; discarding the connection", "error", err)
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	start := time.Now()
	for {
		// The writer connection starts transactions with BEGIN IMMEDIATE, so
		// the busy wait usually happens in BeginTx itself.
		err := fn(conn)
		if err == nil || !isBusy(err) || time.Since(start) >= busyTotal {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", ctxErr, err)
		}
	}
}

func runTx(ctx context.Context, conn *sql.Conn, fn func(*sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(tx); err == nil {
		err = tx.Commit()
	}
	if err != nil {
		tx.Rollback()
	}
	return err
}

// restoreBusyTimeout returns the writer connection to its normal wait. It is a
// variable so tests can inject a failure.
var restoreBusyTimeout = func(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=5000")
	return err
}

func isBusy(err error) bool {
	return strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked")
}

// EachChunk iterates archived chunks of a run in order. Chunks whose last
// frame is at or below after are skipped. Returning false from fn stops the
// iteration; the callback error (if any) is returned wrapped.
func (l *LogDB) EachChunk(ctx context.Context, runID string, after uint64, fn func(Chunk) (bool, error)) error {
	lastNumber := -1
	for {
		// Fetch one blob at a time and release the connection before decoding.
		// A row-count batch alone could prefetch hundreds of MiB for a tiny page.
		var c Chunk
		err := l.rdb.QueryRowContext(ctx, "SELECT number,first_seq,last_seq,raw_bytes,blob FROM log_chunks WHERE run_id=? AND last_seq>? AND number>? ORDER BY number LIMIT 1", runID, after, lastNumber).
			Scan(&c.Number, &c.First, &c.Last, &c.RawBytes, &c.Blob)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok, err := fn(c)
		if err != nil {
			return fmt.Errorf("chunk %d of run %s: %w", c.Number, runID, err)
		}
		if !ok {
			return nil
		}
		lastNumber = c.Number
	}
}

// DeleteRun removes a run and all of its archived chunks.
func (l *LogDB) DeleteRun(ctx context.Context, runID string) error {
	return l.DeleteRuns(ctx, []string{runID})
}

// DeleteRuns removes up to a page of archived runs in one transaction. Both
// tables are deleted explicitly so cleanup does not depend on connection-local
// foreign-key settings if the SQLite connection is ever recreated.
func (l *LogDB) DeleteRuns(ctx context.Context, runIDs []string) error {
	if len(runIDs) == 0 {
		return nil
	}
	return l.writeTx(ctx, func(tx *sql.Tx) error {
		for start := 0; start < len(runIDs); start += 128 {
			page := runIDs[start:min(start+128, len(runIDs))]
			args := make([]any, len(page))
			for i, id := range page {
				args[i] = id
			}
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(page)), ",")
			for _, table := range []string{"log_chunks", "log_runs"} {
				if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE run_id IN ("+placeholders+")", args...); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// pruneBatch bounds each prune transaction. Chunks are up to about 1 MiB, so
// a batch stays short enough that archival and log reads interleave with it.
const pruneBatch = 256

// Prune applies a rolling age window to individual archived chunks. A run row
// is removed only after its final retained chunk is gone. Chunks are deleted
// in small transactions, so a large backlog never holds the writer for long.
func (l *LogDB) Prune(ctx context.Context, before time.Time) (int64, error) {
	if _, err := l.deleteChunks(ctx, "SELECT rowid FROM log_chunks WHERE archived_us<? LIMIT ?", before.UnixMicro()); err != nil {
		return 0, err
	}
	return l.deleteEmptyRuns(ctx)
}

// PruneToSize removes the oldest archived chunks until the database's used
// pages fit in maxBytes. It returns the number of runs removed entirely.
func (l *LogDB) PruneToSize(ctx context.Context, maxBytes int64) (int64, error) {
	for {
		used, err := l.usedBytes(ctx)
		if err != nil {
			return 0, err
		}
		if used <= maxBytes {
			break
		}
		n, err := l.deleteOldest(ctx, used-maxBytes)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			break // Only metadata is left; it cannot shrink further.
		}
	}
	return l.deleteEmptyRuns(ctx)
}

// deleteOldest deletes the oldest chunks whose blobs add up to at least
// excess bytes, at most one batch per call, in one transaction.
func (l *LogDB) deleteOldest(ctx context.Context, excess int64) (int64, error) {
	var deleted int64
	err := l.writeTx(ctx, func(tx *sql.Tx) error {
		deleted = 0
		rows, err := tx.QueryContext(ctx, "SELECT rowid, LENGTH(blob) FROM log_chunks ORDER BY archived_us, rowid LIMIT ?", pruneBatch)
		if err != nil {
			return err
		}
		var ids []any
		var freed int64
		for rows.Next() && freed < excess {
			var id, size int64
			if err := rows.Scan(&id, &size); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			freed += size
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil || len(ids) == 0 {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM log_chunks WHERE rowid IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", ids...)
		if err != nil {
			return err
		}
		deleted, err = res.RowsAffected()
		return err
	})
	return deleted, err
}

// Compact returns pages freed by pruning to the filesystem, a step at a time,
// and truncates the WAL that the deletions grew.
func (l *LogDB) Compact(ctx context.Context) error {
	return l.writeConn(ctx, func(conn *sql.Conn) error {
		for {
			var free int64
			if err := conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
				return err
			}
			if free == 0 {
				break
			}
			if _, err := conn.ExecContext(ctx, "PRAGMA incremental_vacuum(1024)"); err != nil {
				return err
			}
			var after int64
			if err := conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&after); err != nil {
				return err
			}
			if after >= free {
				break // Auto-vacuum is off for this file; nothing can be released.
			}
		}
		_, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		return err
	})
}

// deleteChunks deletes every chunk selected by a rowid query whose last
// parameter is the batch size, one batch per transaction.
func (l *LogDB) deleteChunks(ctx context.Context, selectRows string, args ...any) (int64, error) {
	var total int64
	for {
		n, err := l.deleteChunkBatch(ctx, selectRows, args...)
		total += n
		if err != nil || n < pruneBatch {
			return total, err
		}
	}
}

func (l *LogDB) deleteChunkBatch(ctx context.Context, selectRows string, args ...any) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var n int64
	err := l.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM log_chunks WHERE rowid IN ("+selectRows+")", append(args, pruneBatch)...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

func (l *LogDB) deleteEmptyRuns(ctx context.Context) (int64, error) {
	var n int64
	err := l.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM log_runs WHERE NOT EXISTS (SELECT 1 FROM log_chunks WHERE log_chunks.run_id=log_runs.run_id)")
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// usedBytes is the size of the pages that hold data, excluding free pages.
func (l *LogDB) usedBytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	err := l.db.QueryRowContext(ctx, "SELECT p.page_count, f.freelist_count, s.page_size FROM pragma_page_count() p, pragma_freelist_count() f, pragma_page_size() s").Scan(&pages, &free, &size)
	return (pages - free) * size, err
}

// Stats reports archive totals for logging and diagnostics.
func (l *LogDB) Stats(ctx context.Context) (runs, chunks, blobBytes int64, err error) {
	// One snapshot and one chunk-table scan keep counts consistent with sizes.
	err = l.rdb.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM log_runs), COUNT(*), COALESCE(SUM(LENGTH(blob)),0)
		FROM log_chunks`).Scan(&runs, &chunks, &blobBytes)
	return runs, chunks, blobBytes, err
}
