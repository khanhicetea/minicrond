package executor

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
	"github.com/khanhicetea/minicrond/internal/store"
)

// finalItem is a run whose history could not be written when it happened
// (ADR-16). Execution never waits for it: the run was started, supervised and
// announced from memory; this item only carries the row to the database once
// it accepts writes again. Stages are written in order: the pending row (when
// even the insert failed), the running transition, then the terminal state.
type finalItem struct {
	r        model.Run // the pending row as admitted
	d        model.Definition
	create   bool // the pending row was never stored
	start    bool // the running transition is not stored yet (see started)
	started  startInfo
	done     bool // t is valid: the process has stopped
	t        terminal
	token    uint64 // persistLag pending token (terminal items only)
	backoff  time.Duration
	nextTry  time.Time
	attempts int
}

// startInfo is the running transition of a spawned process.
type startInfo struct {
	pid, pgid int
	id        string
	at        time.Time
}

// finalState is the single background history writer. Its items live in
// Service.finalizing (guarded by Service.mu), at most one per run.
//
// Bounds: at most QueueOptions.MaxFinalizers items. When the map is full the
// history of the next run is not kept (counted in historyDropped and logged);
// its execution, retries, alerts and supervision are unaffected. Nothing on the
// execution path waits for this writer or holds a capacity slot for it.
type finalState struct {
	started bool // guarded by Service.mu
	wake    chan struct{}
}

const (
	finalizerBackoffMin = time.Second
	finalizerBackoffMax = 30 * time.Second
	// finalizerFailStreak ends a pass: that many consecutive failures mean
	// storage is down, so the rest wait for the next (global backoff) pass
	// instead of each hitting the database.
	finalizerFailStreak = 3
	// historyWriteTimeout bounds one background history write.
	historyWriteTimeout = 5 * time.Second
)

// spool registers or updates the deferred history of run id. update runs under
// s.mu on the existing item, or on a new one built from r and d. It reports
// false when the daemon is closing or the map is full; the caller then counts
// the run's history as dropped.
func (s *Service) spool(r model.Run, d model.Definition, update func(*finalItem)) bool {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return false
	}
	it, ok := s.finalizing[r.ID]
	if !ok {
		if len(s.finalizing) >= s.queueOpt.MaxFinalizers {
			s.mu.Unlock()
			return false
		}
		it = &finalItem{r: r.Clone(), d: d.Clone(), backoff: finalizerBackoffMin, nextTry: time.Now()}
		if s.health.down.Load() {
			// Storage is known to be failing: wait for the global backoff
			// pass instead of an immediate attempt.
			it.nextTry = time.Now().Add(finalizerBackoffMin)
		}
		s.finalizing[r.ID] = it
	}
	hadTerminal := it.done
	update(it)
	if it.done && !hadTerminal {
		it.token = s.persistLag.startPending(it.t.ended)
	}
	s.mu.Unlock()
	if !s.startLoop(&s.finals.started, s.finalizeLoop) {
		s.dropFinalizer(r.ID)
		return false
	}
	signal(s.finals.wake)
	return true
}

// spooled reports whether run id has deferred history; update, when non-nil,
// is applied to the item under s.mu.
func (s *Service) spooled(id string, update func(*finalItem)) bool {
	s.mu.Lock()
	it, ok := s.finalizing[id]
	if ok && update != nil {
		hadTerminal := it.done
		update(it)
		if it.done && !hadTerminal {
			it.token = s.persistLag.startPending(it.t.ended)
		}
	}
	s.mu.Unlock()
	if ok && update != nil {
		signal(s.finals.wake)
	}
	return ok
}

// historyLost records that a run's history could not be kept at all.
func (s *Service) historyLost(r model.Run, what string) {
	n := s.health.dropped.Add(1)
	slog.Error("run history not recorded: metadata storage is failing and the deferred-history budget is full; execution continues", "run", r.ID, "job", r.Job, "missing", what, "limit", s.queueOpt.MaxFinalizers, "dropped_total", n)
}

// Finalizers reports runs whose history waits for the background writer. The
// second value is kept for diagnostics compatibility and is always zero: no run
// finalizes inline while holding a capacity slot any more.
func (s *Service) Finalizers() (background, inline int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.finalizing), 0
}

func (s *Service) finalizeLoop() {
	var idle idleTimer
	defer idle.stop()
	global := finalizerBackoffMin
	for {
		if s.isClosing() {
			s.abandonFinalizers()
			return
		}
		var failedEarly bool
		if err := fault.Call(func() error { failedEarly = s.finalizePass(); return nil }); err != nil {
			logFailure("run history writer panicked", err)
			failedEarly = true
		}
		var wait time.Duration
		if failedEarly {
			// Storage is failing across items: one global backoff.
			wait = global
			global = min(global*2, finalizerBackoffMax)
		} else {
			global = finalizerBackoffMin
			wait = s.untilNextFinalizer()
		}
		if wait == 0 {
			continue
		}
		select {
		case <-s.stopping:
			s.abandonFinalizers()
			return
		case <-s.finals.wake:
		case <-idle.arm(wait):
		}
	}
}

