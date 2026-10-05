package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func mustCompile(t *testing.T, expr, zone string) compiledSchedule {
	t.Helper()
	c, err := compileSchedule(model.Definition{Schedule: expr, Timezone: zone})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fires walks the schedule exactly as the scheduling loop does (each fire is
// the next input and the watermark) and returns the fires in [from, to).
func fires(t *testing.T, c compiledSchedule, from, to time.Time) []time.Time {
	t.Helper()
	var out []time.Time
	last := time.Time{}
	after := from.Add(-time.Nanosecond)
	for range 100000 {
		next, err := c.nextFireDistinct(after, from, last)
		if err != nil {
			t.Fatal(err)
		}
		if !next.Before(to) {
			return out
		}
		if len(out) > 0 && !next.After(out[len(out)-1]) {
			t.Fatalf("fire %s does not advance past %s", next.UTC(), out[len(out)-1].UTC())
		}
		out = append(out, next)
		last, after = next, next
	}
	t.Fatal("too many fires")
	return nil
}

// assertOncePerWallMinute fails when two fires share a wall-clock minute.
func assertOncePerWallMinute(t *testing.T, got []time.Time, loc *time.Location) {
	t.Helper()
	seen := map[string]time.Time{}
	for _, f := range got {
		key := f.In(loc).Format("2006-01-02 15:04")
		if prev, dup := seen[key]; dup {
			t.Errorf("wall minute %s fired twice: %s and %s", key, prev.UTC().Format(time.RFC3339), f.UTC().Format(time.RFC3339))
		}
		seen[key] = f
	}
}

type foldCase struct {
	zone string
	day  [3]int // local date of the fold
	size time.Duration
}

var foldCases = []foldCase{
	{"America/New_York", [3]int{2026, 11, 1}, time.Hour},
	{"Europe/Berlin", [3]int{2026, 10, 25}, time.Hour},
	{"Australia/Lord_Howe", [3]int{2026, 4, 5}, 30 * time.Minute}, // non-hour fold
}

func (fc foldCase) bounds(t *testing.T) (loc *time.Location, from, to time.Time) {
	t.Helper()
	loc = mustLoc(t, fc.zone)
	from = time.Date(fc.day[0], time.Month(fc.day[1]), fc.day[2], 0, 0, 0, 0, loc)
	to = time.Date(fc.day[0], time.Month(fc.day[1]), fc.day[2]+1, 0, 0, 0, 0, loc)
	if got := to.Sub(from); got != 24*time.Hour+fc.size {
		t.Fatalf("%s %v: local day lasts %s; the fold moved", fc.zone, fc.day, got)
	}
	return loc, from, to
}

// A08: `* * * * *` fires once per wall minute across the fold, not twice.
func TestFoldEveryMinuteFiresEachWallMinuteOnce(t *testing.T) {
	for _, fc := range foldCases {
		t.Run(fc.zone, func(t *testing.T) {
			loc, from, to := fc.bounds(t)
			got := fires(t, mustCompile(t, "* * * * *", fc.zone), from, to)
			assertOncePerWallMinute(t, got, loc)
			if len(got) != 24*60 {
				t.Fatalf("got %d fires on the fold day, want %d", len(got), 24*60)
			}
		})
	}
}

// A08: a schedule with several fires inside the repeated hour must not repeat
// the earlier wall minutes in the second copy (01:45 EDT → 01:00 EST).
func TestFoldMultiFireScheduleDoesNotRepeat(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	c := mustCompile(t, "*/15 1 * * *", "America/New_York")
	last := time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC) // 01:45 EDT
	for _, anchor := range []time.Time{last.Add(-24 * time.Hour), last} {
		got, err := c.nextFireDistinct(last, anchor, last)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 11, 2, 1, 0, 0, 0, ny)
		if !got.Equal(want) {
			t.Fatalf("after 01:45 EDT: got %s, want %s", got, want)
		}
	}
	for _, fc := range foldCases {
		loc, from, to := fc.bounds(t)
		got := fires(t, mustCompile(t, "*/15 1 * * *", fc.zone), from, to)
		assertOncePerWallMinute(t, got, loc)
		if len(got) != 4 {
			t.Errorf("%s: got %d fires, want 4 (01:00 01:15 01:30 01:45): %v", fc.zone, len(got), got)
		}
	}
}

// A08: one fire per day for schedules inside the fold, across one-hour and
// non-hour folds, over the days around the transition.
func TestFoldDailyScheduleFiresOncePerDay(t *testing.T) {
	for _, fc := range foldCases {
		for _, expr := range []string{"0 1 * * *", "30 1 * * *", "45 1 * * *", "59 1 * * *"} {
			t.Run(fc.zone+"/"+expr, func(t *testing.T) {
				loc, from, to := fc.bounds(t)
				got := fires(t, mustCompile(t, expr, fc.zone), from.AddDate(0, 0, -2), to.AddDate(0, 0, 2))
				if len(got) != 5 {
					t.Fatalf("got %d fires over five days, want 5: %v", len(got), got)
				}
				days := map[string]bool{}
				for _, f := range got {
					day := f.In(loc).Format("2006-01-02")
					if days[day] {
						t.Errorf("two fires on %s", day)
					}
					days[day] = true
				}
			})
		}
	}
}

