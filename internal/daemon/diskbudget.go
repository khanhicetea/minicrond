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
//     tick, the daily log prune), which only call requestDiskCheck, and
//   - a coalesced hint from the log store when a chunk rotates, a run seals or
//     capture fails, handled by diskBudgetLoop.
//
// The loop blocks on a one-slot channel, so there is no timer or wakeup while
// nothing happens. A pass never runs inline in another maintenance loop, so a
// slow pass cannot delay a worker flush or a retention sweep. After a pass the
// loop waits out diskCheckCooldown (a timer that exists only then), so every
// request, whatever its source, yields at most one pass per cooldown.
var diskCheckCooldown = 10 * time.Second

// applyDiskPolicy installs the configured limits in the log store.
func (d *Daemon) applyDiskPolicy(logs config.Logs) {
	d.logs.SetDiskPolicy(&logstore.DiskPolicy{
		Dir:          d.DataDir,
		BudgetBytes:  logs.DiskBudgetBytes(),
		MinFreeBytes: logs.DiskMinFreeBytes(),
	})
	d.logDiskPolicy(logs)
}

// logDiskPolicy states, at startup and on every reload, what the log disk budget
// will do, so a deletion of logs is never the first sign of the setting (the
// headroom rule is on by default). The effective headroom is the configured one
// after clamping to a quarter of the filesystem.
func (d *Daemon) logDiskPolicy(logs config.Logs) {
	attrs := []any{
		"min_free_mib", logs.DiskMinFreeBytes() >> 20,
		"budget_mib", logs.DiskBudgetBytes() >> 20,
		"quarantine_keep_for_days", logs.QuarantineKeepFor,
		"quarantine_max_mib", logs.QuarantineMaxBytes() >> 20,
	}
	p, total, ok, err := d.logs.EffectiveDiskPolicy()
	if ok {
		attrs = append(attrs, "effective_min_free_mib", p.MinFreeBytes>>20, "filesystem_mib", total>>20)
		if err != nil {
			attrs = append(attrs, "statfs_error", err.Error())
		}
	}
	slog.Info("log disk budget: the oldest completed runs' logs are deleted before their retention age when free space on the data directory falls below min_free or log bytes exceed budget (0 = off); quarantine is kept unless a purge policy is set", attrs...)
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
	if hook := d.diskPassHook; hook != nil {
		hook(ctx)
		return
	}
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
