package supervisor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
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
	ctx, cancel := context.WithCancel(s.ctx)
	s.workerCancels[d.Name] = cancel
	s.workerDone[d.Name] = make(chan struct{})
	s.workerDefs[d.Name] = d
	return workerLoop{ctx: ctx, def: d}
}

func (s *Supervisor) launch(worker workerLoop) {
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		s.loop(worker.ctx, worker.def)
	}()
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
		delete(s.workerCancels, d.Name)
		delete(s.workerDefs, d.Name)
		if done := s.workerDone[d.Name]; done != nil {
			close(done)
			delete(s.workerDone, d.Name)
		}
		s.mu.Unlock()
	}()
	healthyAfter, _ := time.ParseDuration(d.HealthyAfter)
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
			return
		}
		s.mu.Lock()
		s.active[d.Name] = r.ID
		s.mu.Unlock()
		// Wait on the executor's done channel instead of polling the store:
		// the channel closes only after the terminal state is persisted, so a
		// single read is current. nil means the run already finished.
		if done := s.exec.Wait(r.ID); done != nil {
			select {
			case <-ctx.Done():
				_ = s.exec.Stop(r.ID)
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
		current, err := s.store.Run(context.Background(), r.ID)
		if err != nil {
			slog.Error("supervisor: reading worker run failed", "worker", d.Name, "run", r.ID, "error", err)
			return
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
			return
		}
		delay, _ := time.ParseDuration(d.RestartDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
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
	if s.ctx != nil && !reserved && d.Kind == model.KindWorker && d.IsEnabled() {
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
	s.mu.Lock()
	done := s.workerDone[d.Name]
	cancel := s.workerCancels[d.Name]
	s.mu.Unlock()
	_ = s.Stop(d.Name)
	if cancel != nil {
		cancel()
	}
	go func() {
		if done != nil {
			<-done
		}
		s.StartDefinition(d)
	}()
}

func (s *Supervisor) Shutdown() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
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
