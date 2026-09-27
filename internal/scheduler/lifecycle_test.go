package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestNextFireRejectsInvalidSchedules(t *testing.T) {
	now := time.Now().UTC()
	for _, schedule := range []string{"@every 0s", "@every -1s", "@every invalid", "0 0 31 2 *"} {
		t.Run(schedule, func(t *testing.T) {
			if _, err := nextFire(model.Definition{Schedule: schedule, Timezone: "UTC"}, now, now); err == nil {
				t.Fatal("expected invalid schedule error")
			}
		})
	}
}

func TestReloadRejectsCanceledContext(t *testing.T) {
	s := New(nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Reload(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestStopJoinsSchedulingLoops(t *testing.T) {
	st, _, s := setup(t)
	d := worker("stopped", "none")
	d.Schedule = "@every 1h"
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := s.Reload(t.Context(), defs); err != nil {
			t.Fatal(err)
		}
	}
	s.Stop()
	// Stop must include loops still initializing, not only loops on timers.
	s.loops.Wait()
	runs, err := st.Runs(t.Context(), d.Name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("unexpected runs: %v", runs)
	}
}

func TestReloadKeepsUnchangedLoopAndJoinsChangedLoop(t *testing.T) {
	st, _, s := setup(t)
	d := worker("selective", "none")
	d.Schedule = "0 0 1 1 *"
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	first := s.running[d.Name]
	deadline := time.Now().Add(3 * time.Second)
	for {
		next, err := st.ScheduleNextBatch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !next[defs[0].ID].IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first loop did not publish next fire")
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	if s.running[d.Name].done != first.done {
		t.Fatal("unchanged definition restarted its loop")
	}
	changed := defs[0]
	changed.Command = "printf changed"
	changed.Revision++
	if err := s.Reload(t.Context(), []model.Definition{changed}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.done:
	default:
		t.Fatal("changed loop was not joined before replacement")
	}
	second := s.running[d.Name]
	if second.done == first.done {
		t.Fatal("changed definition kept the old loop")
	}
	if err := s.Reload(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second.done:
	default:
		t.Fatal("removed definition left its loop running")
	}
	if len(s.running) != 0 {
		t.Fatalf("running loops = %d, want 0", len(s.running))
	}
}
