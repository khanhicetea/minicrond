package store

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestAlertObservations(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	version, err := s.SchemaVersion(t.Context())
	if err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	def, err := s.PutDefinition(t.Context(), model.Definition{Name: "example", Kind: model.KindJob}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	r := model.Run{ID: "run-1", DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Status: "pending", Trigger: "manual", QueuedAt: time.Now().UTC()}
	if err := s.CreateRun(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAlert(t.Context(), r.ID, "ops", "queued", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.InterruptAlerts(t.Context()); err != nil {
		t.Fatal(err)
	}
	items, err := s.RunAlerts(t.Context(), r.ID)
	if err != nil || len(items) != 1 || items[0].Status != "interrupted" {
		t.Fatalf("interrupted = %#v, %v", items, err)
	}
	if err := s.RecordAlert(t.Context(), r.ID, "ops", "failed", 3, "Telegram API returned 503"); err != nil {
		t.Fatal(err)
	}
	counts, err := s.AlertMetrics(t.Context())
	if err != nil || counts["failed"] != 1 {
		t.Fatalf("metrics = %#v, %v", counts, err)
	}
	items, err = s.RunAlerts(t.Context(), r.ID)
	if err != nil || len(items) != 1 || items[0].Attempts != 3 || items[0].LastError == "" {
		t.Fatalf("delivery = %#v, %v", items, err)
	}
}
