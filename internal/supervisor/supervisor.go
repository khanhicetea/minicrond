package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// Supervisor keeps one-instance local workers alive. A worker run that exits
// is restarted per policy with fixed backoff; consecutive starts that never
// reach healthy_after count toward fatal. An operator hold (Stop) suppresses
// restart, including under restart = "always", until Start or a reload lifts it.
type Supervisor struct {
	store         *store.Store
	exec          *executor.Service
	lifecycle     sync.Mutex
	mu            sync.Mutex
	cancel        context.CancelFunc
	ctx           context.Context
	closing       bool
	holds         map[string]bool
	failures      map[string]int
	active        map[string]string
	workerCancels map[string]context.CancelFunc
	workerDone    map[string]chan struct{}
	workerDefs    map[string]model.Definition
	loops         sync.WaitGroup
}

type workerLoop struct {
	ctx context.Context
	def model.Definition
}

func New(st *store.Store, ex *executor.Service) *Supervisor {
	return &Supervisor{
		store:         st,
		exec:          ex,
		holds:         make(map[string]bool),
		failures:      make(map[string]int),
		active:        make(map[string]string),
		workerCancels: make(map[string]context.CancelFunc),
		workerDone:    make(map[string]chan struct{}),
		workerDefs:    make(map[string]model.Definition),
	}
}

func (s *Supervisor) startLocked(d model.Definition) workerLoop {
	d = d.Clone()
	ctx, cancel := context.WithCancel(s.ctx)
	s.workerCancels[d.Name] = cancel
	s.workerDone[d.Name] = make(chan struct{})
	s.workerDefs[d.Name] = d
	return workerLoop{ctx: ctx, def: d}
}

func (s *Supervisor) launch(worker workerLoop) {
	s.loops.Go(func() {
		if err := fault.Call(func() error {
			s.loop(worker.ctx, worker.def)
			return nil
		}); err != nil {
			panicErr, _ := errors.AsType[*fault.PanicError](err)
			slog.Error("worker supervision panicked", "worker", worker.def.Name, "error", err, "stack", string(panicErr.Stack))
		}
	})
}

func (s *Supervisor) Reload(defs []model.Definition) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	desired := make(map[string]model.Definition)
	for _, d := range defs {
		if d.Kind == model.KindWorker && d.IsEnabled() && d.DoesAutostart() {
			desired[d.Name] = d
		}
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	if s.ctx == nil {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
	var waits []<-chan struct{}
	for name, cancel := range s.workerCancels {
		want, ok := desired[name]
		if ok && sameDefinition(s.workerDefs[name], want) {
			delete(desired, name)
			continue
		}
		cancel()
		if done := s.workerDone[name]; done != nil {
			waits = append(waits, done)
		}
	}
	s.mu.Unlock()
	for _, done := range waits {
		<-done
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	workers := make([]workerLoop, 0, len(desired))
	for _, d := range desired {
		workers = append(workers, s.startLocked(d))
	}
	s.mu.Unlock()
	for _, worker := range workers {
		s.launch(worker)
	}
}

func (s *Supervisor) loop(ctx context.Context, d model.Definition) {
	defer func() {
		s.mu.Lock()
		id := s.active[d.Name]
		s.mu.Unlock()
		if id != "" {
			s.stopRun(id)
			if done := s.exec.Wait(id); done != nil {
				<-done
			}
		}
		s.mu.Lock()
		delete(s.active, d.Name)
		delete(s.workerCancels, d.Name)
		delete(s.workerDefs, d.Name)
		if done := s.workerDone[d.Name]; done != nil {
			close(done)
			delete(s.workerDone, d.Name)
		}
		s.mu.Unlock()
	}()
	healthyAfter := time.Duration(d.HealthyAfter) * time.Second
	_, hash, err := config.Canonical(d)
	if err != nil {
		slog.Error("supervisor: canonicalizing definition failed", "worker", d.Name, "error", err)
		return
	}
	for {
		if ctx.Err() != nil || s.held(d.Name) {
			return
		}
		r, err := s.exec.Trigger(ctx, d, hash, "startup", nil)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, executor.ErrShutdown) || s.held(d.Name) {
				return
			}
			slog.Error("starting supervised worker failed", "worker", d.Name, "run", r.ID, "error", err)
			// A failed log open may already have persisted a terminal run.
			// Admission failures still consume the failed-start budget.
			if !model.Terminal(r.Status) {
				if d.Restart == "never" || !s.restartAfter(ctx, d, false) {
					return
				}
				continue
			}
		}
		s.mu.Lock()
		s.active[d.Name] = r.ID
		held := s.holds[d.Name]
		s.mu.Unlock()
		if held {
			s.stopRun(r.ID)
		}
		// Wait for process cleanup and the attempted terminal transition,
		// then verify the persisted state before restarting.
		if done := s.exec.Wait(r.ID); done != nil {
			select {
			case <-ctx.Done():
				s.stopRun(r.ID)
				<-done
				s.mu.Lock()
				if s.active[d.Name] == r.ID {
					delete(s.active, d.Name)
				}
				s.mu.Unlock()
				return
			case <-done:
			}
		}
		s.mu.Lock()
		if s.active[d.Name] == r.ID {
			delete(s.active, d.Name)
		}
		s.mu.Unlock()
		current, err := s.terminalRun(ctx, d.Name, r.ID)
		if err != nil {
			return // canceled while storage was unavailable
		}
		if !model.TerminalStatuses[current.Status] {
			return // defensive: never restart a run that is not terminal
		}
		if s.held(d.Name) {
			return
		}
		if d.Restart == "never" || (d.Restart == "on-failure" && current.Status == "succeeded") {
			return
		}
		healthy := current.StartedAt != nil && current.EndedAt != nil && current.EndedAt.Sub(*current.StartedAt) >= healthyAfter
		if !s.restartAfter(ctx, d, healthy) {
			return
		}
	}
}

