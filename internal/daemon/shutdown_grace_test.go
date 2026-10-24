package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// scheduler.shutdown_grace caps the grace of workers too (Docker's 10 s stop
// timeout), however their stop reaches them.
func TestShutdownGraceCapsWorkerGrace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	marker := filepath.Join(dir, "started")
	content := "[server]\ntcp_enabled=false\n[scheduler]\nshutdown_grace=1\n[[worker]]\nname='stubborn'\ngrace=30\ncommand=\"trap '' TERM; touch " + marker + "; while true; do sleep 0.2; done\"\n" +
		// Also run a job, as a real config would. Which stop reaches the
		// worker first is a race; the supervisor test pins the stop kind.
		"[[job]]\nname='tick'\nschedule='@every 1s'\ncommand='true'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	began := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("shutdown took %v with shutdown_grace = 1 and a worker grace of 30", took)
	}
}
