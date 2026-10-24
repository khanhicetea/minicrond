package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/khanhicetea/minicrond/internal/store"
)

// maxParkedAlerts bounds alert delivery observations held for runs whose row
// is still in the deferred history writer (ADR-16). About 200 bytes each.
const maxParkedAlerts = 256

// alertPark keeps delivery observations of runs whose row is not stored yet,
// so they are written after the row instead of failing alert_deliveries'
// run foreign key. Nothing polls it: the history writer's run-stored callback
// flushes a run's observations. Observations whose run is not pending in the
// writer (history dropped, run deleted) are discarded; so is the overflow
// beyond maxParkedAlerts, which is counted in the daemon log.
type alertPark struct {
	store      *store.Store
	rowPending func(runID string) bool // the run's row is waiting in the history writer
	mu         sync.Mutex
	byRun      map[string][]store.AlertUpdate
	count      int
}

func newAlertPark(st *store.Store, rowPending func(string) bool) *alertPark {
	return &alertPark{store: st, rowPending: rowPending, byRun: make(map[string][]store.AlertUpdate)}
}

// record writes updates whose run is stored and parks the rest.
func (p *alertPark) record(ctx context.Context, updates []store.AlertUpdate) error {
	now := time.Now().UTC()
	for i := range updates {
		if updates[i].At.IsZero() {
			updates[i].At = now
		}
	}
	missing, err := p.store.RecordAlertsForStoredRuns(ctx, updates)
	if err != nil {
		return err
	}
	p.park(ctx, missing)
	return nil
}

func (p *alertPark) park(ctx context.Context, missing []store.AlertUpdate) {
	if len(missing) == 0 {
		return
	}
	var discarded, overflow int
	var recheck []string
	p.mu.Lock()
	for _, u := range missing {
		// Checked under p.mu: the writer clears the pending flag before its
		// run-stored callback takes p.mu, so a parked observation is always
		// flushed by that callback or by the retry below.
		if !p.rowPending(u.RunID) {
			// Either the history writer just stored the row (retry once
			// below) or the run's history is gone.
			if len(p.byRun[u.RunID]) == 0 {
				recheck = append(recheck, u.RunID)
			}
			p.byRun[u.RunID] = append(p.byRun[u.RunID], u)
			p.count++
			continue
		}
		if p.count >= maxParkedAlerts {
			overflow++
			continue
		}
		p.byRun[u.RunID] = append(p.byRun[u.RunID], u)
		p.count++
	}
	p.mu.Unlock()
	if overflow > 0 {
		slog.Warn("alert delivery observations discarded: too many runs awaiting deferred history", "count", overflow, "limit", maxParkedAlerts)
	}
	for _, id := range recheck {
		discarded += p.flush(ctx, id)
	}
	if discarded > 0 {
		slog.Info("alert delivery observations not recorded: their run has no stored history", "count", discarded)
	}
}

// runStored is the history writer's callback: the run's row now exists.
func (p *alertPark) runStored(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n := p.flush(ctx, runID); n > 0 {
		slog.Info("alert delivery observations not recorded after their run was stored", "run", runID, "count", n)
	}
}

// flush writes the parked observations of runID and reports how many could
// not be stored (discarded).
func (p *alertPark) flush(ctx context.Context, runID string) int {
	p.mu.Lock()
	updates := p.byRun[runID]
	delete(p.byRun, runID)
	p.count -= len(updates)
	p.mu.Unlock()
	if len(updates) == 0 {
		return 0
	}
	missing, err := p.store.RecordAlertsForStoredRuns(ctx, updates)
	if err != nil {
		slog.Warn("recording parked alert delivery observations failed", "run", runID, "count", len(updates), "error", err)
		return len(updates)
	}
	return len(missing)
}
