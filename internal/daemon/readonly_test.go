package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A data directory the daemon cannot write (read-only volume, chmod) still
// runs its scheduled jobs as long as the metadata database is readable
// (ADR-16). Only an unreadable or missing database stays fatal.
func TestReadOnlyDataDirectoryKeepsJobsRunning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	marker := filepath.Join(dir, "ticks")
	path := filepath.Join(dir, "minicron.toml")
	content := "[server]\ntcp_enabled=false\n[[job]]\nname='tick'\nschedule='@every 1s'\ncommand='echo x >> " + marker + "'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runFor := func(want func() bool) error {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		d := &Daemon{ConfigPath: path, DataDir: data}
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		deadline := time.After(10 * time.Second)
		for !want() {
			select {
			case err := <-done:
				return err
			case <-deadline:
				cancel()
				<-done
				t.Fatal("job did not fire")
			case <-time.After(50 * time.Millisecond):
			}
		}
		cancel()
		return <-done
	}
	ticks := func() int {
		b, _ := os.ReadFile(marker)
		n := 0
		for _, c := range b {
			if c == '\n' {
				n++
			}
		}
		return n
	}
	if err := runFor(func() bool { return ticks() >= 1 }); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Make the whole data directory read-only, as a read-only volume would be,
	// and drop the lock file the first run created.
	if err := os.Remove(filepath.Join(data, "minicron.lock")); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	filepath.WalkDir(data, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for _, p := range dirs {
		os.Chmod(p, 0o500)
	}
	t.Cleanup(func() {
		for _, p := range dirs {
			os.Chmod(p, 0o700)
		}
	})
	before := ticks()
	if err := runFor(func() bool { return ticks() >= before+2 }); err != nil {
		t.Fatalf("read-only run: %v", err)
	}
}
