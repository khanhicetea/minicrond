package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

// Relaxed commits (ADR-18). Some writes only have to survive a daemon crash,
// not a power loss or OS crash: after a reboot the value is either useless
// (a process ID from the previous boot) or rebuilt from rows that are
// committed with synchronous=FULL (the last scheduled fire). In WAL mode a
// synchronous=NORMAL commit reaches the OS before it returns, so it survives a
// daemon crash; it skips only the fsync. The next FULL commit or checkpoint
// makes it durable as well.

// restoreTimeout bounds switching the writer back to synchronous=FULL. The
// pragma takes no lock, so it only waits for the connection itself.
const restoreTimeout = 5 * time.Second

// relaxed runs fn on the writer connection with synchronous=NORMAL and then
// restores synchronous=FULL. When the store is configured with
// synchronous=NORMAL already, fn runs on the writer as is. If the restore
// fails, the connection is discarded so a relaxed setting can never leak into
// a later write; database/sql opens a replacement with the configured pragmas.
func (s *Store) relaxed(ctx context.Context, fn func(context.Context, *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if !s.syncFull || s.readOnly {
		return fn(ctx, conn)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA synchronous=NORMAL"); err != nil {
		discard(conn)
		return fmt.Errorf("relax synchronous: %w", err)
	}
	err = fn(ctx, conn)
	// Marked even when fn failed: a canceled or timed-out commit may still
	// have reached the WAL, and one extra sync costs one fsync.
	s.history.markDirty(s)
	rctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()
	if _, rerr := conn.ExecContext(rctx, "PRAGMA synchronous=FULL"); rerr != nil {
		discard(conn)
		return errors.Join(err, fmt.Errorf("restore synchronous=FULL: %w", rerr))
	}
	return err
}

// discard closes conn's driver connection instead of returning it to the pool.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// execRelaxedPrepared is execRelaxed for a constant statement that runs on
// every job run. The statement is prepared once and bound to a transaction on
// the pinned writer connection, so its SQL is not parsed and planned per run
// (about 8 µs of 19 µs per write in BenchmarkRelaxed on ext4). It is prepared
// before the connection is pinned: preparing needs that same connection.
func (s *Store) execRelaxedPrepared(ctx context.Context, query string, args ...any) (sql.Result, error) {
	stmt, err := s.stmts.Prepare(ctx, query)
	if err != nil {
		return s.execRelaxed(ctx, query, args...)
	}
	var res sql.Result
	err = s.relaxed(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if res, err = tx.StmtContext(ctx, stmt).ExecContext(ctx, args...); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		return tx.Commit()
	})
	return res, err
}

