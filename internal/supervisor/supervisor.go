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
// reach healthy_after and fail count toward fatal. An operator hold (Stop) suppresses
// restart, including under restart = "always", until Start or a reload lifts it.
type Supervisor struct {
	store     *store.Store
	exec      workerExecutor
	lifecycle sync.Mutex
	// stateMu orders durable exhaustion writes against operator resets. It is
	// never held with mu during I/O, and Stop/Shutdown never acquire it.
	stateMu       sync.Mutex
	epochs        map[string]uint64
	epochBase     uint64 // default epoch for names without an explicit reset
	epochSeq      uint64 // unique reset tokens; full reload can discard the name map
	exhausted     map[string]model.WorkerExhaustion
	pending       map[string]bool
	onExhausted   func(model.Definition, model.WorkerExhaustion)
	mu            sync.Mutex
	cancel        context.CancelFunc
	ctx           context.Context
	closing       bool
	stopped       chan struct{} // closed by BeginShutdown
	holds         map[string]bool
	failures      map[string]int
	active        map[string]string
	workerCancels map[string]context.CancelFunc
	workerDone    map[string]chan struct{}
	workerDefs    map[string]model.Definition
	retiring      map[string]bool // committed to leaving the loop; reload must replace it
	loops         sync.WaitGroup
}

// workerExecutor is the process lifecycle surface used by supervision.
type workerExecutor interface {
	Trigger(context.Context, model.Definition, string, string, *time.Time) (model.Run, error)
	Wait(string) <-chan struct{}
	// TakeOutcome returns the in-memory result of a finished startup run, so
	// a restart decision never waits for the database (ADR-16).
	TakeOutcome(string) (model.Run, bool)
	Finalizing(string) bool
	Stop(string) error
}

type workerLoop struct {
	ctx context.Context
	def model.Definition
}

func New(st *store.Store, ex *executor.Service, onExhausted ...func(model.Definition, model.WorkerExhaustion)) *Supervisor {
	var notify func(model.Definition, model.WorkerExhaustion)
	if len(onExhausted) > 0 {
		notify = onExhausted[0]
	}
	return &Supervisor{
		store:         st,
		epochs:        make(map[string]uint64),
		exhausted:     make(map[string]model.WorkerExhaustion),
		pending:       make(map[string]bool),
		onExhausted:   notify,
		exec:          ex,
		stopped:       make(chan struct{}),
		holds:         make(map[string]bool),
		failures:      make(map[string]int),
		active:        make(map[string]string),
		workerCancels: make(map[string]context.CancelFunc),
		workerDone:    make(map[string]chan struct{}),
		workerDefs:    make(map[string]model.Definition),
		retiring:      make(map[string]bool),
	}
}

func (s *Supervisor) startLocked(d model.Definition) workerLoop {
	d = d.Clone()
	s.failures[d.Name] = 0
	delete(s.holds, d.Name)
	ctx, cancel := context.WithCancel(s.ctx)
	s.workerCancels[d.Name] = cancel
	s.workerDone[d.Name] = make(chan struct{})
	s.workerDefs[d.Name] = d
	delete(s.retiring, d.Name)
	return workerLoop{ctx: ctx, def: d}
}

// launch must be called with s.mu held and s.closing false: every loops.Go
// call is ordered before shutdown's loops.Wait by that lock.
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

