package supervisor

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
	"github.com/khanhicetea/minicrond/internal/store"
)

func setupWithExecutor(t *testing.T) (string, *store.Store, *executor.Service, *Supervisor) {
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
	sup := New(st, ex)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sup.BeginShutdown()
		_ = ex.Shutdown(ctx)
		_ = sup.ShutdownContext(ctx)
	})
	return dir, st, ex, sup
}

// A02: terminal writes fail while reads work, so the executor finalizes in the
// background. Supervision must keep waiting and honor restart=always once the
// state is finally terminal.
func TestWorkerRestartsAfterBackgroundFinalization(t *testing.T) {
	dir, st, ex, sup := setupWithExecutor(t)
	def := workerDef("finalize-later", "", []string{"/bin/sleep", "0.3"}, 3600, 100)
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	if _, err := saboteur.Exec(`CREATE TRIGGER reject_terminal BEFORE UPDATE ON runs WHEN NEW.status='succeeded' BEGIN SELECT RAISE(ABORT, 'injected terminal write failure'); END`); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sup.Reload(defs)
	var first model.Run
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := st.Runs(t.Context(), def.Name, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && ex.Finalizing(runs[0].ID) {
			first = runs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first lifetime never entered background finalization")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Let the supervisor observe the nonterminal row, then restore writes.
	time.Sleep(1500 * time.Millisecond)
	if got := runCount(t, st, def.Name); got != 1 {
		t.Fatalf("restarted before the previous lifetime was finalized: %d runs", got)
	}
	if _, err := saboteur.Exec(`DROP TRIGGER reject_terminal`); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for runCount(t, st, def.Name) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("worker with restart=always never started its next lifetime after background finalization")
		}
		time.Sleep(50 * time.Millisecond)
	}
	stored, err := st.Run(t.Context(), first.ID)
	if err != nil || stored.Status != "succeeded" {
		t.Fatalf("first lifetime = %s, %v; want succeeded", stored.Status, err)
	}
}

// A04: shutdown must not wait for a reload that is replacing a worker with a
// long grace period. ShutdownContext honors its deadline and the reload
// returns once the executor forces the old worker down.
func TestShutdownDoesNotWaitForReloadOfLongGraceWorker(t *testing.T) {
	_, st, ex, sup := setupWithExecutor(t)
	v1 := workerDef("stubborn", "", []string{"/bin/sh", "-c", "trap '' TERM; sleep 30"}, 0, 100)
	v1.Grace = 30
	if _, err := st.PutDefinition(t.Context(), v1, 0, "test"); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sup.Reload(defs)
	deadline := time.Now().Add(10 * time.Second)
	for sup.State("stubborn").Active == false {
		if time.Now().After(deadline) {
			t.Fatal("worker never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	v2 := v1
	v2.RestartDelay = 2 // a different canonical definition: the reload replaces the worker
	var reloaded atomic.Bool
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		sup.Reload([]model.Definition{v2})
		reloaded.Store(true)
	}()
	time.Sleep(300 * time.Millisecond) // the reload now waits out the 30s grace period
	if reloaded.Load() {
		t.Fatal("reload did not wait for the old worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = sup.ShutdownContext(ctx)
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("ShutdownContext took %s behind a long-grace reload", elapsed)
	}
	if err == nil {
		t.Fatal("ShutdownContext reported success while the old worker was still in its grace period")
	}
	forceCtx, forceCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer forceCancel()
	_ = ex.Shutdown(forceCtx)
	select {
	case <-reloadDone:
	case <-time.After(10 * time.Second):
		t.Fatal("reload never returned after shutdown")
	}
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer joinCancel()
	if err := sup.ShutdownContext(joinCtx); err != nil {
		t.Fatalf("final join = %v", err)
	}
	if got := runCount(t, st, "stubborn"); got != 1 {
		t.Fatalf("runs = %d; a replacement must not start during shutdown", got)
	}
}
