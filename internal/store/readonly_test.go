package store

import (
	"os"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

// A data directory the daemon cannot write still boots when its metadata
// database is readable: definitions are read, writes fail as unavailable.
func TestOpenReadOnlyDataDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutDefinition(t.Context(), model.Definition{Name: "kept", Kind: model.KindJob, Command: "true"}, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"minicron.db-wal", "minicron.db-shm"} {
		if _, err := os.Stat(dir + "/" + name); err == nil {
			t.Fatalf("%s left after close", name)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	ro, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer ro.Close()
	if !ro.ReadOnly() {
		t.Fatal("store not reported read-only")
	}
	defs, err := ro.Definitions(t.Context())
	if err != nil || len(defs) != 1 || defs[0].Name != "kept" {
		t.Fatalf("definitions = %+v, %v", defs, err)
	}
	if _, err := ro.PutDefinition(t.Context(), model.Definition{Name: "new", Kind: model.KindJob, Command: "true"}, 0, "test"); err == nil {
		t.Fatal("write to a read-only store succeeded")
	}
}

// Without a database there is nothing to run: an unwritable empty directory
// stays fatal.
func TestOpenReadOnlyDirectoryWithoutDatabaseFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if s, err := Open(t.Context(), dir); err == nil {
		s.Close()
		t.Fatal("opened a missing database read-only")
	}
}
