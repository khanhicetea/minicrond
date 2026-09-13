package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/model"
)

func TestStartupFailureStopsExecutionAndReleasesLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	config := "[server]\nbind='127.0.0.1:99999'\nunix_socket=false\n[[job]]\nname='scheduled'\ncommand='true'\nschedule='@every 1s'\n"
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
