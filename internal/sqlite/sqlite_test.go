package sqlite

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenPathsAndReplacementSettings(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, path := range []string{"relative.db", filepath.Join(root, "reserved ?#%.db")} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.PingContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database not created at literal path: %v", err)
			}
			db.SetMaxIdleConns(0)
			db.SetMaxIdleConns(1)
			for _, setting := range []struct {
				name string
				want int
			}{{"foreign_keys", 1}, {"synchronous", 2}, {"busy_timeout", 5000}} {
				var got int
				if err := db.QueryRowContext(t.Context(), "PRAGMA "+setting.name).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != setting.want {
					t.Errorf("%s = %d, want %d", setting.name, got, setting.want)
				}
			}
		})
	}
}
