package store

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestResourceUsageRoundTripAndReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	d, err := st.PutDefinition(t.Context(), model.Definition{Name: "accounting", Kind: model.KindJob}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	usage := &model.ResourceUsage{UserCPUUS: 12345, SystemCPUUS: 678, PeakRSSBytes: 4 << 20}
	for _, id := range []string{"measured", "unavailable", "zero"} {
		at := time.Now().UTC()
		r := model.Run{ID: id, DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Status: "pending", Trigger: "schedule", Attempt: 1, QueuedAt: at, ScheduledFor: &at}
		if err := st.CreateRun(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		var u *model.ResourceUsage
		if id == "measured" {
			u = usage
		}
		if id == "zero" {
			u = &model.ResourceUsage{}
		}
		// Retrying an identical final write must preserve its accounting.
		for range 2 {
			if err := st.FinishRunWithUsage(t.Context(), id, "succeeded", "exit", nil, "", at, 0, false, u); err != nil {
				t.Fatal(err)
			}
		}
		scheduled, err := st.ScheduledRun(t.Context(), d.ID, at)
		if err != nil || scheduled.ID != id || (u == nil) != (scheduled.ResourceUsage == nil) {
			t.Fatalf("scheduled run: %+v, %v", scheduled, err)
		}
		if u != nil && *scheduled.ResourceUsage != *u {
			t.Fatalf("scheduled usage: %+v", scheduled.ResourceUsage)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := st.Runs(t.Context(), d.Name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %d", len(runs))
	}
	for _, r := range runs {
		detail, err := st.ReadRun(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []model.Run{r, detail} {
			switch got.ID {
			case "measured":
				if got.ResourceUsage == nil || *got.ResourceUsage != *usage {
					t.Fatalf("usage = %+v", got.ResourceUsage)
				}
				clone := got.Clone()
				clone.ResourceUsage.UserCPUUS++
				if *got.ResourceUsage != *usage {
					t.Fatal("clone aliases accounting")
				}
			case "unavailable":
				if got.ResourceUsage != nil {
					t.Fatalf("unavailable usage = %+v", got.ResourceUsage)
				}
			case "zero":
				if got.ResourceUsage == nil || *got.ResourceUsage != (model.ResourceUsage{}) {
					t.Fatalf("zero usage = %+v", got.ResourceUsage)
				}
			}
		}
	}
}
