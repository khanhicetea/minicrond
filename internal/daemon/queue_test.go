package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const queueTestConfig = `[server]
tcp_enabled=false
[scheduler]
max_concurrent_runs=1
[queue]
drain_rate=100
[[job]]
name='blocker'
schedule='@every 24h'
on_overlap='parallel'
command='sleep 30'
grace=1
[[job]]
name='later'
schedule='@every 24h'
on_overlap='parallel'
command='echo ran-after-restart'
`

func waitDaemonReady(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		ready := d.running && d.exec != nil
		d.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon never became ready")
}

// A queued run survives a daemon restart and is started by the new daemon; the
// run that was executing at shutdown is interrupted, never replayed.
func TestQueuedRunSurvivesDaemonRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	if err := os.WriteFile(path, []byte(queueTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")

	ctx1, cancel1 := context.WithCancel(t.Context())
	d1 := &Daemon{ConfigPath: path, DataDir: data}
	done1 := make(chan error, 1)
	go func() { done1 <- d1.Run(ctx1) }()
	waitDaemonReady(t, d1)
	blockerDef, blockerHash, err := d1.store.Definition(t.Context(), "blocker")
	if err != nil {
		t.Fatal(err)
	}
	laterDef, laterHash, err := d1.store.Definition(t.Context(), "later")
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := d1.exec.Trigger(t.Context(), blockerDef, blockerHash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := d1.exec.Trigger(t.Context(), laterDef, laterHash, "manual", nil)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("queue: %s, %v", queued.Status, err)
	}
	cancel1()
	select {
	case err := <-done1:
		if err != nil {
			t.Fatalf("first daemon: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("first daemon did not stop")
	}

	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	d2 := &Daemon{ConfigPath: path, DataDir: data}
	done2 := make(chan error, 1)
	go func() { done2 <- d2.Run(ctx2) }()
	waitDaemonReady(t, d2)
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := d2.store.Run(t.Context(), queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued run after restart = %s/%s", run.Status, run.EndReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The blocker that was executing at shutdown is not re-run.
	runs, err := d2.store.Runs(t.Context(), "blocker", 10)
	if err != nil || len(runs) != 1 || runs[0].ID != blocker.ID || runs[0].Status == "queued" || runs[0].Status == "running" || runs[0].Status == "pending" {
		t.Fatalf("blocker runs after restart = %+v, %v", runs, err)
	}
	cancel2()
	<-done2
}

// Queue limits are fixed at startup; a reload that changes them is refused.
func TestReloadRefusesQueueSettingChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	if err := os.WriteFile(path, []byte(queueTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitDaemonReady(t, d)
	changed := strings.Replace(queueTestConfig, "drain_rate=100", "drain_rate=7", 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Reload(ctx); err == nil || !strings.Contains(err.Error(), "[queue]") {
		t.Fatalf("reload with changed queue settings = %v", err)
	}
	if err := os.WriteFile(path, []byte(queueTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Reload(ctx); err != nil {
		t.Fatalf("reload with unchanged settings: %v", err)
	}
	cancel()
	<-done
}
