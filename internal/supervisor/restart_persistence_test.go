package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

type gatedExecutor struct {
	blockingExecutor
	first   atomic.Bool
	release chan struct{}
}

func (e *gatedExecutor) Trigger(ctx context.Context, d model.Definition, _, _ string, _ *time.Time) (model.Run, error) {
	first := !e.first.Swap(true)
	select {
	case e.started <- d:
	case <-ctx.Done():
	}
	<-ctx.Done()
	if first {
		<-e.release
	}
	return model.Run{}, ctx.Err()
}

func TestAcceptedRestartDoesNotRequireSecondClear(t *testing.T) {
	st, s := setup(t)
	fake := &gatedExecutor{blockingExecutor: blockingExecutor{started: make(chan model.Definition, 1)}, release: make(chan struct{})}
	s.exec = fake
	d := workerDef("restart", "", []string{"/bin/true"}, 30, 3)
	if err := s.Reload([]model.Definition{d}); err != nil {
		t.Fatal(err)
	}
	<-fake.started
	if err := s.Restart(d); err != nil {
		close(fake.release)
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		close(fake.release)
		t.Fatal(err)
	}
	close(fake.release)
	select {
	case <-fake.started:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted restart attempted a second metadata clear")
	}
}
