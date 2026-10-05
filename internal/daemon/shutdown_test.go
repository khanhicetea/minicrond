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
