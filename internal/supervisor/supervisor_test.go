package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func setup(t *testing.T) (*store.Store, *Supervisor) {
	t.Helper()
	st, err := store.Open(t.Context(), t.TempDir())
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
	t.Cleanup(func() { ex.Shutdown(context.Background()) })
	t.Cleanup(s.Shutdown)
	return st, s
}

func workerDef(name, argv0 string, argv []string, healthyAfter string, attempts int) model.Definition {
	d := model.Definition{Name: name, Kind: model.KindWorker, Argv: argv, Shell: "/bin/sh", Timeout: "0", Grace: "0", SuccessCodes: []int{0},
		Restart: "always", RestartDelay: "10ms", HealthyAfter: healthyAfter, MaxRestartAttempts: attempts,
		Timezone: "UTC", OnOverlap: "skip", CatchUp: "none"}
	enabled := true
	d.Enabled = &enabled
	_ = argv0
	return d
}

func runCount(t *testing.T, st *store.Store, name string) int {
	t.Helper()
	runs, err := st.Runs(t.Context(), name, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(runs)
}

// Starts that never reach healthy_after are failed starts: after
// max_restart_attempts consecutive ones the slot goes fatal and stays down.
func TestWorkerGoesFatalAfterConsecutiveUnhealthyStarts(t *testing.T) {
	st, sup := setup(t)
	def := workerDef("flappy", "", []string{"/bin/true"}, "1h", 3)
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sup.Reload(defs)
	deadline := time.Now().Add(5 * time.Second)
	for runCount(t, st, "flappy") < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runCount(t, st, "flappy"); got != 3 {
		t.Fatalf("expected 3 runs (max_restart_attempts unhealthy starts), got %d", got)
	}
	// Fatal: no further restarts even after the backoff window passes.
	time.Sleep(300 * time.Millisecond)
	if got := runCount(t, st, "flappy"); got != 3 {
		t.Fatalf("slot did not stay fatal: %d runs", got)
	}
}

// An operator stop is a hold: restart = "always" must not immediately undo
// it by respawning the worker.
func TestOperatorHoldPreventsAlwaysRestart(t *testing.T) {
	st, sup := setup(t)
	def := workerDef("sleeper", "", []string{"/bin/sleep", "30"}, "0", 5)
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sup.Reload(defs)
	deadline := time.Now().Add(5 * time.Second)
	for runCount(t, st, "sleeper") < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := sup.Stop("sleeper"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	runs, err := st.Runs(t.Context(), "sleeper", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("hold was ignored: %d runs", len(runs))
	}
	if runs[0].Status != "stopped" {
		t.Fatalf("held run status = %s, want stopped", runs[0].Status)
	}
	// A manual start lifts the hold and restarts the worker.
	stored, _, err := st.Definition(t.Context(), "sleeper")
	if err != nil {
		t.Fatal(err)
	}
	sup.StartDefinition(stored)
	deadline = time.Now().Add(5 * time.Second)
	for runCount(t, st, "sleeper") < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runCount(t, st, "sleeper"); got != 2 {
		t.Fatalf("manual start did not restart: %d runs", got)
	}
}