// untilNextFinalizer is how long until the earliest pending item is due; < 0
// means none are pending.
func (s *Service) untilNextFinalizer() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := time.Duration(-1)
	for _, it := range s.finalizing {
		d := max(time.Until(it.nextTry), 0)
		if next < 0 || d < next {
			next = d
		}
	}
	return next
}

// finalItemView is a snapshot of an item taken under s.mu; the writes happen
// without the lock.
type finalItemView struct {
	item          *finalItem
	r             model.Run
	create, start bool
	started       startInfo
	done          bool
	t             terminal
}

// finalizePass tries every due item once, oldest first. Each item has its own
// backoff, so one that keeps failing (a poison item) does not starve the rest;
// finalizerFailStreak consecutive failures end the pass early (storage down).
func (s *Service) finalizePass() (failedEarly bool) {
	now := time.Now()
	s.mu.Lock()
	views := make([]finalItemView, 0, len(s.finalizing))
	for _, it := range s.finalizing {
		if !it.nextTry.After(now) {
			views = append(views, finalItemView{item: it, r: it.r, create: it.create, start: it.start, started: it.started, done: it.done, t: it.t})
		}
	}
	s.mu.Unlock()
	slices.SortFunc(views, func(a, b finalItemView) int { return a.r.QueuedAt.Compare(b.r.QueuedAt) })
	streak := 0
	for _, v := range views {
		if s.isClosing() {
			return false
		}
		err := s.writeHistory(v)
		switch {
		case err == nil:
			s.storageRecovered()
			streak = 0
		case errors.Is(err, store.ErrInvalidTransition), errors.Is(err, errHistoryConflict):
			logFailure("deferred run history rejected", err, "run", v.r.ID)
			s.dropFinalizer(v.r.ID)
			streak = 0
		default:
			s.storageFailed(err)
			s.mu.Lock()
			v.item.attempts++
			v.item.backoff = min(v.item.backoff*2, finalizerBackoffMax)
			v.item.nextTry = time.Now().Add(v.item.backoff)
			attempts, backoff := v.item.attempts, v.item.backoff
			s.mu.Unlock()
			logFailure("writing deferred run history failed; retrying", err, "run", v.r.ID, "attempts", attempts, "retry_in", backoff)
			if streak++; streak >= finalizerFailStreak {
				return true
			}
		}
	}
	return false
}

var errHistoryConflict = errors.New("another run already recorded this scheduled occurrence")

// writeHistory writes the stages of one snapshot and then retires what it
// wrote. Stages a concurrent update added after the snapshot stay pending.
func (s *Service) writeHistory(v finalItemView) error {
	ctx, cancel := context.WithTimeout(s.stopCtx, historyWriteTimeout)
	defer cancel()
	if v.create {
		if err := s.store.CreateRun(ctx, v.r); err != nil {
			if !sqlite.IsConstraint(err) {
				return err
			}
			// A previous attempt may have committed before reporting an error.
			// Otherwise the row is a duplicate scheduled occurrence.
			if _, lookupErr := s.store.Run(ctx, v.r.ID); lookupErr != nil {
				return errors.Join(errHistoryConflict, err)
			}
		}
		s.mu.Lock()
		v.item.create = false
		s.mu.Unlock()
		slog.Info("recorded deferred run", "run", v.r.ID, "job", v.r.Job)
		if s.onRunStored != nil {
			if err := fault.Call(func() error { s.onRunStored(v.r.ID); return nil }); err != nil {
				logFailure("run-stored callback panicked", err, "run", v.r.ID)
			}
		}
	}
	if v.start {
		st := v.started
		if err := s.store.StartRun(ctx, v.r.ID, st.pid, st.pgid, st.id, st.at); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
			return err
		}
		s.mu.Lock()
		v.item.start = false
		s.mu.Unlock()
	}
	if v.done {
		t := v.t
		if s.finishHook != nil {
			if err := s.finishHook(v.r.ID); err != nil {
				return err
			}
		}
		if err := s.store.FinishRunWithUsage(ctx, v.r.ID, t.status, t.reason, t.code, t.signal, t.ended, t.bytes, t.truncated, t.usage); err != nil {
			return err
		}
		slog.Info("persisted terminal run state after retry", "run", v.r.ID, "status", t.status)
		s.persistLag.observe(t.ended)
	}
	s.mu.Lock()
	it := s.finalizing[v.r.ID]
	retire := it == v.item && !it.create && !it.start && (v.done || !it.done)
	if retire {
		if it.done {
			s.persistLag.endPending(it.token)
		}
		delete(s.finalizing, v.r.ID)
	}
	s.mu.Unlock()
	if !retire {
		signal(s.finals.wake) // a stage arrived meanwhile
	}
	return nil
}

func (s *Service) dropFinalizer(id string) {
	s.mu.Lock()
	if it, ok := s.finalizing[id]; ok {
		if it.done {
			s.persistLag.endPending(it.token)
		}
		delete(s.finalizing, id)
	}
	s.mu.Unlock()
}

func (s *Service) abandonFinalizers() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.finalizing))
	for id, it := range s.finalizing {
		ids = append(ids, id)
		if it.done {
			s.persistLag.endPending(it.token)
		}
	}
	clear(s.finalizing)
	s.mu.Unlock()
	for _, id := range ids {
		slog.Warn("run history left unwritten at shutdown; a stored nonterminal row is marked interrupted at the next start", "run", id)
	}
}
