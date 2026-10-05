package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// ADR-9: a catch_up = "latest" occurrence that finds the gate full is queued
// (not skipped) and consumes its schedule occurrence exactly once, so a
// reload does not fire it again; it then runs when capacity frees up.
func TestCatchUpLatestQueuesWhenGateIsFull(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ex := executor.New(st, logs, executor.Options{MaxConcurrentRuns: 1, Queue: executor.QueueOptions{DrainRate: 100}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ex.Shutdown(ctx)
	})
	sched := New(st, ex)
	t.Cleanup(sched.Stop)

	blockerDef := model.Definition{Name: "blocker", Kind: model.KindJob, Command: "sleep 1.5", Shell: "/bin/sh", OnOverlap: "parallel", SuccessCodes: []int{0}}
	if _, err := st.PutDefinition(t.Context(), blockerDef, 0, "test"); err != nil {
		t.Fatal(err)
	}
	blocker, bh, err := st.Definition(t.Context(), "blocker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}

	def := worker("caught-up", "latest")
	def.Schedule = "@every 1h"
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var caught model.Definition
	for _, d := range defs {
		if d.Name == "caught-up" {
			caught = d
		}
	}
	anchor := time.Now().UTC().Add(-3*time.Hour - 10*time.Minute).Truncate(time.Microsecond)
	if err := st.SetScheduleStateWithNext(t.Context(), caught.ID, scheduleHash(caught), anchor, time.Time{}, anchor.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	queued := awaitRun(t, func() (model.Run, bool) {
		runs, err := st.Runs(t.Context(), "caught-up", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			return r, true
		}
		return model.Run{}, false
	})
	if queued.Status != "queued" || queued.Trigger != "schedule" || queued.ScheduledFor == nil || !queued.ScheduledFor.Equal(anchor.Add(3*time.Hour)) {
		t.Fatalf("catch-up with a full gate = %+v", queued)
	}
	// Reloading must not fire the consumed occurrence a second time.
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := st.Run(t.Context(), queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued catch-up run = %s/%s", got.Status, got.EndReason)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if runs, err := st.Runs(t.Context(), "caught-up", 10); err != nil || len(runs) != 1 {
		t.Fatalf("occurrence fired more than once: %+v, %v", runs, err)
	}
}