// terminalRun reads a finished worker run, retrying storage failures with
// capped backoff. Giving up would leave the worker neither running nor fatal
// until the next reload. It fails only when ctx ends.
func (s *Supervisor) terminalRun(ctx context.Context, name, id string) (model.Run, error) {
	backoff := time.Second
	for {
		current, err := s.store.Run(ctx, id)
		if err == nil {
			return current, nil
		}
		if ctx.Err() != nil {
			return model.Run{}, ctx.Err()
		}
		slog.Error("supervisor: reading worker run failed; retrying", "worker", name, "run", id, "retry_in", backoff, "error", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return model.Run{}, ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// restartAfter accounts for both admission failures and unhealthy lifetimes.
func (s *Supervisor) restartAfter(ctx context.Context, d model.Definition, healthy bool) bool {
	s.mu.Lock()
	if healthy {
		s.failures[d.Name] = 0
	} else {
		s.failures[d.Name]++
	}
	failures := s.failures[d.Name]
	s.mu.Unlock()
	if failures >= d.MaxRestartAttempts {
		slog.Warn("worker fatal: restart attempts exhausted", "worker", d.Name, "attempts", failures)
		return false
	}
	timer := time.NewTimer(time.Duration(d.RestartDelay) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return !s.held(d.Name)
	}
}

func (s *Supervisor) stopRun(id string) {
	if err := s.exec.Stop(id); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("stopping supervised worker failed", "run", id, "error", err)
	}
}

func sameDefinition(a, b model.Definition) bool {
	_, ah, aerr := config.Canonical(a)
	_, bh, berr := config.Canonical(b)
	return aerr == nil && berr == nil && ah == bh
}

func (s *Supervisor) held(name string) bool { s.mu.Lock(); defer s.mu.Unlock(); return s.holds[name] }

// Stop places an operator hold and stops the active lifetime. The hold
// prevents restart = "always" from immediately undoing the stop request.
func (s *Supervisor) Stop(name string) error {
	s.mu.Lock()
	s.holds[name] = true
	id := s.active[name]
	s.mu.Unlock()
	if id != "" {
		return s.exec.Stop(id)
	}
	return nil
}

// StartDefinition lifts a hold and, if the worker is not already running,
// starts it immediately; a manual restart bypasses backoff and resets the
// failure counter.
func (s *Supervisor) StartDefinition(d model.Definition) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	delete(s.holds, d.Name)
	s.failures[d.Name] = 0
	_, reserved := s.workerCancels[d.Name]
	var worker workerLoop
	if !s.closing && s.ctx != nil && !reserved && d.Kind == model.KindWorker && d.IsEnabled() {
		worker = s.startLocked(d)
	}
	s.mu.Unlock()
	if worker.ctx != nil {
		s.launch(worker)
	}
}

func (s *Supervisor) Start(name string) { s.mu.Lock(); delete(s.holds, name); s.mu.Unlock() }

// Restart stops the current lifetime and starts its replacement only after the
// old supervisor loop and process have fully completed.
func (s *Supervisor) Restart(d model.Definition) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	d = d.Clone()
	done := s.workerDone[d.Name]
	cancel := s.workerCancels[d.Name]
	s.mu.Unlock()
	if err := s.Stop(d.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("stopping worker for restart failed", "worker", d.Name, "error", err)
	}
	if cancel != nil {
		cancel()
	}
	s.loops.Go(func() {
		if done != nil {
			<-done
		}
		// Closing suppresses a replacement when shutdown interrupts restart.
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if !closing {
			s.StartDefinition(d)
		}
	})
}

// BeginShutdown stops admission and cancels supervision before the executor
// spends its shutdown budget. Joining is separate so grace periods are bounded
// by the daemon's executor deadline.
func (s *Supervisor) BeginShutdown() {
	s.mu.Lock()
	s.closing = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
}

func (s *Supervisor) Shutdown() {
	s.BeginShutdown()
	// Join any admission already in progress before waiting on the group.
	s.lifecycle.Lock()
	s.lifecycle.Unlock()
	s.loops.Wait()
}

// State reports operator supervision facts for one worker definition.
type State struct {
	Held     bool `json:"held"`
	Active   bool `json:"active"`
	Failures int  `json:"failures"`
}

func (s *Supervisor) State(name string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, active := s.active[name]
	return State{Held: s.holds[name], Active: active, Failures: s.failures[name]}
}

// States takes a consistent snapshot of the requested workers under one lock.
func (s *Supervisor) States(names []string) map[string]State {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make(map[string]State, len(names))
	for _, name := range names {
		_, active := s.active[name]
		states[name] = State{Held: s.holds[name], Active: active, Failures: s.failures[name]}
	}
	return states
}
