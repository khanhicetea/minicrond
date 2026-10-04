package store

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
)

func TestPruneMetadataKeepsNewestAudit(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 25 {
		if _, err := s.db.ExecContext(t.Context(), "INSERT INTO audit(at_us,actor,action,target) VALUES(?,?,?,?)", i, "test", "put", "job"); err != nil {
			t.Fatal(err)
		}
	}
	var newest int64
	if err := s.db.QueryRowContext(t.Context(), "SELECT MAX(id) FROM audit").Scan(&newest); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []int{100, 10} {
		if err := s.PruneMetadata(t.Context(), keep); err != nil {
			t.Fatal(err)
		}
	}
	var count, oldest int64
	if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*), MIN(id) FROM audit").Scan(&count, &oldest); err != nil {
		t.Fatal(err)
	}
	if count != 10 || oldest != newest-9 {
		t.Fatalf("kept %d audit rows from id %d, want the 10 newest ending at %d", count, oldest, newest)
	}
}

func TestRecordAlertsAppliesBatchInOrder(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def, err := s.PutDefinition(t.Context(), model.Definition{Name: "example", Kind: model.KindJob}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := s.CreateRun(t.Context(), model.Run{ID: id, DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Status: "pending", Trigger: "manual", QueuedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	err = s.RecordAlerts(t.Context(), []AlertUpdate{
		{RunID: "a", Channel: "ops", Status: "queued"},
		{RunID: "b", Channel: "ops", Status: "queued"},
		{RunID: "a", Channel: "ops", Status: "failed", Attempts: 3, LastError: "boom"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.RunAlerts(t.Context(), "a")
	if err != nil || len(a) != 1 || a[0].Status != "failed" || a[0].Attempts != 3 || a[0].LastError != "boom" {
		t.Fatalf("run a alerts = %+v, error %v", a, err)
	}
	b, err := s.RunAlerts(t.Context(), "b")
	if err != nil || len(b) != 1 || b[0].Status != "queued" {
		t.Fatalf("run b alerts = %+v, error %v", b, err)
	}
	// One bad row rolls back the whole batch.
	err = s.RecordAlerts(t.Context(), []AlertUpdate{{RunID: "b", Channel: "ops", Status: "sent", Attempts: 1}, {RunID: "missing", Channel: "ops", Status: "sent"}})
	if err == nil {
		t.Fatal("expected a foreign key failure")
	}
	if b, _ := s.RunAlerts(t.Context(), "b"); len(b) != 1 || b[0].Status != "queued" {
		t.Fatalf("failed batch was partly applied: %+v", b)
	}
}

// Listings are served by the read pool and see every committed write.
func TestReadPoolSeesCommittedRuns(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir(), sqlite.Options{Synchronous: "normal"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var sync int
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&sync); err != nil || sync != 1 {
		t.Fatalf("synchronous = %d, error %v; want NORMAL (1)", sync, err)
	}
	def, err := s.PutDefinition(t.Context(), model.Definition{Name: "example", Kind: model.KindJob}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		r := model.Run{ID: string(rune('a' + i)), DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Status: "pending", Trigger: "manual", QueuedAt: time.Now().UTC()}
		if err := s.CreateRun(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(t.Context(), def.Name, 10)
		if err != nil || len(runs) != i+1 {
			t.Fatalf("listing after %d writes returned %d runs, error %v", i+1, len(runs), err)
		}
	}
	if _, err := s.rdb.ExecContext(t.Context(), "DELETE FROM runs"); err == nil {
		t.Fatal("read pool accepted a write")
	}
}
