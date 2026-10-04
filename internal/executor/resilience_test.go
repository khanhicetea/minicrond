package executor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
	"github.com/khanhicetea/minicrond/internal/store"
)

func resilienceService(t *testing.T, opt Options) (string, *store.Store, *Service) {
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
	s := New(st, logs, opt)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return dir, st, s
}

func putJob(t *testing.T, st *store.Store, d model.Definition) (model.Definition, string) {
	t.Helper()
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	stored, hash, err := st.Definition(t.Context(), d.Name)
	if err != nil {
		t.Fatal(err)
	}
	return stored, hash
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// When storage is unavailable longer than the inline persistence budget, the
// terminal state and completion callback still arrive once storage recovers.
func TestTerminalStatePersistsAfterStorageOutage(t *testing.T) {
	finished := make(chan model.Run, 1)
	dir, st, s := resilienceService(t, Options{OnFinished: func(r model.Run, _ model.Definition) { finished <- r }})
	d, hash := putJob(t, st, model.Definition{Name: "outage", Kind: model.KindJob, Command: "sleep 0.5", Shell: "/bin/sh", OnOverlap: "skip", SuccessCodes: []int{0}})
	run, err := s.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "run never started", func() bool {
		r, err := st.Run(t.Context(), run.ID)
		return err == nil && r.Status == "running"
	})
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	if _, err := saboteur.Exec("ALTER TABLE runs RENAME TO runs_offline"); err != nil {
		t.Fatal(err)
	}
	// Outlast the five-second inline retry budget.
	time.Sleep(6500 * time.Millisecond)
	if _, err := saboteur.Exec("ALTER TABLE runs_offline RENAME TO runs"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-finished:
		if r.ID != run.ID || r.Status != "succeeded" {
			t.Fatalf("completion = %s/%s", r.ID, r.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("completion callback never ran after storage recovered")
	}
	stored, err := st.Run(t.Context(), run.ID)
	if err != nil || stored.Status != "succeeded" {
		t.Fatalf("stored run = %s, %v", stored.Status, err)
	}
}

// A full concurrency gate is reported distinctly from the overlap policy, and
// a retry that meets it waits for capacity instead of being dropped.
func TestCapacityGateSkipsAndDefersRetries(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, blockerHash := putJob(t, st, model.Definition{Name: "blocker", Kind: model.KindJob, Command: "sleep 2", Shell: "/bin/sh", OnOverlap: "parallel", SuccessCodes: []int{0}})
	flaky, flakyHash := putJob(t, st, model.Definition{Name: "flaky-gate", Kind: model.KindJob, Command: "exit 1", Shell: "/bin/sh", OnOverlap: "skip", Retries: 1, RetryDelay: 1, SuccessCodes: []int{0}})

	first, err := s.Trigger(t.Context(), flaky, flakyHash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if done := s.Wait(first.ID); done != nil {
		<-done
	}
	// Occupy the only slot before the retry delay elapses.
	if _, err := s.Trigger(t.Context(), blocker, blockerHash, "manual", nil); err != nil {
		t.Fatal(err)
	}
	declined, err := s.Trigger(t.Context(), blocker, blockerHash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if declined.Status != "skipped" || declined.EndReason != "queue_full" {
		t.Fatalf("over-capacity trigger = %s/%s, want skipped/queue_full", declined.Status, declined.EndReason)
	}
	eventually(t, 10*time.Second, "deferred retry never ran", func() bool {
		runs, err := st.Runs(t.Context(), "flaky-gate", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.Attempt == 2 {
				if r.Status == "skipped" {
					t.Fatalf("retry was dropped as %s", r.EndReason)
				}
				return r.Status == "failed"
			}
		}
		return false
	})
}
