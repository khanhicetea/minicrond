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
	old = strings.Replace(old, "PRAGMA user_version=7;", "PRAGMA user_version=4;", 1)
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

func TestMigration7AddsRetentionIndexes(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(schema, "CREATE INDEX idx_runs_retention ON runs(definition_id, COALESCE(ended_us,queued_us) DESC, run_id DESC) WHERE status IN ('succeeded','failed','timeout','stopped','interrupted','skipped','missed');\n", "", 1)
	old = strings.Replace(old, "CREATE INDEX idx_idempotency_run_time ON idempotency(run_id,created_us);\n", "", 1)
	old = strings.Replace(old, "PRAGMA user_version=7;", "PRAGMA user_version=6;", 1)
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
	if version, err := st.SchemaVersion(t.Context()); err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	for _, name := range []string{"idx_runs_retention", "idx_idempotency_run_time"} {
		var found string
		if err := st.db.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&found); err != nil || found != name {
			t.Fatalf("migration index %q = %q, %v", name, found, err)
		}
	}
}
