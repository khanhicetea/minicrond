package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func TestAlertParkWritesObservationsAfterTheRunIsStored(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	def, err := st.PutDefinition(t.Context(), model.Definition{Name: "job", Kind: model.KindJob}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	pending := map[string]bool{"deferred": true}
	p := newAlertPark(st, func(id string) bool { mu.Lock(); defer mu.Unlock(); return pending[id] })

	// The run row is deferred: observations are parked, not failed.
	if err := p.record(t.Context(), []store.AlertUpdate{{RunID: "deferred", Channel: "ops", Status: "queued"}}); err != nil {
		t.Fatal(err)
	}
	// A newer observation also waits behind the row.
	if err := p.record(t.Context(), []store.AlertUpdate{{RunID: "deferred", Channel: "ops", Status: "sent", Attempts: 1}}); err != nil {
		t.Fatal(err)
	}
	// History lost entirely: discarded without error.
	if err := p.record(t.Context(), []store.AlertUpdate{{RunID: "gone", Channel: "ops", Status: "sent"}}); err != nil {
		t.Fatal(err)
	}
	if p.count != 2 {
		t.Fatalf("parked %d observations, want 2", p.count)
	}
	if err := st.CreateRun(t.Context(), model.Run{ID: "deferred", DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Status: "pending", Trigger: "schedule", QueuedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	delete(pending, "deferred")
	mu.Unlock()
	p.runStored("deferred")
	got, err := st.RunAlerts(t.Context(), "deferred")
	if err != nil || len(got) != 1 || got[0].Status != "sent" || got[0].Attempts != 1 {
		t.Fatalf("alerts = %+v, error %v", got, err)
	}
	if p.count != 0 || len(p.byRun) != 0 {
		t.Fatalf("park not emptied: %d %v", p.count, p.byRun)
	}
}

func TestAlertParkIsBounded(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := newAlertPark(st, func(string) bool { return true })
	updates := make([]store.AlertUpdate, maxParkedAlerts+10)
	for i := range updates {
		updates[i] = store.AlertUpdate{RunID: "r", Channel: "ops", Status: "queued"}
	}
	if err := p.record(t.Context(), updates); err != nil {
		t.Fatal(err)
	}
	if p.count != maxParkedAlerts {
		t.Fatalf("parked %d, limit %d", p.count, maxParkedAlerts)
	}
}
