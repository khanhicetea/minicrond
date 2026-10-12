package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestExhaustionEventUsesBoundedBatchPipeline(t *testing.T) {
	d, err := New([]model.AlertChannel{{Name: "ops", Type: "telegram", BotToken: "123:test", ChatID: "123", BatchWindow: 3600}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	capture := &captureChannel{messages: make(chan Alert, 1)}
	d.mu.Lock()
	d.channels["ops"] = capture
	d.mu.Unlock()
	def := model.Definition{Name: "queue", Alerts: []string{"ops"}}
	for _, id := range []string{"last-run", ""} {
		d.NotifyExhausted(def, model.WorkerExhaustion{Reason: "restart attempts exhausted after 5 failures", LastRunID: id, At: time.Now().UTC()})
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case alert := <-capture.messages:
		if len(alert.Runs) != 2 || alert.Runs[0].Status != "exhausted" || !strings.Contains(formatAlert(alert), "restart attempts exhausted") {
			t.Fatalf("alert: %+v", alert)
		}
		if got := deliveryRecordName("ops", alert.Runs[0]); got != "ops~worker_exhausted" {
			t.Fatal(got)
		}
	default:
		t.Fatal("no exhaustion alert")
	}
}
