package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
)

func synchronousOf(t *testing.T, ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) int {
	t.Helper()
	var mode int
	if err := q.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}

// ADR-18: a relaxed write runs with synchronous=NORMAL (1) and the writer is
// back to FULL (2) afterwards; a store configured with NORMAL stays NORMAL.
func TestRelaxedSwitchesSynchronousAndRestores(t *testing.T) {
	for _, tc := range []struct {
		name          string
		opt           sqlite.Options
		inside, after int
	}{
		{"full", sqlite.Options{}, 1, 2},
		{"normal", sqlite.Options{Synchronous: "normal"}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(t.Context(), t.TempDir(), tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got := synchronousOf(t, t.Context(), s.db); got != tc.after {
				t.Fatalf("writer synchronous before = %d, want %d", got, tc.after)
			}
			err = s.relaxed(t.Context(), func(ctx context.Context, conn *sql.Conn) error {
				if got := synchronousOf(t, ctx, conn); got != tc.inside {
					t.Errorf("synchronous inside relaxed = %d, want %d", got, tc.inside)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := synchronousOf(t, t.Context(), s.db); got != tc.after {
				t.Fatalf("writer synchronous after = %d, want %d", got, tc.after)
			}
		})
	}
}

// A failed relaxed statement still restores FULL and returns its own error.
func TestRelaxedRestoresAfterStatementError(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.StartRun(t.Context(), "missing", 1, 1, "id", time.Now()); err == nil {
		t.Fatal("StartRun of a missing run succeeded")
	}
	if _, err := s.execRelaxed(t.Context(), "INSERT INTO no_such_table VALUES(1)"); err == nil {
		t.Fatal("invalid statement succeeded")
	}
	if got := synchronousOf(t, t.Context(), s.db); got != 2 {
		t.Fatalf("writer synchronous = %d after failed relaxed writes, want 2 (FULL)", got)
	}
}

// A canceled caller does not leave the writer relaxed: the restore uses its
// own context.
func TestRelaxedRestoresWhenCallerCanceled(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(t.Context())
	err = s.relaxed(ctx, func(context.Context, *sql.Conn) error {
		cancel()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("canceled relaxed write reported success")
	}
	if got := synchronousOf(t, t.Context(), s.db); got != 2 {
		t.Fatalf("writer synchronous = %d after a canceled relaxed write, want 2 (FULL)", got)
	}
}

const crashHelperEnv = "MINICROND_RELAXED_CRASH_DIR"

// ADR-18 rests on WAL + synchronous=NORMAL surviving a process crash. The
// helper process commits the running transition and the schedule state
// relaxed and then dies with SIGKILL, with no Close; the parent must see both.
func TestRelaxedWritesSurviveDaemonCrash(t *testing.T) {
	if dir := os.Getenv(crashHelperEnv); dir != "" {
		crashAfterRelaxedWrites(dir)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRelaxedWritesSurviveDaemonCrash$")
	cmd.Env = append(os.Environ(), crashHelperEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("helper did not die by SIGKILL: %v\n%s", err, out)
	}
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Run(t.Context(), "crash-run")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "running" || r.PID != 4242 || r.PGID != 4242 || r.ProcessStartID != "start-id" {
		t.Fatalf("running state lost by a process crash: %+v", r)
	}
	defs, err := s.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, last, next, hash, err := s.ScheduleState(t.Context(), defs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "crash-hash" || last.UnixMicro() != crashLast.UnixMicro() || next.UnixMicro() != crashLast.Add(time.Minute).UnixMicro() {
		t.Fatalf("schedule state lost by a process crash: hash=%q last=%v next=%v", hash, last, next)
	}
}

var crashLast = time.UnixMicro(1_700_000_000_000_000)

func crashAfterRelaxedWrites(dir string) {
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		os.Exit(3)
	}
	d, err := s.PutDefinition(ctx, scheduledDefinition("crash-job"), 0, "test")
	if err != nil {
		os.Exit(4)
	}
	now := time.Now()
	if err := s.CreateRun(ctx, model.Run{ID: "crash-run", DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Status: "pending", Trigger: "manual", Attempt: 1, QueuedAt: now}); err != nil {
		os.Exit(5)
	}
	if err := s.StartRun(ctx, "crash-run", 4242, 4242, "start-id", now); err != nil {
		os.Exit(6)
	}
	if err := s.SetScheduleStateWithNext(ctx, d.ID, "crash-hash", crashLast, crashLast, crashLast.Add(time.Minute)); err != nil {
		os.Exit(7)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}

func scheduledDefinition(name string) model.Definition {
	enabled := true
	return model.Definition{Name: name, Kind: model.KindJob, Enabled: &enabled, Schedule: "@every 1m", Timezone: "UTC", CatchUp: "none", OnOverlap: "skip",
		Command: "true", Shell: "/bin/sh", SuccessCodes: []int{0}}
}

// LastScheduledFire is the newest scheduled occurrence with a row of any
// status; manual and retry runs do not count.
func TestLastScheduledFire(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := s.PutDefinition(t.Context(), scheduledDefinition("job"), 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if last, err := s.LastScheduledFire(t.Context(), d.ID); err != nil || !last.IsZero() {
		t.Fatalf("no runs: last=%v err=%v", last, err)
	}
	base := time.UnixMicro(1_700_000_000_000_000)
	at := func(m int) *time.Time { v := base.Add(time.Duration(m) * time.Minute); return &v }
	now := time.Now()
	for _, r := range []model.Run{
		{ID: "a", Status: "succeeded", Trigger: "schedule", ScheduledFor: at(1), EndedAt: &now},
		{ID: "b", Status: "skipped", Trigger: "schedule", ScheduledFor: at(3), EndedAt: &now},
		{ID: "c", Status: "missed", Trigger: "schedule", ScheduledFor: at(2), EndedAt: &now},
		{ID: "d", Status: "pending", Trigger: "retry", ScheduledFor: at(9)},
		{ID: "e", Status: "pending", Trigger: "manual"},
	} {
		r.DefinitionID, r.Job, r.Kind, r.Attempt, r.QueuedAt = d.ID, d.Name, d.Kind, 1, now
		if err := s.CreateRun(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.LastScheduledFire(t.Context(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !last.Equal(*at(3)) {
		t.Fatalf("last scheduled fire = %v, want %v", last, *at(3))
	}
	var plan string
	var a, b int
	if err := s.db.QueryRowContext(t.Context(), "EXPLAIN QUERY PLAN SELECT MAX(scheduled_for_us) FROM runs WHERE definition_id=? AND trigger='schedule' AND scheduled_for_us IS NOT NULL", d.ID).Scan(&a, &b, new(int), &plan); err != nil {
		t.Fatal(err)
	}
	if plan == "" || !strings.Contains(plan, "idx_runs_schedule_occurrence") {
		t.Fatalf("LastScheduledFire does not use the occurrence index: %q", plan)
	}
}
