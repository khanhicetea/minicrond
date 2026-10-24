package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/sqlite"
	"github.com/khanhicetea/minicrond/internal/store"
)

// A storage failure stops one scheduling pass, not the job's schedule: once
// the database works again, the loop resumes without a reload.
func TestLoopRecoversFromStorageFailure(t *testing.T) {
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
	sched := New(st, executor.New(st, logs, executor.Options{MaxConcurrentRuns: 4}))
	t.Cleanup(sched.Stop)
	// A second connection simulates the failure without touching the store.
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { saboteur.Close() })

	def := worker("resilient", "latest")
	def.Schedule = "@every 1s"
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	countRuns := func() int {
		runs, err := st.Runs(t.Context(), "resilient", 500)
		if err != nil {
			t.Fatal(err)
		}
		return len(runs)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("job never fired", func() bool { return countRuns() > 0 })

	if _, err := saboteur.Exec("ALTER TABLE schedule_state RENAME TO schedule_state_offline"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // long enough for at least one failed pass
	if _, err := saboteur.Exec("ALTER TABLE schedule_state_offline RENAME TO schedule_state"); err != nil {
		t.Fatal(err)
	}
	restored := countRuns()
	waitFor("schedule did not resume after the database recovered", func() bool { return countRuns() >= restored+2 })
}

// ADR-16: a job keeps firing on schedule while the metadata database cannot
// store schedule state or run rows at all. The executions are observed through
// a side effect outside the database; their history is written afterwards.
func TestJobKeepsFiringWhileMetadataIsUnwritable(t *testing.T) {
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
	sched := New(st, ex)
	t.Cleanup(sched.Stop)
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { saboteur.Close() })

	marks := filepath.Join(t.TempDir(), "fired")
	def := worker("unstoppable", "latest")
	def.Argv = nil
	def.Shell = "/bin/sh"
	def.Command = "echo x >> " + marks
	def.Schedule = "@every 1s"
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fired := func() int {
		b, _ := os.ReadFile(marks)
		return strings.Count(string(b), "x")
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	waitFor("job never fired", func() bool { return fired() > 0 })
	for _, stmt := range []string{
		`CREATE TRIGGER block_state_insert BEFORE INSERT ON schedule_state BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		`CREATE TRIGGER block_state_update BEFORE UPDATE ON schedule_state BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		`ALTER TABLE runs RENAME TO runs_offline`,
	} {
		if _, err := saboteur.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	during := fired()
	waitFor("schedule stopped while metadata was unwritable", func() bool { return fired() >= during+4 })
	if !ex.StorageDegraded() {
		t.Fatal("executor did not report degraded storage")
	}
	for _, stmt := range []string{`DROP TRIGGER block_state_insert`, `DROP TRIGGER block_state_update`, `ALTER TABLE runs_offline RENAME TO runs`} {
		if _, err := saboteur.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	// History written behind execution: the outage's runs appear afterwards.
	waitFor("deferred runs never recorded", func() bool {
		runs, err := st.Runs(t.Context(), "unstoppable", 500)
		if err != nil {
			t.Fatal(err)
		}
		return len(runs) >= during+4
	})
}

// A write lock held by another process makes every write wait SQLite's 5 s
// busy handler, which ignores the 2 s write deadline. Once the executor knows
// storage is failing, schedule-state writes are skipped so firing keeps its
// cadence (fault re-test 2026-10-24: 2 of 15 expected runs before this).
func TestJobKeepsCadenceWhileDatabaseIsWriteLocked(t *testing.T) {
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
	t.Cleanup(func() { ex.Shutdown(t.Context()) })
	sched := New(st, ex)
	t.Cleanup(sched.Stop)
	marks := filepath.Join(t.TempDir(), "fired")
	def := worker("cadence", "none")
	def.Argv = nil
	def.Shell = "/bin/sh"
	def.Command = "echo x >> " + marks
	def.Schedule = "@every 1s"
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fired := func() int {
		b, _ := os.ReadFile(marks)
		return strings.Count(string(b), "x")
	}
	if err := sched.Reload(t.Context(), defs); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); fired() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job never fired")
		}
	}
	locker, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { locker.Close() })
	conn, err := locker.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.ExecContext(t.Context(), "COMMIT"); conn.Close() })
	// The first writes of the episode each wait the busy handler; after that
	// the cadence must be close to one fire per second.
	for deadline := time.Now().Add(15 * time.Second); !ex.StorageDegraded(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("executor never noticed the locked database")
		}
	}
	time.Sleep(6 * time.Second) // let in-flight busy waits drain
	before := fired()
	time.Sleep(8 * time.Second)
	if got := fired() - before; got < 6 {
		t.Fatalf("fired %d times in 8 s while the database was write-locked", got)
	}
}
