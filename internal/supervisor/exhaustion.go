package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// resetExhaustion commits the clear before allowing a new lifecycle. stateMu
// serializes it with exhaustion writes; epochs invalidate a retrying old write.
func (s *Supervisor) resetExhaustion(name string) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return executor.ErrShutdown
	}
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.store.ClearWorkerExhaustion(ctx, name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		for name := range s.epochs {
			s.epochs[name]++
		}
		// Some workers have not attempted an exhaustion write yet.
		for name := range s.failures {
			if _, ok := s.epochs[name]; !ok {
				s.epochs[name] = 1
			}
		}
		clear(s.failures)
		clear(s.exhausted)
		clear(s.pending)
	} else {
		s.epochs[name]++
		s.failures[name] = 0
		delete(s.exhausted, name)
		delete(s.pending, name)
	}
	return nil
}

// persistExhaustion retains required state in this worker's existing loop when
// storage fails. No process slot, supervisor mutex, or extra goroutine is held
// during retry/backoff. Stop, reset and shutdown can interrupt it.
func (s *Supervisor) persistExhaustion(ctx context.Context, d model.Definition, epoch uint64, attempts int, lastRunID, reason string) bool {
	reason = fmt.Sprintf("restart attempts exhausted after %d failures: %s", attempts, reason)
	if len(reason) > 512 {
		reason = reason[:512]
	}
	outcome := model.WorkerExhaustion{Reason: reason, LastRunID: lastRunID, Attempts: attempts, Revision: d.Revision, At: time.Now().UTC()}
	delay := time.Second
	for {
		s.stateMu.Lock()
		s.mu.Lock()
		if ctx.Err() != nil || s.holds[d.Name] || s.epochs[d.Name] != epoch {
			s.mu.Unlock()
			s.stateMu.Unlock()
			return false
		}
		s.exhausted[d.Name] = outcome
		s.pending[d.Name] = true
		s.mu.Unlock()
		writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := s.store.SaveWorkerExhaustion(writeCtx, d, outcome)
		cancel()
		stale := errors.Is(err, store.ErrRevisionConflict)
		if err == nil || stale {
			s.mu.Lock()
			delete(s.pending, d.Name)
			if stale {
				delete(s.exhausted, d.Name)
			}
			s.mu.Unlock()
		}
		s.stateMu.Unlock()
		if stale {
			return true
		} // replacement/reconcile owns the next lifecycle
		if err == nil {
			slog.Warn("worker restart attempts exhausted", "worker", d.Name, "attempts", attempts, "last_run", lastRunID)
			if s.onExhausted != nil {
				if err := fault.Call(func() error { s.onExhausted(d.Clone(), outcome); return nil }); err != nil {
					slog.Error("worker exhaustion callback failed", "worker", d.Name, "error", err)
				}
			}
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		slog.Error("persisting worker exhaustion failed; retrying", "worker", d.Name, "error", err, "retry_in", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		delay = min(2*delay, 30*time.Second)
	}
}

func (s *Supervisor) stateLocked(name string) State {
	_, active := s.active[name]
	state := State{Held: s.holds[name], Active: active, Failures: s.failures[name], Status: "stopped"}
	if active {
		state.Status = "running"
	} else if _, ok := s.workerCancels[name]; ok {
		state.Status = "restarting"
	}
	if state.Held {
		state.Status = "held"
	}
	if outcome, ok := s.exhausted[name]; ok {
		state.Status = "exhausted"
		state.Exhaustion = &outcome
		state.ExhaustionPending = s.pending[name]
	}
	return state
}

// ReadStates combines volatile process facts with durable exhaustion evidence.
// Database errors are returned, never rendered as a healthy/stopped worker.
func (s *Supervisor) ReadStates(ctx context.Context, names []string) (map[string]State, error) {
	// A concurrent reset can make this request's snapshot stale, as with any
	// ordinary metadata read; readers never hold the lifecycle/write lock.
	outcomes, err := s.store.WorkerExhaustions(ctx)
	if err != nil {
		return nil, err
	}
	states := s.States(names)
	for name, state := range states {
		if outcome, ok := outcomes[name]; ok && state.Exhaustion == nil {
			state.Status = "exhausted"
			state.Exhaustion = &outcome
			state.Failures = outcome.Attempts
			states[name] = state
		}
	}
	return states, nil
}
