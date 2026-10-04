package alerts

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
)

type panicChannel struct{ calls atomic.Int64 }

func (c *panicChannel) Send(context.Context, Alert) error {
	c.calls.Add(1)
	panic("transport failed")
}

type sliceChannel []chan Alert

func (c sliceChannel) Send(_ context.Context, alert Alert) error {
	c[0] <- alert
	return nil
}

type nestedChannel struct{ value any }

func (nestedChannel) Send(context.Context, Alert) error { return nil }

func TestChannelIdentityHandlesNestedNonComparableValues(t *testing.T) {
	channel := nestedChannel{value: []string{"credentials"}}
	if sameChannel(channel, channel) {
		t.Fatal("non-comparable channels must be kept separate")
	}
}

func testDispatcher(t *testing.T, channel Channel, record func(context.Context, string, string, string, int, string) error) *Dispatcher {
	t.Helper()
	t.Setenv("BOT_TOKEN", "test")
	var recordAll RecordFunc
	if record != nil {
		recordAll = func(ctx context.Context, records []Record) error {
			for _, r := range records {
				if err := record(ctx, r.RunID, r.Channel, r.Status, r.Attempts, r.Reason); err != nil {
					return err
				}
			}
			return nil
		}
	}
	d, err := New([]config.AlertChannel{{Name: "ops", Type: "telegram", BotToken: "env:BOT_TOKEN", ChatID: "123", BatchWindow: 3600}}, recordAll)
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.channels["ops"] = channel
	d.mu.Unlock()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return d
}

func TestTransportPanicMarksDeliveryFailedWithoutRetry(t *testing.T) {
	channel := &panicChannel{}
	var failed atomic.Bool
	d := testDispatcher(t, channel, func(_ context.Context, _, _, status string, attempts int, _ string) error {
		if status == "failed" && attempts == 1 {
			failed.Store(true)
		}
		return nil
	})
	d.Notify(model.Run{ID: "one", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !failed.Load() || channel.calls.Load() != 1 || d.QueueDepth() != 0 {
		t.Fatalf("failed = %v, sends = %d, depth = %d", failed.Load(), channel.calls.Load(), d.QueueDepth())
	}
}

func TestTestTransportPanicReturnsError(t *testing.T) {
	d := testDispatcher(t, &panicChannel{}, nil)
	err := d.Test(t.Context(), "ops")
	var panicErr *fault.PanicError
	if !errors.As(err, &panicErr) || len(panicErr.Stack) == 0 {
		t.Fatalf("test alert error = %v, want panic with stack", err)
	}
}

func TestRecordPanicDoesNotStopDelivery(t *testing.T) {
	channel := &captureChannel{messages: make(chan Alert, 1)}
	d := testDispatcher(t, channel, func(context.Context, string, string, string, int, string) error {
		panic("record failed")
	})
	d.Notify(model.Run{ID: "one", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(channel.messages) != 1 || d.QueueDepth() != 0 {
		t.Fatalf("messages = %d, depth = %d", len(channel.messages), d.QueueDepth())
	}
}

func TestNonComparableChannelsDeliverWithoutPanic(t *testing.T) {
	messages := make(chan Alert, 2)
	d := testDispatcher(t, sliceChannel{messages}, nil)
	for _, id := range []string{"one", "two"} {
		d.Notify(model.Run{ID: id, Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || d.QueueDepth() != 0 {
		t.Fatalf("messages = %d, depth = %d", len(messages), d.QueueDepth())
	}
}

func TestQueuedAlertOwnsRunSnapshot(t *testing.T) {
	channel := &captureChannel{messages: make(chan Alert, 1)}
	d := testDispatcher(t, channel, nil)
	code := 7
	d.Notify(model.Run{ID: "one", Status: "failed", ExitCode: &code}, model.Definition{Alerts: []string{"ops"}})
	code = 9
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	alert := <-channel.messages
	if len(alert.Runs) != 1 || alert.Runs[0].ExitCode == nil || *alert.Runs[0].ExitCode != 7 {
		t.Fatalf("queued run snapshot = %+v", alert.Runs)
	}
}

func TestCloseCancellationCancelsRecordCallback(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	channel := &captureChannel{messages: make(chan Alert, 1)}
	d := testDispatcher(t, channel, func(ctx context.Context, _, _, status string, _ int, _ string) error {
		if status == "sending" {
			close(started)
			<-ctx.Done()
			close(finished)
			return ctx.Err()
		}
		return nil
	})
	d.Notify(model.Run{ID: "one", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	ctx, cancel := context.WithCancel(t.Context())
	closed := make(chan error, 1)
	go func() { closed <- d.Close(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("record callback did not start")
	}
	cancel()
	if err := <-closed; !errors.Is(err, context.Canceled) {
		t.Fatalf("close error = %v, want cancellation", err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("record callback continued after dispatcher cancellation")
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.QueueDepth() != 0 {
		t.Fatalf("depth = %d after shutdown", d.QueueDepth())
	}
	if len(channel.messages) != 0 {
		t.Fatal("delivery began after shutdown cancellation")
	}
}

func TestCloseCancellationUnblocksQueuedRecord(t *testing.T) {
	started := make(chan struct{})
	channel := &captureChannel{messages: make(chan Alert, 1)}
	d := testDispatcher(t, channel, func(ctx context.Context, _, _, status string, _ int, _ string) error {
		if status == "queued" {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	notified := make(chan struct{})
	go func() {
		d.Notify(model.Run{ID: "one", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
		close(notified)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued record callback did not start")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- d.Close(ctx) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close remained blocked on the Notify lock after cancellation")
	}
	<-notified
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.QueueDepth() != 0 || len(channel.messages) != 0 {
		t.Fatalf("depth = %d, messages = %d after cancellation", d.QueueDepth(), len(channel.messages))
	}
}

// Notify runs on the run-completion path: it must not wait for the database.
// A burst of failures is recorded as queued in one call.
func TestNotifyDoesNotWaitForQueuedRecord(t *testing.T) {
	release := make(chan struct{})
	groups := make(chan int, 16)
	channel := &captureChannel{messages: make(chan Alert, 1)}
	t.Setenv("BOT_TOKEN", "test")
	d, err := New([]config.AlertChannel{{Name: "ops", Type: "telegram", BotToken: "env:BOT_TOKEN", ChatID: "123", BatchWindow: 3600}}, func(ctx context.Context, records []Record) error {
		if records[0].Status == "queued" {
			groups <- len(records)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.channels["ops"] = channel
	d.mu.Unlock()
	d.Notify(model.Run{ID: "first", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	<-groups // the batch goroutine is now blocked recording "first"
	notified := make(chan struct{})
	go func() {
		for i := range 10 {
			d.Notify(model.Run{ID: fmt.Sprint(i), Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
		}
		close(notified)
	}()
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("Notify waited for the queued record")
	}
	close(release)
	if got := <-groups; got != 10 {
		t.Fatalf("burst recorded in a group of %d, want 10", got)
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if alert := <-channel.messages; len(alert.Runs) != 11 {
		t.Fatalf("delivered %d runs, want 11", len(alert.Runs))
	}
}