func (s *Supervisor) Reload(defs []model.Definition) error {
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
		return executor.ErrShutdown
	}
	if s.ctx == nil {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
	s.mu.Unlock()
	if err := s.resetExhaustion(""); err != nil {
		if errors.Is(err, executor.ErrShutdown) {
			return err
		}
		// ADR-16: a database that cannot clear stored exhaustion must not keep
		// workers from starting. The in-memory budgets are reset regardless;
		// the stale stored record is cleared by the next successful reset.
		slog.Error("supervisor: clearing stored worker exhaustion failed; starting workers anyway", "error", err)
	}
	s.mu.Lock()
	var waits []<-chan struct{}
	for name, cancel := range s.workerCancels {
		want, ok := desired[name]
		if ok && sameDefinition(s.workerDefs[name], want) && !s.holds[name] && !s.retiring[name] {
			delete(desired, name)
			continue
		}
		cancel()
		if done := s.workerDone[name]; done != nil {
			waits = append(waits, done)
		}
	}
	s.mu.Unlock()
	// Old workers may hold a long grace period. Shutdown abandons this wait
	// (it cancels every worker itself) instead of blocking behind it.
	for _, done := range waits {
		select {
		case <-done:
		case <-s.stopped:
			return executor.ErrShutdown
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return executor.ErrShutdown
	}
	clear(s.holds)
	for _, d := range desired {
		s.launch(s.startLocked(d))
	}
	return nil
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
			s.exec.TakeOutcome(id)
		}
		s.mu.Lock()
		delete(s.active, d.Name)
		delete(s.workerCancels, d.Name)
		delete(s.workerDefs, d.Name)
		delete(s.retiring, d.Name)
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
	lastRunID := ""
	for {
		if ctx.Err() != nil || s.held(d.Name) {
			return
		}
		r, err := s.exec.Trigger(ctx, d, hash, "startup", nil)
		if r.ID != "" {
			lastRunID = r.ID
		}
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, executor.ErrShutdown) || s.held(d.Name) {
				return
			}
			slog.Error("starting supervised worker failed", "worker", d.Name, "run", r.ID, "error", err)
			// A failed log open may already have persisted a terminal run.
			// Admission failures still consume the failed-start budget.
			if !model.Terminal(r.Status) {
				if d.Restart == "never" || !s.restartAfter(ctx, d, false, lastRunID, "start_error: "+err.Error()) {
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
				s.exec.TakeOutcome(r.ID)
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
		// The executor's in-memory outcome decides the restart: the terminal
		// write may still be waiting for a failing database (ADR-16). Only an
		// outcome that is unexpectedly missing falls back to the stored row.
		current, ok := s.exec.TakeOutcome(r.ID)
		var abandoned bool
		if !ok {
			current, abandoned, err = s.terminalRun(ctx, d.Name, r.ID)
			if err != nil {
				return // canceled while storage was unavailable
			}
		}
		if abandoned {
			// Explicit finalization-failure policy: the process is gone, the
			// stored row stayed nonterminal and no finalizer owns it (only
			// possible when the executor was shutting down). Count it as an
			// unhealthy lifetime instead of silently abandoning the worker.
			slog.Error("worker run left nonterminal without a finalizer; applying restart policy as a failed lifetime", "worker", d.Name, "run", r.ID, "status", current.Status)
			if s.held(d.Name) || d.Restart == "never" || !s.restartAfter(ctx, d, false, r.ID, "terminal persistence abandoned") {
				return
			}
			continue
		}
		if s.held(d.Name) {
			return
		}
		if d.Restart == "never" || (d.Restart == "on-failure" && current.Status == "succeeded") {
			return
		}
		healthy := current.Status == "succeeded" || (current.StartedAt != nil && current.EndedAt != nil && current.EndedAt.Sub(*current.StartedAt) >= healthyAfter)
		if !s.restartAfter(ctx, d, healthy, current.ID, current.EndReason) {
			return
		}
	}
}

// terminalRun waits for a finished worker run to reach a terminal state.
// The executor persists that state inline or, when storage is down, from a
// background finalizer; a nonterminal read is therefore not an outcome. It
// keeps polling with capped backoff until the state is terminal, so the restart
// policy is applied once finalization succeeds, and fails only when ctx ends.
// If the row stays nonterminal and no finalizer owns it, abandoned is true and
// the caller applies its failure policy. Finalizing is sampled before the read
// so a finalizer that just succeeded is never mistaken for an abandoned one.
func (s *Supervisor) terminalRun(ctx context.Context, name, id string) (run model.Run, abandoned bool, err error) {
	backoff := 500 * time.Millisecond
	for {
		finalizing := s.exec.Finalizing(id)
		current, readErr := s.store.Run(ctx, id)
		switch {
		case readErr == nil && model.TerminalStatuses[current.Status]:
			return current, false, nil
		case readErr == nil && !finalizing:
			return current, true, nil
		case readErr == nil:
			slog.Warn("supervisor: worker run is awaiting background finalization; restart is deferred", "worker", name, "run", id, "status", current.Status, "retry_in", backoff)
		case ctx.Err() != nil:
			return model.Run{}, false, ctx.Err()
		default:
			slog.Error("supervisor: reading worker run failed; retrying", "worker", name, "run", id, "retry_in", backoff, "error", readErr)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return model.Run{}, false, ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// restartAfter accounts for both admission failures and unhealthy lifetimes.
func (s *Supervisor) restartAfter(ctx context.Context, d model.Definition, healthy bool, lastRunID, reason string) bool {
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		return false
	}
	if healthy {
		s.failures[d.Name] = 0
	} else {
		s.failures[d.Name]++
	}
	failures := s.failures[d.Name]
	epoch := s.epochLocked(d.Name)
	s.mu.Unlock()
	if d.RestartBackoffMax > 0 {
		// Backoff mode never retires: a container's only service keeps being
		// retried. The budget only marks when the crash loop is reported.
		if failures == d.MaxRestartAttempts {
			slog.Warn("supervisor: worker is crash looping; restarting with backoff", "worker", d.Name, "failures", failures, "max_delay_s", d.RestartBackoffMax, "reason", reason)
		}
	} else if failures >= d.MaxRestartAttempts {
		if s.persistExhaustion(ctx, d, epoch, failures, lastRunID, reason) {
			s.mu.Lock()
			retire := s.epochLocked(d.Name) == epoch
			if retire {
				s.retiring[d.Name] = true
			}
			s.mu.Unlock()
			if retire {
				return false
			}
		}
		// An operator reset won the race with the exhaustion write.
	}
	timer := time.NewTimer(restartDelay(d, failures))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return !s.held(d.Name)
	}
}

// restartDelay is restart_delay, doubled per consecutive unhealthy exit up to
// restart_backoff_max when that is set.
func restartDelay(d model.Definition, failures int) time.Duration {
	delay := time.Duration(d.RestartDelay) * time.Second
	limit := time.Duration(d.RestartBackoffMax) * time.Second
	if limit <= 0 {
		return delay
	}
	for i := 1; i < failures && delay < limit; i++ {
		delay *= 2
	}
	return min(delay, limit)
}

func (s *Supervisor) stopRun(id string) {
	if err := s.exec.Stop(id); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("stopping supervised worker failed", "run", id, "error", err)
	}
}

func sameDefinition(a, b model.Definition) bool {
	_, ah, aerr := config.Canonical(a)
	_, bh, berr := config.Canonical(b)
	return aerr == nil && berr == nil && a.Revision == b.Revision && ah == bh
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
func (s *Supervisor) StartDefinition(d model.Definition) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if err := s.resetExhaustion(d.Name); err != nil {
		return err
	}
	return s.startDefinitionLocked(d)
}

// startDefinitionLocked starts after the caller has durably reset state and
// acquired lifecycle. Restart's async completion must not need a second clear.
func (s *Supervisor) startDefinitionLocked(d model.Definition) error {
	s.mu.Lock()
	delete(s.holds, d.Name)
	s.failures[d.Name] = 0
	cancel, reserved := s.workerCancels[d.Name]
	if reserved && s.active[d.Name] == "" {
		done := s.workerDone[d.Name]
		cancel()
		s.mu.Unlock()
		select {
		case <-done:
		case <-s.stopped:
			return executor.ErrShutdown
		}
		s.mu.Lock()
		reserved = false
	}
	if !s.closing && s.ctx != nil && !reserved && d.Kind == model.KindWorker && d.IsEnabled() {
		s.launch(s.startLocked(d))
	}
	s.mu.Unlock()
	return nil
}

// Restart stops the current lifetime and starts its replacement only after the
// old supervisor loop and process have fully completed.
func (s *Supervisor) Restart(d model.Definition) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if err := s.resetExhaustion(d.Name); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return executor.ErrShutdown
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return executor.ErrShutdown
	}
	epoch := s.epochLocked(d.Name)
	s.loops.Go(func() {
		if done != nil {
			select {
			case <-done:
			case <-s.stopped:
				return // shutdown suppresses the replacement
			}
		}
		s.lifecycle.Lock()
		defer s.lifecycle.Unlock()
		s.mu.Lock()
		current := !s.closing && s.epochLocked(d.Name) == epoch
		s.mu.Unlock()
		if !current {
			return
		} // a newer start/reload owns this worker now
		if err := s.startDefinitionLocked(d); err != nil {
			slog.Error("starting replacement worker failed", "worker", d.Name, "error", err)
		}
	})
	return nil
}

