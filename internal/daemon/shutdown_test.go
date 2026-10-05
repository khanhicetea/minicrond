package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A04: a reload that is waiting for an old worker's long grace period must
// not hold up shutdown. The executor budget (shortened here) bounds shutdown;
// the reload then returns instead of restarting the worker.
func TestShutdownIsBoundedWhileReloadWaitsOnLongGraceWorker(t *testing.T) {
	old := executorShutdownBudget
	executorShutdownBudget = time.Second
	t.Cleanup(func() { executorShutdownBudget = old })

	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	write := func(command string) {
		content := "[server]\ntcp_enabled=false\n[[worker]]\nname='w'\ncommand=\"" + command + "\"\ngrace=30\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("trap '' TERM; sleep 60")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitWorkerActive(t, d, "w")

	write("trap '' TERM; sleep 61")
	reloaded := make(chan error, 1)
	go func() { reloaded <- d.Reload(context.Background()) }()
	time.Sleep(500 * time.Millisecond) // Reload now waits out the old worker's 30s grace period.

	began := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("daemon shutdown still blocked %s after cancel; it waited for the reload", time.Since(began))
	}
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("reload never returned after shutdown")
	}
}

func startTestDaemon(t *testing.T) (*Daemon, context.CancelFunc, chan error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	if err := os.WriteFile(path, []byte("[server]\ntcp_enabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		running := d.running
		d.mu.Unlock()
		if running {
			return d, cancel, done
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Review SHOULD-FIX 2: a maintenance loop that cannot be stopped must not have
// its databases closed underneath it; a normal shutdown still closes them.
func TestStuckMaintenanceLoopKeepsDatabasesOpen(t *testing.T) {
	oldBudget := maintenanceJoinBudget
	maintenanceJoinBudget = 300 * time.Millisecond
	release := make(chan struct{})
	extraMaintenanceLoop = func(context.Context) { <-release } // ignores cancellation
	t.Cleanup(func() { maintenanceJoinBudget, extraMaintenanceLoop = oldBudget, nil })

	d, cancel, done := startTestDaemon(t)
	st := d.store
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown reported success although a maintenance loop was stuck")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if _, err := st.Definitions(context.Background()); err != nil {
		t.Fatalf("metadata database was closed under a stuck maintenance loop: %v", err)
	}
	close(release)
	_ = st.Close()
}

func TestCleanShutdownClosesDatabases(t *testing.T) {
	d, cancel, done := startTestDaemon(t)
	st := d.store
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := st.Definitions(context.Background()); err == nil {
		t.Fatal("metadata database still open after a clean shutdown")
	}
}
