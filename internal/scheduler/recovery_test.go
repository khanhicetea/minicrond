package scheduler

import (
	"path/filepath"
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
