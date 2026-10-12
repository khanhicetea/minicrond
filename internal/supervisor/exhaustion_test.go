package supervisor

import (
	"context"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestExhaustionPersistsNotifiesAndClears(t *testing.T) {
	st, s := setup(t)
	d := workerDef("fatal", "", []string{"/bin/false"}, 30, 3)
	var err error
	d, err = st.PutDefinition(t.Context(), d, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	fake := &instantExecutor{store: st, status: "failed"}
	s.exec = fake
	events := 0
	s.onExhausted = func(def model.Definition, outcome model.WorkerExhaustion) {
		events++
		saved, err := st.WorkerExhaustions(t.Context())
		if err != nil || saved[d.Name].LastRunID != outcome.LastRunID {
			t.Errorf("notification preceded persistence: %v %v", saved, err)
		}
		if def.Name != d.Name || outcome.Attempts != 3 {
			t.Errorf("bad event: %+v", outcome)
		}
	}
	d.RestartDelay = 0
	s.loop(t.Context(), d)
	if events != 1 || fake.starts != 3 {
		t.Fatalf("events=%d starts=%d", events, fake.starts)
	}
	// A fresh supervisor can query persisted evidence without any memory state.
	fresh := New(st, nil)
	states, err := fresh.ReadStates(t.Context(), []string{d.Name})
	if err != nil {
		t.Fatal(err)
	}
	outcome := states[d.Name].Exhaustion
	if states[d.Name].Status != "exhausted" || outcome == nil || outcome.LastRunID != "instant-3" || outcome.Reason == "" {
		t.Fatalf("state: %+v", states)
	}
	if err := s.StartDefinition(d); err != nil {
		t.Fatal(err)
	}
	saved, err := st.WorkerExhaustions(t.Context())
	if err != nil || len(saved) != 0 || s.State(d.Name).Exhaustion != nil {
		t.Fatalf("not cleared: %v %v", saved, err)
	}
}

func TestOldExhaustionCannotUndoReset(t *testing.T) {
	st, s := setup(t)
	d := workerDef("fatal", "", []string{"/bin/false"}, 30, 3)
	var err error
	d, err = st.PutDefinition(t.Context(), d, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := s.epochs[d.Name]
	if err := s.resetExhaustion(d.Name); err != nil {
		t.Fatal(err)
	}
	if s.persistExhaustion(t.Context(), d, oldEpoch, 3, "", "start_error") {
		t.Fatal("stale write accepted")
	}
	states, err := st.WorkerExhaustions(t.Context())
	if err != nil || len(states) != 0 {
		t.Fatalf("stale state: %v %v", states, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if s.persistExhaustion(ctx, d, s.epochs[d.Name], 3, "", "start_error") {
		t.Fatal("canceled write accepted")
	}
}

func TestReloadClearsPersistedDormantExhaustion(t *testing.T) {
	st, s := setup(t)
	d := workerDef("fatal", "", []string{"/bin/false"}, 30, 3)
	no := false
	d.Autostart = &no
	var err error
	d, err = st.PutDefinition(t.Context(), d, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !s.persistExhaustion(t.Context(), d, 0, 3, "", "start_error") {
		t.Fatal("not persisted")
	}
	if err := s.Reload([]model.Definition{d}); err != nil {
		t.Fatal(err)
	}
	saved, err := st.WorkerExhaustions(t.Context())
	if err != nil || len(saved) != 0 {
		t.Fatalf("reload retained state: %v %v", saved, err)
	}
}
