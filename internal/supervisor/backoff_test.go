package supervisor

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestRestartDelayDoublesUpToCap(t *testing.T) {
	d := model.Definition{RestartDelay: 2}
	if got := restartDelay(d, 10); got != 2*time.Second {
		t.Fatalf("without backoff: %v", got)
	}
	d.RestartBackoffMax = 30
	for failures, want := range map[int]time.Duration{0: 2 * time.Second, 1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 4: 16 * time.Second, 5: 30 * time.Second, 1000: 30 * time.Second} {
		if got := restartDelay(d, failures); got != want {
			t.Fatalf("failures %d: got %v, want %v", failures, got, want)
		}
	}
}

// With restart_backoff_max a crash-looping worker is never retired: it keeps
// restarting past max_restart_attempts and no exhaustion is recorded.
func TestBackoffWorkerIsNotRetired(t *testing.T) {
	st, sup := setup(t)
	def := workerDef("looping", "", []string{"/bin/false"}, 3600, 2)
	def.RestartBackoffMax = 1
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Reload(defs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for runCount(t, st, "looping") < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runCount(t, st, "looping"); got < 4 {
		t.Fatalf("worker stopped restarting after %d runs (max_restart_attempts 2)", got)
	}
	if ex, err := st.WorkerExhaustions(t.Context()); err != nil || len(ex) != 0 {
		t.Fatalf("exhaustion recorded in backoff mode: %v %v", ex, err)
	}
}
