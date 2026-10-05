package executor

import (
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// finalItem is a run whose process has stopped but whose terminal state could
// not be written yet.
type finalItem struct {
	r model.Run
	d model.Definition
	t terminal
}

// finalState is the single background finalizer. Its items live in
// Service.finalizing (guarded by Service.mu), at most one per run and at most
// QueueOptions.MaxFinalizers in total.
type finalState struct {
	started bool // guarded by Service.mu
	wake    chan struct{}
	inline  atomic.Int64 // runs finalizing inline because the budget was full
}

const (
	finalizerBackoffMin = time.Second
	finalizerBackoffMax = 30 * time.Second
)

type finalizerResult int

const (
	finalizerQueued finalizerResult = iota
	finalizerClosing
	finalizerFull
)

// addFinalizer registers a run for background finalization. It is registered
// before the run's done channel closes, which supervisors use to tell a pending
// background finalization from an abandoned one. When the budget is full the
// caller keeps the work (finalizeInline), so nothing is ever discarded and the
// overload becomes backpressure on capacity.
func (s *Service) addFinalizer(r model.Run, d model.Definition, t terminal) finalizerResult {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return finalizerClosing
	}
	if _, ok := s.finalizing[r.ID]; !ok && len(s.finalizing) >= s.queueOpt.MaxFinalizers {
		s.mu.Unlock()
		return finalizerFull
	}
	s.finalizing[r.ID] = &finalItem{r: r, d: d, t: t}
	s.mu.Unlock()
	if !s.startLoop(&s.finals.started, s.finalizeLoop) {
		return finalizerClosing
	}
	signal(s.finals.wake)
	return finalizerQueued
}

// Finalizers reports runs awaiting background finalization and runs finalizing
// inline because the budget was exhausted.
func (s *Service) Finalizers() (background, inline int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.finalizing), int(s.finals.inline.Load())
}

func (s *Service) finalizeLoop() {
	var idle idleTimer
	defer idle.stop()
	backoff := finalizerBackoffMin
	for {
		if s.isClosing() {
			s.abandonFinalizers()
			return
		}
		failed := false
		if err := fault.Call(func() error { failed = s.finalizePass(); return nil }); err != nil {
			logFailure("run finalizer panicked", err)
			failed = true
		}
		wake := s.finals.wake
		var timerC <-chan time.Time
		if failed {
			// Storage is failing: one global backoff for every pending run,
			// not a retry per run.
			timerC, wake = idle.arm(backoff), nil
			backoff = min(backoff*2, finalizerBackoffMax)
		} else {
			backoff = finalizerBackoffMin
			if background, _ := s.Finalizers(); background > 0 {
				continue // work arrived during the pass
			}
			timerC = idle.arm(-1)
		}
		select {
		case <-s.stopping:
			s.abandonFinalizers()
			return
		case <-wake:
		case <-timerC:
		}
	}
}

// finalizePass tries every pending run once, stopping at the first storage
// failure (the rest would fail the same way). It reports whether it failed.
func (s *Service) finalizePass() (failed bool) {
	s.mu.Lock()
	items := make([]*finalItem, 0, len(s.finalizing))
	for _, it := range s.finalizing {
		items = append(items, it)
	}
	s.mu.Unlock()
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
		case errors.Is(err, store.ErrInvalidTransition):
			logFailure("persisting terminal run state rejected", err, "run", it.r.ID, "status", t.status)
			s.dropFinalizer(it.r.ID)
		default:
			logFailure("persisting terminal run state failed; retrying", err, "run", it.r.ID, "status", t.status, "pending", len(items))
			return true
		}
	}
	return false
}

func (s *Service) dropFinalizer(id string) {
	s.mu.Lock()
	delete(s.finalizing, id)
	s.mu.Unlock()
}

func (s *Service) abandonFinalizers() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.finalizing))
	for id := range s.finalizing {
		ids = append(ids, id)
	}
	clear(s.finalizing)
	s.mu.Unlock()
	for _, id := range ids {
		slog.Warn("run left unfinalized at shutdown; recovery will mark it interrupted", "run", id)
	}
}

// finalizeInline retries a terminal write in the completing goroutine, which
// keeps the run's capacity slot. It is the overflow path of the finalizer
// budget and ends at shutdown.
func (s *Service) finalizeInline(r model.Run, d model.Definition, t terminal) {
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
	}
}
