package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/logstore"
)

// Log disk budget wiring (ADR-10). The policy lives in the log store; the daemon
// only supplies it from configuration and decides when a pass runs:
//
//   - the existing maintenance cadences (hourly retention, the worker flush
//     tick, the daily log prune), and
//   - a coalesced hint from the log store when a chunk rotates, a run seals or
//     capture fails, handled by diskBudgetLoop.
//
// The loop blocks on a one-slot channel, so there is no timer or wakeup while
// nothing happens; after a pass it waits out diskCheckCooldown (a timer that
// exists only then) so a busy daemon runs at most one pass per cooldown.
var diskCheckCooldown = 10 * time.Second

// applyDiskPolicy installs the configured limits in the log store.
func (d *Daemon) applyDiskPolicy(logs config.Logs) {
	d.logs.SetDiskPolicy(&logstore.DiskPolicy{
		Dir:          d.DataDir,
		BudgetBytes:  logs.DiskBudgetBytes(),
		MinFreeBytes: logs.DiskMinFreeBytes(),
	})
}

// requestDiskCheck asks diskBudgetLoop for a pass. It never blocks and is safe
// to call from capture paths.
func (d *Daemon) requestDiskCheck() {
	select {
	case d.diskHint <- struct{}{}:
	default:
	}
}

// diskBudgetLoop runs passes on request until ctx ends.
func (d *Daemon) diskBudgetLoop(ctx context.Context) {
	d.requestDiskCheck() // one pass at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.diskHint:
		}
		d.enforceDiskBudget(ctx)
		cooldown := time.NewTimer(diskCheckCooldown)
		select {
		case <-ctx.Done():
			cooldown.Stop()
			return
		case <-cooldown.C:
		}
	}
}

// enforceDiskBudget runs one pass and records its duration. Without a policy
// (unit tests, or a store used standalone) it does nothing.
func (d *Daemon) enforceDiskBudget(ctx context.Context) {
	if d.logs == nil {
		return
	}
	start := time.Now()
	st, err := d.logs.EnforceDiskBudget(ctx)
	if err != nil && ctx.Err() == nil {
		slog.Error("log disk budget pass failed", "error", err)
	}
	if st.Enabled {
		d.logs.RecordMaintenance("disk_budget", time.Since(start))
	}
}

// purgeQuarantine applies the opt-in quarantine policy (default: keep all).
func (d *Daemon) purgeQuarantine(ctx context.Context) {
	if d.logs == nil || d.cfg == nil {
		return
	}
	d.mu.Lock()
	cfg := d.cfg.Logs
	d.mu.Unlock()
	policy := logstore.QuarantinePolicy{KeepFor: cfg.QuarantineKeepDuration(), MaxBytes: cfg.QuarantineMaxBytes()}
	if !policy.Enabled() {
		return
	}
	defer d.timed("quarantine_purge")()
	if _, err := d.logs.PurgeQuarantine(ctx, policy, time.Now()); err != nil && ctx.Err() == nil {
		slog.Error("quarantine purge failed", "error", err)
	}
}

// timed returns a function that records how long a named maintenance task took:
//
//	defer d.timed("retention")()
func (d *Daemon) timed(name string) func() {
	start := time.Now()
	return func() {
		if d.logs != nil {
			d.logs.RecordMaintenance(name, time.Since(start))
		}
	}
}
