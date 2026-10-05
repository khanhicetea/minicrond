package logdb

import (
	"context"
	"database/sql"
	"errors"
	"os"
)

// FileBytes reports what the archive occupies on disk: the database file and
// its write-ahead log. Both count against the filesystem; the WAL grows with
// deletions until Compact truncates it, so size accounting that ignores it
// under-reports exactly while pruning is under way. A missing WAL is zero.
func (l *LogDB) FileBytes() (db, wal int64, err error) {
	info, err := os.Stat(l.path)
	if err != nil {
		return 0, 0, err
	}
	db = info.Size()
	if info, err := os.Stat(l.path + "-wal"); err == nil {
		wal = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return db, 0, err
	}
	return db, wal, nil
}

// PruneResult describes a PruneOldest pass.
type PruneResult struct {
	Chunks int64 // chunk rows deleted
	Bytes  int64 // compressed blob bytes those rows held
	Runs   int64 // runs left without any chunk and removed
}

// PruneOldest deletes the oldest archived chunks (by archive time) until at
// least wantBytes of compressed blob data are gone or no eligible chunk is
// left. Chunks of runs for which protect returns true are never touched and
// are not counted; protect may be nil. Runs that end up with no chunks are
// removed. Unlike Prune and PruneToSize this is the disk-pressure path (3A):
// it ignores retention age on purpose.
//
// Deletion is in transactions of at most pruneBatch chunks, each through the
// ctx-aware write path, so a large reclamation never holds the writer for long
// and stops promptly when ctx ends. The rows' space returns to the filesystem
// only after Compact.
func (l *LogDB) PruneOldest(ctx context.Context, wantBytes int64, protect func(runID string) bool) (PruneResult, error) {
	var res PruneResult
	// A keyset cursor over (archived_us, rowid) visits each protected row once
	// per pass instead of once per batch.
	var cursorUS, cursorRow int64 = -1 << 62, -1 << 62
	for res.Bytes < wantBytes {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var batch PruneResult
		scanned := 0
		nextUS, nextRow := cursorUS, cursorRow
		err := l.writeTx(ctx, func(tx *sql.Tx) error {
			// writeTx repeats the function after a busy database; start from the
			// committed cursor each time.
			batch, scanned, nextUS, nextRow = PruneResult{}, 0, cursorUS, cursorRow
			rows, err := tx.QueryContext(ctx, `SELECT rowid, archived_us, run_id, LENGTH(blob) FROM log_chunks
				WHERE (archived_us, rowid) > (?, ?) ORDER BY archived_us, rowid LIMIT ?`, cursorUS, cursorRow, pruneBatch)
			if err != nil {
				return err
			}
			var ids []any
			for rows.Next() {
				var id, us, size int64
				var runID string
				if err := rows.Scan(&id, &us, &runID, &size); err != nil {
					rows.Close()
					return err
				}
				scanned++
				nextUS, nextRow = us, id
				if protect != nil && protect(runID) {
					continue
				}
				if res.Bytes+batch.Bytes >= wantBytes {
					break
				}
				ids = append(ids, id)
				batch.Bytes += size
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
			r, err := tx.ExecContext(ctx, "DELETE FROM log_chunks WHERE rowid IN ("+placeholders(len(ids))+")", ids...)
			if err != nil {
				return err
			}
			batch.Chunks, err = r.RowsAffected()
			return err
		})
		if err != nil {
			return res, err
		}
		cursorUS, cursorRow = nextUS, nextRow
		res.Chunks += batch.Chunks
		res.Bytes += batch.Bytes
		if scanned == 0 {
			break
		}
	}
	if res.Chunks > 0 {
		runs, err := l.deleteEmptyRuns(ctx)
		res.Runs = runs
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func placeholders(n int) string {
	b := make([]byte, 0, 2*n)
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}
