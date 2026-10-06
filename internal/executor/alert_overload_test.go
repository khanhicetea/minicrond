package executor

import (
	"context"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/alerts"
	"github.com/khanhicetea/minicrond/internal/model"
)

// A16: a stalled alert recording callback must not keep a finished run's
// capacity slot occupied. The stored channel is absent from the dispatcher's
// live registry (as with an in-flight definition after deletion), which used
// to write the drop observation synchronously.
func TestAlertDropRecordingDoesNotHoldCapacity(t *testing.T) {
	release := make(chan struct{})
	dispatcher, err := alerts.New(nil, func(ctx context.Context, _ []alerts.Record) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dispatcher.Close(ctx)
	})
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, OnFinished: dispatcher.Notify})
	if err := st.PutAlertChannel(t.Context(), model.AlertChannel{Name: "missing", Type: "telegram", BotToken: "123:test", ChatID: "123"}, "test"); err != nil {
		t.Fatal(err)
	}
	d, hash := putJob(t, st, model.Definition{Name: "alerting", Kind: model.KindJob, Command: "exit 1", Shell: "/bin/sh", OnOverlap: "parallel", SuccessCodes: []int{0}, Alerts: []string{"missing"}})
	for i := range 3 {
		run, err := s.Trigger(t.Context(), d, hash, "manual", nil)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == "skipped" {
			t.Fatalf("run %d was skipped: capacity still held by the previous run (%s)", i, run.EndReason)
		}
		done := s.Wait(run.ID)
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("run %d completion waited for alert drop recording", i)
		}
	}
}
