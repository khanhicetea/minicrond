package supervisor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// Each fake lifetime completes immediately with a persisted terminal result.
type instantExecutor struct {
	store  *store.Store
	status string
	starts int
	limit  int
	cancel context.CancelFunc
}

func (e *instantExecutor) Trigger(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time) (model.Run, error) {
	e.starts++
	now := time.Now().UTC()
	r := model.Run{ID: fmt.Sprintf("instant-%d", e.starts), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Status: e.status, QueuedAt: now, EndedAt: &now, Trigger: trigger, DefinitionHash: hash}
	err := e.store.CreateRun(ctx, r)
	if e.starts == e.limit {
		e.cancel()
	}
	return r, err
}
func (*instantExecutor) Wait(string) <-chan struct{}          { return nil }
func (*instantExecutor) Finalizing(string) bool               { return false }
func (*instantExecutor) TakeOutcome(string) (model.Run, bool) { return model.Run{}, false }
func (*instantExecutor) Stop(string) error                    { return nil }
func (*instantExecutor) StopForShutdown(string) bool          { return false }

func TestQuickTerminalOutcomes(t *testing.T) {
	for _, status := range []string{"succeeded", "failed"} {
		t.Run(status, func(t *testing.T) {
			st, s := setup(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fake := &instantExecutor{store: st, status: status, limit: 8, cancel: cancel}
			s.exec = fake
			d := workerDef("instant", "", []string{"/bin/true"}, 30, 3)
			d.RestartDelay = 0
			if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
				t.Fatal(err)
			}
			d, _, err := st.Definition(t.Context(), d.Name)
			if err != nil {
				t.Fatal(err)
			}
			d.RestartDelay = 0
			s.loop(ctx, d)
			want := 3
			if status == "succeeded" {
				want = 8
			}
			if fake.starts != want {
				t.Fatalf("starts=%d, want %d", fake.starts, want)
			}
			if status == "succeeded" && s.State(d.Name).Failures != 0 {
				t.Fatal("successful exits consumed crash budget")
			}
		})
	}
}
