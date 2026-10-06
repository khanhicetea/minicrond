package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestPrepareUsesLiteralCredentials(t *testing.T) {
	c := model.AlertChannel{Name: "ops", Type: "telegram", BotToken: "123:stored_token", ChatID: "-123"}
	registry, err := Prepare([]model.AlertChannel{c})
	if err != nil {
		t.Fatal(err)
	}
	channel := registry.channels["ops"].(*telegram)
	if channel.token != c.BotToken || registry.windows["ops"] != 10*time.Second {
		t.Fatal("registry did not use stored credentials/default window")
	}
	for _, token := range []string{"env:PRIVATE_TOKEN", "file:/tmp/PRIVATE_TOKEN"} {
		c.BotToken = token
		if _, err := Prepare([]model.AlertChannel{c}); err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("reference should fail safely: %v", err)
		}
	}
}

func TestDeleteRegistryKeepsQueuedDeliveries(t *testing.T) {
	d, err := New([]model.AlertChannel{{Name: "ops", Type: "telegram", BotToken: "123:token", ChatID: "123", BatchWindow: 3600}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := &captureChannel{messages: make(chan Alert, 2)}
	if err := d.Apply(&Registry{channels: map[string]Channel{"ops": old}, windows: map[string]time.Duration{"ops": time.Hour}}); err != nil {
		t.Fatal(err)
	}
	d.Notify(model.Run{ID: "before-delete", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	registry, err := Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Apply(registry); err != nil {
		t.Fatal(err)
	}
	d.Notify(model.Run{ID: "after-delete", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-old.messages:
		if len(got.Runs) != 1 || got.Runs[0].ID != "before-delete" {
			t.Fatalf("queued delivery = %+v", got)
		}
	default:
		t.Fatal("delete cancelled queued alert")
	}
	if len(old.messages) != 0 || d.QueueDepth() != 0 {
		t.Fatal("new delivery used deleted channel")
	}
}
