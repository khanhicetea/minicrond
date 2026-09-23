package alerts

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/model"
)

type captureChannel struct {
	messages chan Alert
}

func (c *captureChannel) Send(_ context.Context, alert Alert) error {
	c.messages <- alert
	return nil
}

func TestBatchFlushesOnClose(t *testing.T) {
	t.Setenv("BOT_TOKEN", "test")
	d, err := New([]config.AlertChannel{{Name: "ops", Type: "telegram", BotToken: "env:BOT_TOKEN", ChatID: "123", BatchWindow: "1h"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	capture := &captureChannel{messages: make(chan Alert, 2)}
	d.mu.Lock()
	d.channels["ops"] = capture
	d.mu.Unlock()
	def := model.Definition{Alerts: []string{"ops"}}
	d.Notify(model.Run{ID: "one", Status: "failed"}, def)
	d.Notify(model.Run{ID: "two", Status: "timeout"}, def)
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-capture.messages:
		if len(got.Runs) != 2 || got.Runs[0].ID != "one" || got.Runs[1].ID != "two" {
			t.Fatalf("batch = %#v", got)
		}
	default:
		t.Fatal("batch was not sent")
	}
	if len(capture.messages) != 0 {
		t.Fatal("expected one message")
	}
}

func TestBatchWindowSendsWithoutClose(t *testing.T) {
	t.Setenv("BOT_TOKEN", "test")
	d, err := New([]config.AlertChannel{{Name: "ops", Type: "telegram", BotToken: "env:BOT_TOKEN", ChatID: "123", BatchWindow: "1s"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close(t.Context())
	capture := &captureChannel{messages: make(chan Alert, 2)}
	d.mu.Lock()
	d.channels["ops"] = capture
	d.mu.Unlock()
	d.Notify(model.Run{ID: "one", Status: "failed"}, model.Definition{Alerts: []string{"ops"}})
	select {
	case got := <-capture.messages:
		if len(got.Runs) != 1 || got.Runs[0].ID != "one" {
			t.Fatalf("batch = %#v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("batch window did not flush")
	}
}

func TestFormatBatch(t *testing.T) {
	text := formatAlert(Alert{Runs: []model.Run{{ID: "one", Job: "backup", Status: "failed"}, {ID: "two", Job: "worker", Status: "timeout"}}})
	for _, want := range []string{"minicrond alerts", "Run: one", "Run: two", "Status: timeout"} {
		if !strings.Contains(text, want) {
			t.Errorf("batch %q missing %q", text, want)
		}
	}
}

func TestTelegramSend(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bottest-token/sendMessage" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		value, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = string(value)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	code := 7
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ended := started.Add(1500 * time.Millisecond)
	channel := newTelegram("test-token", "-100123", true, server.URL, server.Client())
	err := channel.Send(t.Context(), Alert{Run: model.Run{
		ID: "run-1", Job: "backup", Status: "failed", EndReason: "exit_code",
		ExitCode: &code, StartedAt: &started, EndedAt: &ended,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"chat_id=-100123", "disable_notification=true", "Definition%3A+backup", "Exit+code%3A+7", "Duration%3A+1.5s"} {
		if !strings.Contains(body, want) {
			t.Errorf("request body %q does not contain %q", body, want)
		}
	}
}

func TestTelegramDoesNotExposeTokenOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	channel := newTelegram("secret-token", "123", false, server.URL, server.Client())
	assertSendErrorHidesToken(t, channel)

	server.Close()
	assertSendErrorHidesToken(t, channel)
}

func assertSendErrorHidesToken(t *testing.T, channel *telegram) {
	t.Helper()
	err := channel.Send(t.Context(), Alert{Run: model.Run{ID: "run-1", Job: "job", Status: "failed"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error exposed token: %v", err)
	}
}
