package scheduler

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// ADR-18: schedule_state is committed relaxed, so after a power loss it can
// trail the durable run rows. The newest scheduled run row is then the last
// fire: here the stored state says the last fire was the anchor (195 minutes
// ago, every 30 minutes), but a row proves the occurrence at anchor+120m
// fired. Only anchor+150m and anchor+180m are missed, not six occurrences, and
// none of the already-recorded ones is fired again.
func TestStaleScheduleStateUsesNewestScheduledRun(t *testing.T) {
	st, _, sched := setup(t)
	if _, err := st.PutDefinition(t.Context(), worker("stale-state", "none"), 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	d := defs[0]
	anchor := time.Now().UTC().Add(-195 * time.Minute).Truncate(time.Second)
	if err := st.SetScheduleState(t.Context(), d.ID, scheduleHash(d), anchor, anchor); err != nil {
		t.Fatal(err)
	}
	fired := anchor.Add(120 * time.Minute)
	ended := fired.Add(time.Second)
	if err := st.CreateRun(t.Context(), model.Run{ID: "fired-before-power-loss", DefinitionID: d.ID, Job: d.Name, Kind: d.Kind,
		Status: "succeeded", Trigger: "schedule", Attempt: 1, ScheduledFor: &fired, QueuedAt: fired, EndedAt: &ended}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		runs, err := st.Runs(t.Context(), d.Name, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.Status != "missed" {
				continue
			}
			if r.MissedCount != 2 {
				t.Fatalf("missed_count = %d, want 2 (counted from the newest run row, not the stale state)", r.MissedCount)
			}
			if r.ScheduledFor == nil || !r.ScheduledFor.Equal(anchor.Add(180*time.Minute)) {
				t.Fatalf("missed run scheduled_for = %v, want %v", r.ScheduledFor, anchor.Add(180*time.Minute))
			}
			_, last, _, _, err := st.ScheduleState(t.Context(), d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !last.Equal(anchor.Add(180 * time.Minute)) {
				t.Fatalf("stored last fire = %v, want it advanced to %v", last, anchor.Add(180*time.Minute))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no missed run recorded; runs: %+v", runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
