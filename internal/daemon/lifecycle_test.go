package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/model"
)

func TestBasePathEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, old, current string
		wantBasePathError  bool
	}{
		{"legacy ignored", "invalid", "", false},
		{"new validated", "", "invalid", true},
		{"new takes precedence", "invalid", "/valid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BASE_PATH", tc.old)
			t.Setenv("MINICROND_BASE_PATH", tc.current)
			dir := t.TempDir()
			path := filepath.Join(dir, "minicron.toml")
			if err := os.WriteFile(path, []byte("[server]\nbind='127.0.0.1:99999'\nunix_socket=false\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
			err := d.Run(t.Context())
			if err == nil || strings.Contains(err.Error(), "MINICROND_BASE_PATH") != tc.wantBasePathError {
				t.Fatalf("unexpected startup error: %v", err)
			}
		})
	}
}

func TestStartupFailureStopsExecutionAndReleasesLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	config := "[server]\nbind='127.0.0.1:99999'\nunix_socket=false\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	if err := d.Run(t.Context()); err == nil {
		t.Fatal("expected startup failure")
	}
	if d.exec == nil {
		t.Fatal("startup failed before the execution service was created")
	}
	if _, err := d.exec.Trigger(t.Context(), model.Definition{}, "", "manual", nil); !errors.Is(err, executor.ErrShutdown) {
		t.Fatalf("executor still accepts work after startup failure: %v", err)
	}
	if err := d.Reload(t.Context()); err == nil {
		t.Fatal("reload accepted after shutdown")
	}
	next := &Daemon{DataDir: d.DataDir}
	if err := next.acquireLock(); err != nil {
		t.Fatalf("startup failure leaked lock: %v", err)
	}
	next.releaseLock()
}
