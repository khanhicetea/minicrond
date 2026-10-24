package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// runningExecutor starts one long-lived run and records how it is stopped.
type runningExecutor struct {
	started  chan struct{}
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	stops    []string
	runStart sync.Once
}

func (e *runningExecutor) Trigger(ctx context.Context, d model.Definition, _, _ string, _ *time.Time) (model.Run, error) {
	e.runStart.Do(func() { close(e.started) })
	return model.Run{ID: "run-1", Job: d.Name, Status: "running"}, nil
}
func (e *runningExecutor) Wait(string) <-chan struct{}        { return e.done }
func (*runningExecutor) Finalizing(string) bool               { return false }
func (*runningExecutor) TakeOutcome(string) (model.Run, bool) { return model.Run{}, false }
func (e *runningExecutor) Stop(string) error                  { e.record("stop"); return nil }
func (e *runningExecutor) StopForShutdown(string) bool        { e.record("shutdown"); return true }
func (e *runningExecutor) record(kind string) {
	e.mu.Lock()
	e.stops = append(e.stops, kind)
	e.mu.Unlock()
	e.once.Do(func() { close(e.done) })
}

// At daemon shutdown supervision stops its workers as shutdown stops, so
// scheduler.shutdown_grace caps their grace; a reload stop stays ordinary.
func TestShutdownStopsWorkersAsShutdownStops(t *testing.T) {
	for _, how := range []string{"shutdown", "reload"} {
		t.Run(how, func(t *testing.T) {
			_, s := setup(t)
			fake := &runningExecutor{started: make(chan struct{}), done: make(chan struct{})}
			s.exec = fake
			d := workerDef("svc", "", []string{"/bin/sleep", "30"}, 30, 5)
			if err := s.Reload([]model.Definition{d}); err != nil {
				t.Fatal(err)
			}
			<-fake.started
			want := "shutdown"
			if how == "shutdown" {
				s.BeginShutdown()
			} else {
				want = "stop"
				if err := s.Reload(nil); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-fake.done:
			case <-time.After(5 * time.Second):
				t.Fatal("worker run was not stopped")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.stops) == 0 || fake.stops[0] != want {
				t.Fatalf("stops = %v, want %s first", fake.stops, want)
			}
		})
	}
}
