package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/executor"
	"github.com/khanhicetea/minicrond/logstore"
	"github.com/khanhicetea/minicrond/model"
	"github.com/khanhicetea/minicrond/store"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Spring forward: the 02:00 wall time does not exist on 2026-03-08 in
// America/New_York. The occurrence fires once, at the end of the gap.
func TestNextFireDSTGapFiresOnceAtGapEnd(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	d := model.Definition{Schedule: "0 2 * * *", Timezone: "America/New_York"}
	after := time.Date(2026, 3, 7, 12, 0, 0, 0, ny)
	got, err := nextFire(d, after, after)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 3, 8, 3, 0, 0, 0, ny)
	if !got.Equal(want) {
		t.Fatalf("gap fire: got %s, want %s", got.UTC(), want.UTC())
	}
}

// Fall back: 01:00 occurs twice on 2026-11-01. The occurrence fires on the
// first (earlier) instance only; the second is suppressed, not re-fired.
func TestNextFireDSTFoldFiresFirstOccurrenceOnce(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	d := model.Definition{Schedule: "0 1 * * *", Timezone: "America/New_York"}
	after := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	got, err := nextFire(d, after, after)
	if err != nil {
		t.Fatal(err)
	}
	if got.UTC() != time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC) {
		t.Fatalf("fold fire: got %s, want first 01:00 occurrence (05:00Z)", got.UTC())
	}
}

// @every keeps its persisted anchor across restarts so schedules do not
// drift by daemon uptime.
func TestEveryIntervalAnchorsToPersistedAnchor(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	d := model.Definition{Schedule: "@every 30m", Timezone: "UTC"}
	for _, tc := range []struct{ after, want time.Time }{
		{time.Date(2026, 1, 1, 10, 41, 0, 0, time.UTC), time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)},
		{time.Date(2026, 1, 2, 9, 59, 0, 0, time.UTC), time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)},
		{anchor, time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)},
	} {
		got, err := nextFire(d, tc.after, anchor)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(tc.want) {
			t.Fatalf("after %s: got %s, want %s", tc.after, got, tc.want)
		}
	}
}

// Steady state: a freshly reloaded @every job must actually fire on its
// interval. Guards against a recompute-after-wait livelock where reaching the
// pending slot recomputed `next` strictly-after `now`, deferring every
// occurrence by one interval forever.
func TestReloadFiresEveryInterval(t *testing.T) {
	st, _, sched := setup(t)
	def := worker("ticker-job", "none")
	def.Schedule = "@every 2s"
	if err := st.SyncFiles(t.Context(), []model.Definition{def}, false); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := st.Runs(t.Context(), "ticker-job", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("@every 2s job did not fire within 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func scheduleHash(d model.Definition) string {
	sum := sha256.Sum256([]byte(d.Schedule + "\x00" + d.Timezone))
	return hex.EncodeToString(sum[:])
}

func setup(t *testing.T) (*store.Store, *executor.Service, *Scheduler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ex := executor.New(st, logs, executor.Options{MaxConcurrentRuns: 4})
	s := New(st, ex)
	t.Cleanup(s.Stop)
	return st, ex, s
}

func worker(name, catchUp string) model.Definition {
	d := model.Definition{Name: name, Kind: model.KindJob, Authority: "file", SourceFile: "test",
		Schedule: "@every 30m", Timezone: "UTC", CatchUp: catchUp, OnOverlap: "skip",
		Command: "true", Shell: "/bin/sh", Timeout: "0", Grace: "0", SuccessCodes: []int{0}}
	enabled := true
	d.Enabled = &enabled
	return d
}

// Downtime with catch_up = "none": one summarized missed run is recorded,
// nothing executes. The anchor is 195 minutes old, so exactly six 30-minute
// occurrences elapsed (anchor+30m .. anchor+180m).
func TestCatchUpNoneRecordsSummarizedMissedRun(t *testing.T) {
	st, _, sched := setup(t)
	def := worker("missed-job", "none")
	if err := st.SyncFiles(t.Context(), []model.Definition{def}, false); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UTC().Add(-195 * time.Minute)
	if err := st.SetScheduleState(t.Context(), defs[0].ID, scheduleHash(defs[0]), anchor, anchor); err != nil {
		t.Fatal(err)
	}
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		runs, err := st.Runs(t.Context(), "missed-job", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.Status == "missed" {
				if r.MissedCount != 6 {
					t.Fatalf("missed_count = %d, want 6", r.MissedCount)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no missed run recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A changed schedule must not create catch-up occurrences from before the
// new revision existed: a fresh anchor resets the fire history.
func TestScheduleChangeResetsAnchorNotCatchUp(t *testing.T) {
	st, _, sched := setup(t)
	def := worker("changed-job", "none")
	if err := st.SyncFiles(t.Context(), []model.Definition{def}, false); err != nil {
		t.Fatal(err)
	}
	defs, _ := st.Definitions(t.Context())
	anchor := time.Now().UTC().Add(-195 * time.Minute)
	// Persist state under the hash of a DIFFERENT schedule, as if the
	// definition just changed. Reload must reset the anchor, not catch up.
	old := scheduleHash(model.Definition{Schedule: "@every 5m", Timezone: "UTC"})
	if err := st.SetScheduleState(t.Context(), defs[0].ID, old, anchor, anchor); err != nil {
		t.Fatal(err)
	}
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	runs, err := st.Runs(t.Context(), "changed-job", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("schedule change produced %d catch-up records, want 0", len(runs))
	}
}
