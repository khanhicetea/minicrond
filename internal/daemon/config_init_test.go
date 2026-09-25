package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/store"
)

func TestConfigInitRunsBeforeReady(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	marker := filepath.Join(dir, "initialized")
	content := "[server]\ntcp_enabled=false\n[[init]]\nname='prepare'\ncommand='touch " + marker + "'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for {
		d.mu.Lock()
		ready := d.running
		d.mu.Unlock()
		if ready {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("daemon stopped before ready: %v", err)
		case <-deadline:
			t.Fatal("daemon did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("init did not finish before ready: %v", err)
	}
	st, err := store.Open(t.Context(), d.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	st.Close()
	if err != nil || len(defs) != 1 || defs[0].Source != "config" {
		t.Fatalf("definitions = %+v, %v", defs, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFailedConfigInitStopsStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	if err := os.WriteFile(path, []byte("[server]\ntcp_enabled=false\n[[init]]\nname='fail'\ncommand='exit 7'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	if err := d.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "init fail") {
		t.Fatalf("startup error = %v", err)
	}
	d.mu.Lock()
	ready := d.running
	d.mu.Unlock()
	if ready {
		t.Fatal("failed init left daemon ready")
	}
}
