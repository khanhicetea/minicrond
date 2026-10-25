package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
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

func (s *Store) execRelaxed(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := s.relaxed(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		res, err = conn.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}
