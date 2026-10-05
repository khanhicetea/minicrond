package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// [reads] sizes the admission gate once at startup, so a reload that changes it
// is refused (like storage.synchronous) instead of silently not applying.
func TestReloadRejectsChangedReadsLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	write := func(reads string) {
		if err := os.WriteFile(path, []byte("[server]\ntcp_enabled=false\n"+reads), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[reads]\nslots=2\nbudget=16\nwork_timeout=10\n")
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

	if err := d.Reload(ctx); err != nil {
		t.Fatalf("reload with unchanged [reads]: %v", err)
	}
	for _, changed := range []string{
		"[reads]\nslots=3\nbudget=16\nwork_timeout=10\n",
		"[reads]\nslots=2\nbudget=32\nwork_timeout=10\n",
		"[reads]\nslots=2\nbudget=16\nwork_timeout=11\n",
		"", // dropping the block selects the defaults, which differ from the running values
	} {
		write(changed)
		err := d.Reload(ctx)
		if err == nil || !strings.Contains(err.Error(), "[reads]") || !strings.Contains(err.Error(), "restart") {
			t.Fatalf("reload with %q = %v, want a restart-required error", changed, err)
		}
	}
	// The running daemon is unaffected by the refused reloads.
	write("[reads]\nslots=2\nbudget=16\nwork_timeout=10\n")
	if err := d.Reload(ctx); err != nil {
		t.Fatalf("reload after restoring [reads]: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}
