package supervisor

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/store"
)

func TestWorkerLogOpenFailureUsesRestartBudget(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	root := t.TempDir()
	logs, err := logstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	ex := executor.New(st, logs, executor.Options{})
	sup := New(st, ex)
	t.Cleanup(func() {
		sup.Shutdown()
		if err := ex.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	d := workerDef("no-log", "", []string{"/bin/true"}, 3600, 3)
	d.RestartDelay = 0
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sup.Reload(defs)
	sup.mu.Lock()
	done := sup.workerDone[d.Name]
	sup.mu.Unlock()
	if done != nil {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			t.Fatal("worker did not exhaust its restart budget")
		}
	}
	if got := runCount(t, st, d.Name); got != 3 {
		t.Fatalf("got %d failed starts, want 3", got)
	}
	if state := sup.State(d.Name); state.Failures != 3 || state.Active {
		t.Fatalf("state = %#v", state)
	}
}

func TestShutdownPreventsManualStartsAndJoinsRestart(t *testing.T) {
	st, sup := setup(t)
	d := workerDef("shutdown-restart", "", []string{"/bin/sleep", "30"}, 0, 3)
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sup.Reload(defs)
	sup.Restart(defs[0])
	sup.BeginShutdown()
	sup.Shutdown()
	before := runCount(t, st, d.Name)
	sup.StartDefinition(defs[0])
	sup.Restart(defs[0])
	sup.Reload(defs)
	if after := runCount(t, st, d.Name); after != before {
		t.Fatalf("shutdown admitted work: before=%d after=%d", before, after)
	}
	if state := sup.State(d.Name); state.Active {
		t.Fatalf("still active: %#v", state)
	}
}
