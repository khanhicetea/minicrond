package alerts

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

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