// A08: fold behavior survives restart/recovery. A loop restarted inside the
// second copy resumes from "now", which must stay suppressed too.
func TestFoldPreservedAcrossRestart(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	secondCopy := time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC) // 01:10 EST
	for _, tc := range []struct {
		expr string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 11, 1, 2, 0, 0, 0, ny)},
		{"*/15 1 * * *", time.Date(2026, 11, 2, 1, 0, 0, 0, ny)},
		{"30 1 * * *", time.Date(2026, 11, 2, 1, 30, 0, 0, ny)},
	} {
		c := mustCompile(t, tc.expr, "America/New_York")
		// Persisted watermark from the first copy, restart inside the second.
		last := time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC)
		got, err := c.nextFireDistinct(maxTime(last, secondCopy), secondCopy.Add(-72*time.Hour), last)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s restart in fold: got %s, want %s", tc.expr, got, tc.want)
		}
	}
	// Catch-up after downtime spanning the fold counts each wall minute once:
	// last fire 01:59 EDT, now 02:30 EST: fires 02:00..02:30 = 31, not 91.
	c := mustCompile(t, "* * * * *", "America/New_York")
	last := time.Date(2026, 11, 1, 5, 59, 0, 0, time.UTC)
	now := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC)
	got, err := c.overdue(t.Context(), last.Add(-time.Hour), last, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.count != 31 || !got.exact || !got.first.Equal(time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)) || !got.latest.Equal(now) {
		t.Fatalf("catch-up across fold: %+v", got)
	}
}

// A09: restart before the first fire. The anchor and pending fire are
// persisted but `last` is still empty; the overdue first occurrence counts.
func TestOverdueFirstOccurrenceWithoutLastFire(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	anchor := now.Add(-3 * time.Hour)
	c := mustCompile(t, "@every 1h", "UTC")
	got, err := c.overdue(t.Context(), anchor, time.Time{}, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.count != 3 || !got.first.Equal(anchor.Add(time.Hour)) || !got.latest.Equal(anchor.Add(3*time.Hour)) {
		t.Fatalf("@every without last: %+v", got)
	}
	// Cron: anchor 12:27:30, now 12:30:00 → fires 12:28, 12:29, 12:30.
	c = mustCompile(t, "* * * * *", "UTC")
	got, err = c.overdue(t.Context(), now.Add(-150*time.Second), time.Time{}, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.count != 3 || !got.latest.Equal(now) {
		t.Fatalf("cron without last: %+v", got)
	}
	// A fresh anchor has nothing overdue.
	if got, err = c.overdue(t.Context(), now, time.Time{}, now.Add(30*time.Second), true); err != nil || got.count != 0 {
		t.Fatalf("fresh anchor: %+v, %v", got, err)
	}
}

// A09: downtime beyond the old 10,000-tick cap must select the true latest
// occurrence and report the true count.
func TestOverdueBeyondTenThousandCronTicks(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 30, 20, 0, time.UTC)
	last := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC).Add(-10020 * time.Minute)
	c := mustCompile(t, "* * * * *", "UTC")
	wantLatest := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)

	got, err := c.overdue(t.Context(), last.Add(-time.Hour), last, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.count != 10020 || !got.exact || !got.latest.Equal(wantLatest) {
		t.Fatalf("catch_up=none: %+v, want count 10020 latest %s", got, wantLatest)
	}
	got, err = c.overdue(t.Context(), last.Add(-time.Hour), last, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.count == 0 || !got.latest.Equal(wantLatest) {
		t.Fatalf("catch_up=latest: %+v, want latest %s", got, wantLatest)
	}
}

// A09: when counting would exceed its CPU bound the count is flagged as a
// lower bound while the latest occurrence stays exact.
func TestOverdueCountLowerBoundIsReported(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 30, 20, 0, time.UTC)
	last := now.AddDate(0, -4, 0) // ~175k minutes
	c := mustCompile(t, "* * * * *", "UTC")
	got, err := c.overdue(t.Context(), last, last, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.exact || got.count < maxCatchUpCount {
		t.Fatalf("expected a flagged lower bound, got %+v", got)
	}
	if want := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC); !got.latest.Equal(want) {
		t.Fatalf("latest = %s, want %s", got.latest, want)
	}
}

