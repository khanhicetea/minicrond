package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrustedPathRequiresRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	write := func(paths string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("[server]\ntcp_enabled=false\n[scheduler]\ntrusted_path="+paths+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	original := "['/usr/local/bin','/usr/bin','/bin']"
	write(original)
	ctx, cancel := context.WithCancel(t.Context())
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("shutdown timed out")
		}
	}()
	waitDaemonReady(t, d)
	if err := d.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	write("['/usr/bin','/bin']")
	if err := d.Reload(ctx); err == nil || !strings.Contains(err.Error(), "scheduler.trusted_path requires daemon restart") {
		t.Fatalf("reload: %v", err)
	}
	write(original)
	if err := d.Reload(ctx); err != nil {
		t.Fatal(err)
	}
}
