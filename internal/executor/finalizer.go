package executor

import (
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// finalItem is a run whose process has stopped but whose terminal state could
// not be written yet.
type finalItem struct {
	r        model.Run
	d        model.Definition
	t        terminal
	token    uint64 // persistLag pending token
	backoff  time.Duration
	nextTry  time.Time
	attempts int
}

// finalState is the single background finalizer. Its items live in
// Service.finalizing (guarded by Service.mu), at most one per run.
//
// Bounds: a job whose terminal write fails joins the map while it has fewer than
// QueueOptions.MaxFinalizers entries. Beyond that it retries inline for at most
// QueueOptions.InlineHold, keeping its capacity slot (backpressure on
// admission), and is then handed to the map anyway, up to the hard cap of
// finalizerHardFactor x MaxFinalizers; past the hard cap it keeps holding its
// slot. Workers always join the map (they hold no slot and the supervisor waits
// for them), so the map exceeds the hard cap by at most the worker count.
type finalState struct {
	started bool // guarded by Service.mu
	wake    chan struct{}
	inline  atomic.Int64 // jobs finalizing inline because the soft cap was full
}

const (
	finalizerBackoffMin = time.Second
	finalizerBackoffMax = 30 * time.Second
	// finalizerHardFactor scales MaxFinalizers into the cap for hand-offs.
	finalizerHardFactor = 4
	// finalizerFailStreak ends a pass: that many consecutive failures mean
	// storage is down, so the rest wait for the next (global backoff) pass
	// instead of each hitting the database.
	finalizerFailStreak = 3
	defaultInlineHold   = 2 * time.Minute
)

type finalizerResult int

const (
	finalizerQueued finalizerResult = iota
	finalizerClosing
	finalizerFull
)

type finalizerMode int

const (
	// finalizerSoft joins the map below the soft cap (jobs, first attempt).
	finalizerSoft finalizerMode = iota
	// finalizerHandoff joins below the hard cap (jobs that held a slot too long).
	finalizerHandoff
	// finalizerWorker always joins.
	finalizerWorker
)

// addFinalizer registers a run for background finalization. It is registered
// before the run's done channel closes, which supervisors use to tell a pending
// background finalization from an abandoned one. When the map is full the
// caller keeps the work (finalizeInline), so nothing is ever discarded.
func (s *Service) addFinalizer(r model.Run, d model.Definition, t terminal, mode finalizerMode) finalizerResult {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return finalizerClosing
	}
	limit := s.queueOpt.MaxFinalizers
	switch mode {
	case finalizerHandoff:
		limit *= finalizerHardFactor
	case finalizerWorker:
		limit = int(^uint(0) >> 1)
	}
	if _, ok := s.finalizing[r.ID]; !ok && len(s.finalizing) >= limit {
		s.mu.Unlock()
		return finalizerFull
	}
	s.finalizing[r.ID] = &finalItem{r: r, d: d, t: t, token: s.persistLag.startPending(t.ended), backoff: finalizerBackoffMin, nextTry: time.Now().Add(finalizerBackoffMin)}
	s.mu.Unlock()
	if !s.startLoop(&s.finals.started, s.finalizeLoop) {
		s.dropFinalizer(r.ID)
		return finalizerClosing
	}
	signal(s.finals.wake)
	return finalizerQueued
}

// Finalizers reports runs awaiting background finalization and jobs
// finalizing inline because the soft cap was exhausted.
func (s *Service) Finalizers() (background, inline int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.finalizing), int(s.finals.inline.Load())
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
			logFailure("run finalizer panicked", err)
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

