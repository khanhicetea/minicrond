package logdb

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

// FileBytes reports what the archive occupies on disk: the database file and
// its write-ahead log (archived file chunks are reported by ChunkFileBytes). Both count against the filesystem; the WAL grows with
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

// ChunkFileBytes sums the sizes of the archived file chunks on disk, by
// listing the archive directory (on-demand and pressure-pass accounting).
func (l *LogDB) ChunkFileBytes() int64 {
	runs, err := os.ReadDir(l.files)
	if err != nil {
		return 0
	}
	var total int64
	for _, r := range runs {
		if !r.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(l.files, r.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if info, err := f.Info(); err == nil && info.Mode().IsRegular() {
				total += info.Size()
			}
		}
	}
	return total
}

// PruneResult describes a PruneOldest pass.
type PruneResult struct {
	Chunks int64 // chunk rows deleted
	Bytes  int64 // compressed blob and file bytes those rows held
	Runs   int64 // runs left without any chunk and removed
}

// PruneOldest deletes the oldest archived chunks (by archive time) until at
// least wantBytes of compressed blob data are gone or no eligible chunk is
// left. Chunks for which protect(runID, number) returns true are never touched
// and are not counted; protect may be nil. Runs that end up with no chunks are
// removed. Unlike Prune and PruneToSize this is the disk-pressure path (3A):
// it ignores retention age on purpose.
//
// Deletion is in transactions of at most pruneBatch chunks, each through the
// ctx-aware write path, so a large reclamation never holds the writer for long
// and stops promptly when ctx ends. The rows' space returns to the filesystem
// only after Compact.
func (l *LogDB) PruneOldest(ctx context.Context, wantBytes int64, protect func(runID string, number int) bool) (PruneResult, error) {
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
		var gone []fileRef
		err := l.writeTx(ctx, func(tx *sql.Tx) error {
			// writeTx repeats the function after a busy database; start from the
			// committed cursor each time.
			batch, scanned, nextUS, nextRow, gone = PruneResult{}, 0, cursorUS, cursorRow, nil
			rows, err := tx.QueryContext(ctx, `SELECT rowid, archived_us, run_id, number, LENGTH(blob)+file_bytes FROM log_chunks
				WHERE (archived_us, rowid) > (?, ?) ORDER BY archived_us, rowid LIMIT ?`, cursorUS, cursorRow, pruneBatch)
			if err != nil {
				return err
			}
			var ids []any
			for rows.Next() {
				var id, us, size int64
				var runID string
				var number int
				if err := rows.Scan(&id, &us, &runID, &number, &size); err != nil {
					rows.Close()
					return err
				}
				scanned++
				nextUS, nextRow = us, id
				if protect != nil && protect(runID, number) {
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
			rows, err = tx.QueryContext(ctx, "DELETE FROM log_chunks WHERE rowid IN ("+placeholders(len(ids))+") RETURNING run_id, number, file_bytes", ids...)
			if err != nil {
				return err
			}
			batch.Chunks, gone, err = scanDeleted(rows)
			return err
		})
		if err != nil {
			return res, err
		}
		l.removeFiles(gone)
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

// TailStart returns the lowest chunk number of runID's newest archived chunks
// that together hold at least keepBytes of compressed data. The newest chunk is
// always part of the tail. ok is false when the run has no archived chunk.
// Disk pressure uses it to keep a running worker's recent output (ADR-10).
func (l *LogDB) TailStart(ctx context.Context, runID string, keepBytes int64) (number int, ok bool, err error) {
	rows, err := l.rdb.QueryContext(ctx, "SELECT number, LENGTH(blob)+file_bytes FROM log_chunks WHERE run_id=? ORDER BY number DESC", runID)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var kept int64
	for rows.Next() {
		var size int64
		if err := rows.Scan(&number, &size); err != nil {
			return 0, false, err
		}
		ok = true
		kept += size
		if kept >= keepBytes {
			break
		}
	}
	return number, ok, rows.Err()
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
