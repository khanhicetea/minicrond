package supervisor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/minicron/minicron/internal/config"
	"github.com/minicron/minicron/internal/executor"
	"github.com/minicron/minicron/internal/model"
	"github.com/minicron/minicron/internal/store"
)

// Supervisor keeps one-instance local workers alive. A worker run that exits
// is restarted per policy with fixed backoff; consecutive starts that never
// reach healthy_after count toward fatal. An operator hold (Stop) suppresses
// restart, including under restart = "always", until Start or a reload lifts it.
type Supervisor struct {
	store    *store.Store
	exec     *executor.Service
	mu       sync.Mutex
	cancel   context.CancelFunc
	ctx      context.Context
	holds    map[string]bool
	failures map[string]int
	active   map[string]string
}

func New(st *store.Store, ex *executor.Service) *Supervisor {
	return &Supervisor{store: st, exec: ex, holds: make(map[string]bool), failures: make(map[string]int), active: make(map[string]string)}
}

func (s *Supervisor) Reload(defs []model.Definition) {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.ctx = ctx
	s.mu.Unlock()
	for _, d := range defs {
		if d.Kind == model.KindWorker && d.IsEnabled() && d.DoesAutostart() {
			go s.loop(ctx, d)
		}
	}
}

func (s *Supervisor) loop(ctx context.Context, d model.Definition) {
	healthyAfter, _ := time.ParseDuration(d.HealthyAfter)
	for {
		if ctx.Err() != nil || s.held(d.Name) {
			return
		}
		_, hash, _ := config.Canonical(d)
		r, err := s.exec.Trigger(ctx, d, hash, "startup", nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.active[d.Name] = r.ID
		s.mu.Unlock()
		var current model.Run
		terminal := false
		for !terminal {
			select {
			case <-ctx.Done():
				_ = s.exec.Stop(r.ID)
				return
			case <-time.After(200 * time.Millisecond):
				current, err = s.store.Run(context.Background(), r.ID)
				if err == nil && model.TerminalStatuses[current.Status] {
					terminal = true
				}
			}
		}
		s.mu.Lock()
		delete(s.active, d.Name)
		s.mu.Unlock()
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
	s.mu.Lock()
	delete(s.holds, d.Name)
	s.failures[d.Name] = 0
	ctx := s.ctx
	_, running := s.active[d.Name]
	s.mu.Unlock()
	if ctx != nil && !running && d.Kind == model.KindWorker && d.IsEnabled() {
		go s.loop(ctx, d)
	}
}

func (s *Supervisor) Start(name string) { s.mu.Lock(); delete(s.holds, name); s.mu.Unlock() }
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
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
