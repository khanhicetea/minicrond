package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigration4AddsRetryParent(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(schema, " parent_run_id TEXT,\n", "", 1)
	old = strings.Replace(old, " source TEXT NOT NULL DEFAULT '',", "", 1)
	old = strings.Replace(old, "PRAGMA user_version=6;", "PRAGMA user_version=4;", 1)
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	version, err := st.SchemaVersion(t.Context())
	if err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	var name string
	if err := st.db.QueryRowContext(t.Context(), "SELECT name FROM pragma_table_info('runs') WHERE name='parent_run_id'").Scan(&name); err != nil || name != "parent_run_id" {
		t.Fatalf("retry parent column = %q, %v", name, err)
	}
}
