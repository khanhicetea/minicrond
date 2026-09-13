package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/model"
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
	if err := st.SyncFiles(t.Context(), []model.Definition{d}, false); err != nil {
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
