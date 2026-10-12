package supervisor

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// A reload can happen after exhaustion is committed but before the loop leaves.
// Either it replaces a retiring loop, or the reset epoch keeps that loop alive.
func TestReloadDuringExhaustionNotificationRestarts(t *testing.T) {
	st, s := setup(t)
	d := workerDef("reset-at-exhaustion", "", []string{"/bin/false"}, 30, 1)
	var err error
	d, err = st.PutDefinition(t.Context(), d, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	d.RestartDelay = 0
	s.exec = &instantExecutor{store: st, status: "failed"}
	events := make(chan struct{}, 2)
	release := make(chan struct{})
	first := true
	s.onExhausted = func(_ model.Definition, _ model.WorkerExhaustion) {
		events <- struct{}{}
		if first {
			first = false
			select {
			case <-release:
			case <-s.stopped:
			}
		}
	}
	if err := s.Reload([]model.Definition{d}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("first exhaustion missing")
	}
	if err := s.Reload([]model.Definition{d}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("reload lost worker while exhausted loop was leaving")
	}
}

func TestReloadDiscardsOldResetNamesWithoutReusingEpochs(t *testing.T) {
	_, s := setup(t)
	if err := s.resetExhaustion("removed-worker"); err != nil {
		t.Fatal(err)
	}
	old := s.epochLocked("removed-worker")
	if err := s.resetExhaustion(""); err != nil {
		t.Fatal(err)
	}
	if len(s.epochs) != 0 || s.epochLocked("removed-worker") == old {
		t.Fatal("reload retained old names or reused their epoch")
	}
	base := s.epochLocked("unrelated-worker")
	if err := s.resetExhaustion("new-worker"); err != nil {
		t.Fatal(err)
	}
	if s.epochLocked("unrelated-worker") != base {
		t.Fatal("named reset invalidated an unrelated worker")
	}
}

func TestReloadReplacesRetiringLoop(t *testing.T) {
	_, s := setup(t)
	fake := &blockingExecutor{started: make(chan model.Definition, 1)}
	s.exec = fake
	d := workerDef("retiring", "", []string{"/bin/false"}, 30, 3)
	if err := s.Reload([]model.Definition{d}); err != nil {
		t.Fatal(err)
	}
	<-fake.started
	s.mu.Lock()
	s.retiring[d.Name] = true
	s.mu.Unlock()
	if err := s.Reload([]model.Definition{d}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.started:
	case <-time.After(5 * time.Second):
		t.Fatal("retiring loop was retained")
	}
}
