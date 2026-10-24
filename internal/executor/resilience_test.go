package executor

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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
// completion callback is not delayed by it and the terminal state is stored
// once storage recovers (ADR-16).
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
	eventually(t, 15*time.Second, "terminal state never stored after recovery", func() bool {
		stored, err := st.Run(t.Context(), run.ID)
		return err == nil && stored.Status == "succeeded"
	})
}

// A full concurrency gate is reported distinctly from the overlap policy, and
// a retry that meets it waits for capacity instead of being dropped.
func TestCapacityGateSkipsAndDefersRetries(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{Disabled: true}})
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

// ADR-16: a database that cannot record the running transition no longer
// stops the process. The run completes normally and its history (running and
// terminal state) is written once storage accepts writes again.
func TestRunningStateWriteFailureDoesNotKillProcess(t *testing.T) {
	dir, st, s := resilienceService(t, Options{})
	d, hash := putJob(t, st, model.Definition{Name: "keep-going", Kind: model.KindJob, Command: "sleep 1; exit 0", Shell: "/bin/sh", OnOverlap: "skip", SuccessCodes: []int{0}})
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	if _, err := saboteur.Exec(`CREATE TRIGGER reject_updates BEFORE UPDATE ON runs BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`); err != nil {
		t.Fatal(err)
	}
	run, err := s.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := s.Wait(run.ID)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run never finished")
	}
	if _, err := saboteur.Exec(`DROP TRIGGER reject_updates`); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "history never written", func() bool {
		r, err := st.Run(t.Context(), run.ID)
		return err == nil && r.Status == "succeeded" && r.StartedAt != nil && r.PID > 0
	})
}

// ADR-16: when the run row itself cannot be inserted, the run still executes
// and the row is inserted later.
func TestRunExecutesWhenRowInsertFails(t *testing.T) {
	var storedMu sync.Mutex
	stored := map[string]bool{}
	var s *Service
	dir, st, s := resilienceService(t, Options{OnRunStored: func(id string) {
		storedMu.Lock()
		defer storedMu.Unlock()
		// The row is stored and no longer pending when the callback runs.
		stored[id] = !s.RowPending(id)
	}})
	marker := filepath.Join(t.TempDir(), "ran")
	d, hash := putJob(t, st, model.Definition{Name: "no-row", Kind: model.KindJob, Command: "touch " + marker, Shell: "/bin/sh", OnOverlap: "skip", SuccessCodes: []int{0}})
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	// Another process holds the write lock: the insert times out (busy).
	if _, err := saboteur.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	run, err := s.Trigger(t.Context(), d, hash, "schedule", nil)
	if err != nil {
		t.Fatal(err)
	}
	// SQLite's busy handler (5 s) is the bound; it does not observe contexts.
	if took := time.Since(started); took > 7*time.Second {
		t.Fatalf("launch waited %s for the locked database", took)
	}
	waitDone(t, s, run.ID)
	// While storage is known to be failing, later launches skip the database.
	started = time.Now()
	second, err := s.Trigger(t.Context(), d, hash, "schedule", nil)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("second launch waited %s although storage was known to be failing", took)
	}
	waitDone(t, s, second.ID)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("run did not execute: %v", err)
	}
	if deferred, _ := s.History(); deferred != 2 {
		t.Fatalf("deferred = %d, want 2", deferred)
	}
	if !s.RowPending(run.ID) {
		t.Fatal("a run admitted without a row is not reported pending")
	}
	if _, err := saboteur.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "deferred rows never written", func() bool {
		a, errA := st.Run(t.Context(), run.ID)
		b, errB := st.Run(t.Context(), second.ID)
		return errA == nil && errB == nil && a.Status == "succeeded" && b.Status == "succeeded"
	})
	storedMu.Lock()
	defer storedMu.Unlock()
	if !stored[run.ID] || !stored[second.ID] {
		t.Fatalf("run-stored callback = %v", stored)
	}
}
