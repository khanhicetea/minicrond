package alerts

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/model"
)

// A16: with the alert queue full and the recording callback stalled, Notify
// (the run-completion path) and registry replacement must stay prompt. Drop
// observations are bounded and recorded once the callback recovers.
func TestNotifyOverloadDoesNotWaitForDropRecording(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	dropped := 0
	seen := map[string]bool{}
	t.Setenv("BOT_TOKEN", "test")
	channels := []config.AlertChannel{{Name: "ops", Type: "telegram", BotToken: "env:BOT_TOKEN", ChatID: "123", BatchWindow: 3600}}
	d, err := New(channels, func(ctx context.Context, records []Record) error {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		mu.Lock()
		defer mu.Unlock()
		for _, r := range records {
			if r.Status == "dropped" {
				dropped++
				seen[r.RunID] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	channel := &captureChannel{messages: make(chan Alert, 1)}
	d.mu.Lock()
	d.channels["ops"] = channel
	d.mu.Unlock()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Close(ctx)
	})
	def := model.Definition{Alerts: []string{"ops", "missing"}}
	d.Notify(model.Run{ID: "first", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	select {
	case <-started: // the batch goroutine is now stuck in the record callback
	case <-time.After(2 * time.Second):
		t.Fatal("record callback never started")
	}

	const failures = 600
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := range failures {
			d.Notify(model.Run{ID: fmt.Sprint("run-", i), Status: "failed"}, def)
		}
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on drop recording while the queue was full")
	}
	applied := make(chan error, 1)
	go func() {
		registry, err := Prepare(channels)
		if err == nil {
			err = d.Apply(registry)
		}
		applied <- err
	}()
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("registry replacement waited behind Notify")
	}
	d.dropMu.Lock()
	pendingDrops, overflow := len(d.drops), d.dropOverflow
	d.dropMu.Unlock()
	if pendingDrops != maxDropObservations || overflow == 0 {
		t.Fatalf("pending drop observations = %d, overflow = %d; want bounded at %d with overflow counted", pendingDrops, overflow, maxDropObservations)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := dropped
		newest := seen[fmt.Sprint("run-", failures-1)]
		mu.Unlock()
		if got >= maxDropObservations {
			if got != maxDropObservations {
				t.Fatalf("recorded %d drop observations, want at most %d", got, maxDropObservations)
			}
			if !newest {
				t.Fatal("newest drop was not kept: observations must be newest-wins")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d drop observations after recovery", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