func (s *Store) execRelaxed(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := s.relaxed(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		res, err = conn.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

// Deferred history sync (ADR-23). With synchronous=FULL the
// run row (CreateRun) and the terminal state (FinishRunWithUsage) are also
// committed relaxed, like the ADR-18 writes, and made durable on a cadence:
// while any relaxed commit is not yet covered by a synced one, a single
// time.AfterFunc timer is armed; when it fires, it makes one small
// synchronous=FULL commit (historySyncSQL). Its WAL fsync covers every frame
// written before it, so all earlier relaxed commits become durable at once.
// Nothing runs while idle: the timer exists only while dirty, and a later
// relaxed commit re-arms it after it fired.
//
// Per-write FULL stays for everything else (definitions, alert channels,
// queue, idempotent admissions, worker state) and for history written with
// CreateRunSynced / FinishRunSynced (on_history_unavailable = "skip" jobs).
// A store configured with synchronous=NORMAL, a read-only store and
// MINICROND_HISTORY_SYNC=commit keep per-write FULL history commits.

// historySyncInterval is how long a relaxed history commit may stay
// unsynced (plus the duration of the sync itself and storage stalls).
const historySyncInterval = 2 * time.Second

// historySyncTimeout bounds one sync write, including the wait for the
// writer connection and SQLite's busy handler.
const historySyncTimeout = 5 * time.Second

// historySyncEnv selects the history durability mode at store open. The only
// recognized value is "commit": per-write FULL for run rows and terminal
// states, as before ADR-23. It is an escape hatch for A/B measurements.
const historySyncEnv = "MINICROND_HISTORY_SYNC"

// historySyncSQL is the sync write. It must change a page: SQLite does not
// fsync a transaction that wrote nothing.
const historySyncSQL = `INSERT INTO meta(key,value) VALUES('history_synced_us',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`

// historySync tracks relaxed commits that are not yet covered by a synced
// one. The zero value is disabled.
type historySync struct {
	deferred bool          // deferred history sync is on: history writes are relaxed
	interval time.Duration // historySyncInterval; tests shorten it

	// syncMu serializes sync writes with Close, so a timer callback never
	// writes to a closed database.
	syncMu sync.Mutex

	mu      sync.Mutex
	dirty   bool        // a relaxed commit may not be durable yet
	timer   *time.Timer // non-nil while armed
	closed  bool
	failing bool // the last sync failed; logged once per failure episode

	relaxedWrites, syncs, failures uint64
}

// HistorySyncStats is a snapshot of the deferred history sync counters.
type HistorySyncStats struct {
	Deferred      bool   // deferred history sync is on
	Dirty, Armed  bool   // unsynced relaxed commits exist; the timer is armed
	RelaxedWrites uint64 // run rows and terminal states committed relaxed
	Syncs         uint64 // successful sync commits
	Failures      uint64 // failed sync commits
}

// HistorySyncStats reports the deferred history sync state (diagnostics and
// tests).
func (s *Store) HistorySyncStats() HistorySyncStats {
	h := &s.history
	h.mu.Lock()
	defer h.mu.Unlock()
	return HistorySyncStats{Deferred: h.deferred, Dirty: h.dirty, Armed: h.timer != nil, RelaxedWrites: h.relaxedWrites, Syncs: h.syncs, Failures: h.failures}
}

// newHistorySync decides the mode once, at store open.
func newHistorySync(syncFull, readOnly bool) historySync {
	deferred := syncFull && !readOnly
	if v := os.Getenv(historySyncEnv); v != "" {
		if v == "commit" {
			deferred = false
		} else {
			slog.Warn("ignoring unknown "+historySyncEnv+" value; only \"commit\" is recognized", "value", v)
		}
	}
	return historySync{deferred: deferred, interval: historySyncInterval}
}

// markDirty records a relaxed commit and arms the timer if it is not armed.
// It is called after the commit, so a sync that clears dirty before this call
// already covered the commit, and one that runs later is armed here.
func (h *historySync) markDirty(s *Store) {
	if !h.deferred {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dirty = true
	h.armLocked(s)
}

func (h *historySync) armLocked(s *Store) {
	if h.timer == nil && !h.closed {
		h.timer = time.AfterFunc(h.interval, func() { s.syncHistory(false) })
	}
}

// syncHistory makes every earlier relaxed commit durable with one FULL
// commit. It runs on the timer, and with final set from Close. A failed sync
// leaves the store dirty and re-arms the timer.
func (s *Store) syncHistory(final bool) error {
	h := &s.history
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	h.mu.Lock()
	if !final {
		h.timer = nil
	}
	if !h.dirty || (h.closed && !final) {
		h.mu.Unlock()
		return nil
	}
	// Cleared before the write: a relaxed commit that lands while the sync
	// runs marks dirty again and arms the next sync.
	h.dirty = false
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), historySyncTimeout)
	defer cancel()
	// The pool's writer is always at the configured synchronous=FULL here:
	// relaxed restores it or discards the connection.
	_, err := s.stmts.Exec(ctx, historySyncSQL, strconv.FormatInt(time.Now().UnixMicro(), 10))

	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.failures++
		h.dirty = true
		if !final {
			h.armLocked(s)
		}
		if !h.failing {
			h.failing = true
			slog.Error("syncing deferred run history failed; recent run rows and terminal states survive a daemon crash but not yet a power loss; retrying", "error", err, "retry_in", h.interval)
		}
		return fmt.Errorf("sync deferred run history: %w", err)
	}
	h.syncs++
	if h.failing {
		h.failing = false
		slog.Info("syncing deferred run history recovered")
	}
	return nil
}

// closeHistory stops the timer and performs a final sync if dirty. Later
// relaxed commits are not synced on a cadence (the store is closing).
func (s *Store) closeHistory() error {
	h := &s.history
	if !h.deferred {
		return nil
	}
	h.mu.Lock()
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.mu.Unlock()
	return s.syncHistory(true)
}

// execHistory commits a per-run history write: relaxed under deferred history
// sync unless synced is set, else with the configured durability.
func (s *Store) execHistory(ctx context.Context, synced bool, query string, args ...any) (sql.Result, error) {
	if synced || !s.history.deferred {
		return s.stmts.Exec(ctx, query, args...)
	}
	res, err := s.execRelaxedPrepared(ctx, query, args...)
	s.history.mu.Lock()
	s.history.relaxedWrites++
	s.history.mu.Unlock()
	return res, err
}
