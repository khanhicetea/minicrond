package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

type blockingExecutor struct{ started chan model.Definition }

func (e *blockingExecutor) Trigger(ctx context.Context, d model.Definition, _, _ string, _ *time.Time) (model.Run, error) {
	select {
	case e.started <- d:
	case <-ctx.Done():
	}
	<-ctx.Done()
	return model.Run{}, ctx.Err()
}
func (*blockingExecutor) Wait(string) <-chan struct{} { return nil }
func (*blockingExecutor) Finalizing(string) bool      { return false }
func (*blockingExecutor) Stop(string) error           { return nil }

func TestReloadResetsBudget(t *testing.T) {
	for _, change := range []string{"reload", "revision", "hash"} {
		t.Run(change, func(t *testing.T) {
			_, s := setup(t)
			fake := &blockingExecutor{started: make(chan model.Definition, 1)}
			s.exec = fake
			d := workerDef("reset", "", []string{"/bin/true"}, 30, 5)
			s.Reload([]model.Definition{d})
			<-fake.started
			s.mu.Lock()
			s.failures[d.Name] = 4
			s.mu.Unlock()
			switch change {
			case "revision":
				d.Revision++
			case "hash":
				d.Argv = []string{"/bin/false"}
			}
			s.Reload([]model.Definition{d})
			if change != "reload" {
				<-fake.started
			}
			if got := s.State(d.Name).Failures; got != 0 {
				t.Fatalf("failures=%d", got)
			}
		})
	}
}

func TestExplicitStartResetsBudget(t *testing.T) {
	_, s := setup(t)
	d := workerDef("reset", "", []string{"/bin/true"}, 30, 5)
	s.failures[d.Name] = 5
	s.StartDefinition(d)
	if s.State(d.Name).Failures != 0 {
		t.Fatal("start retained failures")
	}
}
