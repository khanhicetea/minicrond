package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func waitWorkerActive(t *testing.T, d *Daemon, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		ready := d.running && d.super != nil && d.super.State(name).Active
		d.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker %s never became active", name)
}

func returnsWithin(t *testing.T, what string, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return: reload deadlocked with a running worker", what)
	}
}

// A reload that replaces a running worker waits for its run to finish. The
// run's completion callback must not need a lock the reload holds.
func TestReloadReplacesRunningWorker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	write := func(command string) {
		content := "[server]\ntcp_enabled=false\n[[worker]]\nname='w'\ncommand='" + command + "'\ngrace=1\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sleep 30")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitWorkerActive(t, d, "w")

	write("sleep 31")
	returnsWithin(t, "config reload", func() error { return d.Reload(ctx) })
	waitWorkerActive(t, d, "w")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("daemon did not shut down after reload")
	}
}

// The API path edits the registry and then reconciles.
func TestReconcileDisablesRunningWorker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	if err := os.WriteFile(path, []byte("[server]\ntcp_enabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		running := d.running
		d.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	worker := model.Definition{Name: "api-worker", Kind: model.KindWorker, Command: "sleep 30", Shell: "/bin/sh", Restart: "always", RestartDelay: 1, MaxRestartAttempts: 5, Grace: 1, LogOnFull: "drop_old", EnvBase: "clean", Timezone: "UTC"}
	if _, err := d.store.PutDefinition(ctx, worker, 0, "test"); err != nil {
		t.Fatal(err)
	}
	returnsWithin(t, "reconcile after create", func() error { return d.Reconcile(ctx) })
	waitWorkerActive(t, d, "api-worker")

	if err := d.store.SetEnabled(ctx, "api-worker", false); err != nil {
		t.Fatal(err)
	}
	returnsWithin(t, "reconcile after disable", func() error { return d.Reconcile(ctx) })

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
