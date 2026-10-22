package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// A fresh container volume is created 0755 by the runtime. An empty directory
// owned by the daemon's user is tightened; one holding data is refused.
func TestEnsurePrivateDataDir(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "volume")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(empty, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDataDir(empty); err != nil {
		t.Fatalf("empty 0755 dir refused: %v", err)
	}
	if info, _ := os.Stat(empty); info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %#o, want 0700", info.Mode().Perm())
	}

	used := filepath.Join(t.TempDir(), "used")
	if err := os.Mkdir(used, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(used, "minicron.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(used, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDataDir(used); err == nil {
		t.Fatal("a non-empty shared directory was accepted")
	}
	if info, _ := os.Stat(used); info.Mode().Perm() != 0o755 {
		t.Fatalf("a non-empty directory was changed to %#o", info.Mode().Perm())
	}
	if err := os.Chmod(used, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDataDir(used); err != nil {
		t.Fatalf("private dir refused: %v", err)
	}
}
