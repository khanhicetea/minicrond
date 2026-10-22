package executor

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/khanhicetea/minicrond/internal/sqlite"
	"github.com/khanhicetea/minicrond/internal/store"
)

// Execution first (ADR-16): scheduled jobs and supervised workers must keep
// running when the metadata database cannot take writes (full disk, read-only
// volume, a held lock, a stalled device). Run history is written behind
// execution: synchronously while storage is healthy, by the background history
// writer (finalizer.go) while it is not.

// storageHealth tracks whether execution-path metadata writes are failing.
type storageHealth struct {
	down  atomic.Bool
	since atomic.Int64 // unix nanoseconds when the current episode began
	// deferred counts runs admitted without a stored row; dropped counts
	// history records (a row, a running or a terminal state) that could not be
	// kept because the deferred-history budget was full. Process-lifetime.
	deferred atomic.Int64
	dropped  atomic.Int64
}

// createRunTimeout bounds the synchronous insert of a run row on the execution
// path. A stalled or locked database must not delay a launch by more than this.
const createRunTimeout = 2 * time.Second

func (s *Service) storageFailed(err error) {
	if s.health.down.CompareAndSwap(false, true) {
		s.health.since.Store(time.Now().UnixNano())
		slog.Error("metadata storage is failing; runs keep executing and their history is written when storage recovers", "error", err)
	}
}

func (s *Service) storageRecovered() {
	if s.health.down.CompareAndSwap(true, false) {
		since := time.Unix(0, s.health.since.Load())
		slog.Warn("metadata storage recovered; writing deferred run history", "degraded_for", time.Since(since).Round(time.Second).String())
	}
}

// StorageDegraded reports whether execution-path metadata writes are failing.
func (s *Service) StorageDegraded() bool { return s.health.down.Load() }

// History reports runs admitted without a stored row and history records lost
// because the deferred-history budget was full (process lifetime).
func (s *Service) History() (deferred, dropped int64) {
	return s.health.deferred.Load(), s.health.dropped.Load()
}

// storageUnavailable reports whether err from a write made on behalf of
// caller means the database is unavailable rather than that the write was
// refused. A canceled caller, a constraint violation (for example a scheduled
// occurrence that already has a row) and a refused transition are refusals.
func storageUnavailable(caller context.Context, err error) bool {
	if err == nil || caller.Err() != nil {
		return false
	}
	if sqlite.IsConstraint(err) || errors.Is(err, store.ErrInvalidTransition) || errors.Is(err, store.ErrIdempotencyConflict) {
		return false
	}
	return true
}