// BeginShutdown stops admission and cancels supervision before the executor
// spends its shutdown budget. Joining is separate so grace periods are bounded
// by the daemon's executor deadline.
func (s *Supervisor) BeginShutdown() {
	s.mu.Lock()
	if !s.closing {
		close(s.stopped)
	}
	s.closing = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
}

// Shutdown joins supervision without a deadline. Prefer ShutdownContext.
func (s *Supervisor) Shutdown() { _ = s.ShutdownContext(context.Background()) }

// ShutdownContext stops supervision and waits for the worker loops until ctx
// ends. Once closing is set no loop can be started (every loops.Go is ordered
// before this call by s.mu), and it never waits for the lifecycle lock, which
// a reload may hold while an old worker finishes its grace period.
//
// Loops left running at expiry only wait for their executor runs: their
// contexts are already canceled, so they start no new run and every store call
// they make fails immediately. They therefore cannot touch the database in a
// harmful way after the caller closes it. The executor's own shutdown (its
// forced kill and bounded join) is what bounds the processes they wait for.
func (s *Supervisor) ShutdownContext(ctx context.Context) error {
	s.BeginShutdown()
	joined := make(chan struct{})
	go func() {
		s.loops.Wait()
		close(joined)
	}()
	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// State reports operator supervision facts for one worker definition.
type State struct {
	Held              bool                    `json:"held"`
	Active            bool                    `json:"active"`
	Failures          int                     `json:"failures"`
	Status            string                  `json:"status"`
	Exhaustion        *model.WorkerExhaustion `json:"exhaustion,omitempty"`
	ExhaustionPending bool                    `json:"exhaustion_pending,omitempty"`
}

func (s *Supervisor) State(name string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked(name)
}

// States takes a consistent snapshot of the requested workers under one lock.
func (s *Supervisor) States(names []string) map[string]State {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make(map[string]State, len(names))
	for _, name := range names {
		states[name] = s.stateLocked(name)
	}
	return states
}