// The backward search must agree with plain enumeration for dense, sparse and
// DST-affected schedules.
func TestLatestFireMatchesEnumeration(t *testing.T) {
	zones := []string{"UTC", "America/New_York", "Australia/Lord_Howe"}
	exprs := []string{"* * * * *", "*/7 * * * *", "0 3 * * 1", "30 1 * * *", "*/15 1 * * *", "0 0 1 * *", "0 2 * * *", "15 4 29 2 *", "@hourly"}
	for _, zone := range zones {
		for _, expr := range exprs {
			c := mustCompile(t, expr, zone)
			start := time.Date(2023, 12, 31, 23, 59, 0, 0, time.UTC)
			for _, span := range []time.Duration{30 * time.Minute, 5 * time.Hour, 3 * 24 * time.Hour, 400 * 24 * time.Hour} {
				for _, offset := range []time.Duration{0, 41 * 24 * time.Hour, 301*24*time.Hour + 17*time.Minute} {
					last := start.Add(offset)
					now := last.Add(span)
					var want time.Time
					for cur := last; ; {
						next, err := c.nextFireDistinct(cur, last, last)
						if err != nil {
							t.Fatal(err)
						}
						if next.After(now) {
							break
						}
						want, cur = next, next
					}
					got, err := c.overdue(context.Background(), last, last, now, false)
					if err != nil {
						t.Fatal(err)
					}
					if want.IsZero() {
						if got.count != 0 {
							t.Fatalf("%s %s last=%s span=%s: got %+v, want none", zone, expr, last, span, got)
						}
						continue
					}
					if !got.latest.Equal(want) {
						t.Fatalf("%s %s last=%s span=%s: latest %s, want %s", zone, expr, last, span, got.latest, want)
					}
				}
			}
		}
	}
}

func awaitRun(t *testing.T, find func() (model.Run, bool)) model.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r, ok := find(); ok {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatal("expected run was not recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A09 through the real scheduler: the daemon stopped after persisting the
// anchor and pending fire but before the first fire.
func TestRestartBeforeFirstFireCatchesUp(t *testing.T) {
	for _, policy := range []string{"latest", "none"} {
		t.Run(policy, func(t *testing.T) {
			st, _, sched := setup(t)
			def := worker("first-fire-"+policy, policy)
			def.Schedule = "@every 1h"
			if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
				t.Fatal(err)
			}
			defs, err := st.Definitions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			anchor := time.Now().UTC().Add(-3*time.Hour - 10*time.Minute).Truncate(time.Microsecond)
			if err := st.SetScheduleStateWithNext(t.Context(), defs[0].ID, scheduleHash(defs[0]), anchor, time.Time{}, anchor.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := sched.Reload(t.Context(), defs); err != nil {
				t.Fatal(err)
			}
			r := awaitRun(t, func() (model.Run, bool) {
				runs, err := st.Runs(t.Context(), def.Name, 10)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range runs {
					if (policy == "none") == (r.Status == "missed") {
						return r, true
					}
				}
				return model.Run{}, false
			})
			if r.ScheduledFor == nil || !r.ScheduledFor.Equal(anchor.Add(3*time.Hour)) {
				t.Fatalf("scheduled_for = %v, want %s", r.ScheduledFor, anchor.Add(3*time.Hour))
			}
			if policy == "none" && r.MissedCount != 3 {
				t.Fatalf("missed_count = %d, want 3", r.MissedCount)
			}
		})
	}
}

// A09 through the real scheduler: more than 10,000 cron ticks of downtime.
func TestCatchUpAfterMoreThanTenThousandCronTicks(t *testing.T) {
	for _, policy := range []string{"latest", "none"} {
		t.Run(policy, func(t *testing.T) {
			st, _, sched := setup(t)
			def := worker("long-downtime-"+policy, policy)
			def.Schedule = "* * * * *"
			if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
				t.Fatal(err)
			}
			defs, err := st.Definitions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now().UTC()
			last := start.Truncate(time.Minute).Add(-10020 * time.Minute)
			if err := st.SetScheduleState(t.Context(), defs[0].ID, scheduleHash(defs[0]), last.Add(-time.Hour), last); err != nil {
				t.Fatal(err)
			}
			if err := sched.Reload(t.Context(), defs); err != nil {
				t.Fatal(err)
			}
			r := awaitRun(t, func() (model.Run, bool) {
				runs, err := st.Runs(t.Context(), def.Name, 10)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range runs {
					if (policy == "none") == (r.Status == "missed") {
						return r, true
					}
				}
				return model.Run{}, false
			})
			// The latest missed minute is the one just passed, never one of the
			// ~20-minute-old ticks the old safety bound selected.
			if r.ScheduledFor == nil || r.ScheduledFor.Before(start.Truncate(time.Minute)) || r.ScheduledFor.After(time.Now()) {
				t.Fatalf("scheduled_for = %v, want the latest minute at %s", r.ScheduledFor, start.Truncate(time.Minute))
			}
			if policy == "none" && (r.MissedCount < 10020 || r.MissedCount > 10021) {
				t.Fatalf("missed_count = %d, want 10020", r.MissedCount)
			}
		})
	}
}