// finalizePass tries every due item once, oldest first. Each item has its own
// backoff, so one that keeps failing (a poison item) does not starve the rest;
// finalizerFailStreak consecutive failures end the pass early (storage down).
func (s *Service) finalizePass() (failedEarly bool) {
	now := time.Now()
	s.mu.Lock()
	items := make([]*finalItem, 0, len(s.finalizing))
	for _, it := range s.finalizing {
		if !it.nextTry.After(now) {
			items = append(items, it)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(items, func(a, b *finalItem) int { return a.t.ended.Compare(b.t.ended) })
	streak := 0
	for _, it := range items {
		if s.isClosing() {
			return false
		}
		t := it.t
		err := s.finishRun(it.r.ID, t.status, t.reason, t.code, t.signal, t.ended, t.bytes, t.truncated)
		switch {
		case err == nil:
			slog.Info("persisted terminal run state after retry", "run", it.r.ID, "status", t.status)
			s.dropFinalizer(it.r.ID)
			s.finished(it.r, it.d, t)
			streak = 0
		case errors.Is(err, store.ErrInvalidTransition):
			logFailure("persisting terminal run state rejected", err, "run", it.r.ID, "status", t.status)
			s.dropFinalizer(it.r.ID)
			streak = 0
		default:
			it.attempts++
			it.backoff = min(it.backoff*2, finalizerBackoffMax)
			s.mu.Lock()
			it.nextTry = time.Now().Add(it.backoff)
			s.mu.Unlock()
			logFailure("persisting terminal run state failed; retrying", err, "run", it.r.ID, "status", t.status, "attempts", it.attempts, "retry_in", it.backoff)
			if streak++; streak >= finalizerFailStreak {
				return true
			}
		}
	}
	return false
}

func (s *Service) dropFinalizer(id string) {
	s.mu.Lock()
	if it, ok := s.finalizing[id]; ok {
		s.persistLag.endPending(it.token)
		delete(s.finalizing, id)
	}
	s.mu.Unlock()
}

func (s *Service) abandonFinalizers() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.finalizing))
	for id, it := range s.finalizing {
		ids = append(ids, id)
		s.persistLag.endPending(it.token)
	}
	clear(s.finalizing)
	s.mu.Unlock()
	for _, id := range ids {
		slog.Warn("run left unfinalized at shutdown; recovery will mark it interrupted", "run", id)
	}
}

// finalizeInline retries a job's terminal write in the completing goroutine,
// which keeps the run's capacity slot. After InlineHold it hands the item to the
// background map (up to the hard cap) and returns, releasing the slot. It also
// ends at shutdown.
func (s *Service) finalizeInline(r model.Run, d model.Definition, t terminal) {
	defer s.persistLag.endPending(s.persistLag.startPending(t.ended))
	hold := s.queueOpt.InlineHold
	deadline := time.Now().Add(hold)
	backoff := finalizerBackoffMin
	for {
		timer := time.NewTimer(backoff)
		select {
		case <-s.stopping:
			timer.Stop()
			slog.Warn("run left unfinalized at shutdown; recovery will mark it interrupted", "run", r.ID)
			return
		case <-timer.C:
		}
		err := s.finishRun(r.ID, t.status, t.reason, t.code, t.signal, t.ended, t.bytes, t.truncated)
		if err == nil {
			slog.Info("persisted terminal run state after retry", "run", r.ID, "status", t.status)
			s.finished(r, d, t)
			return
		}
		if errors.Is(err, store.ErrInvalidTransition) {
			logFailure("persisting terminal run state rejected", err, "run", r.ID, "status", t.status)
			return
		}
		backoff = min(backoff*2, finalizerBackoffMax)
		logFailure("persisting terminal run state failed; retrying", err, "run", r.ID, "status", t.status, "retry_in", backoff)
		if time.Now().After(deadline) {
			switch s.addFinalizer(r, d, t, finalizerHandoff) {
			case finalizerQueued, finalizerClosing:
				slog.Warn("terminal write still failing; handed to the background finalizer and released the capacity slot", "run", r.ID, "held", hold)
				return
			}
			// Hard cap reached: keep holding the slot (documented backpressure).
			deadline = time.Now().Add(hold)
		}
	}
}
