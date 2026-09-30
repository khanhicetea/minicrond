package scheduler

import (
	"math"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestIntervalNextFireRejectsOverflow(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, interval := range []time.Duration{1, time.Duration(math.MaxInt64) / 2, time.Duration(math.MaxInt64)} {
		t.Run(interval.String(), func(t *testing.T) {
			schedule := compiledSchedule{raw: "@every " + interval.String(), interval: interval}
			first, err := schedule.nextFire(anchor, anchor)
			if err != nil || !first.Equal(anchor.Add(interval)) {
				t.Fatalf("first fire = %s, %v", first, err)
			}
			if next, err := schedule.nextFire(anchor.Add(time.Duration(math.MaxInt64)), anchor); err == nil {
				t.Fatalf("overflow produced fire %s instead of an error", next)
			}
			if next, err := schedule.nextFire(anchor.AddDate(500, 0, 0), anchor); err == nil {
				t.Fatalf("saturated subtraction produced fire %s instead of an error", next)
			}
		})
	}
}

func TestSchedulerPanicClosesLoopAndAllowsShutdown(t *testing.T) {
	s := New(nil, nil)
	d := model.Definition{Name: "panic", Kind: model.KindJob, Schedule: "@every 1h", Timezone: "UTC"}
	if err := s.Reload(t.Context(), []model.Definition{d}); err != nil {
		t.Fatal(err)
	}
	done := s.running[d.Name].done
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("panicked scheduling loop did not release its lifecycle")
	}
	s.Stop()
}

func TestSchedulerRetainsIndependentDefinition(t *testing.T) {
	st, _, s := setup(t)
	d := worker("snapshot", "none")
	d.Schedule = "@every 1h"
	d.Env = map[string]string{"MODE": "original"}
	d.Argv = []string{"/bin/true"}
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
	defs[0].Env["MODE"] = "changed"
	defs[0].Argv[0] = "/bin/false"
	owned := s.running[d.Name].def
	if owned.Env["MODE"] != "original" || owned.Argv[0] != "/bin/true" {
		t.Fatalf("scheduler retained caller-owned fields: %+v", owned)
	}
	s.Stop()
}
