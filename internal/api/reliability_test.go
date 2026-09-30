package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerPanicReturnsGenericError(t *testing.T) {
	s := New(nil, nil, nil, nil, nil, nil, "test")
	handler := s.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("private database path")
	}), true)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private database path") {
		t.Fatalf("panic response = %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "internal_error") {
		t.Fatalf("missing error envelope: %s", response.Body.String())
	}
}

func TestHandlerPanicAbortsPartialResponse(t *testing.T) {
	s := New(nil, nil, nil, nil, nil, nil, "test")
	handler := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "partial"); err != nil {
			t.Fatal(err)
		}
		panic("handler failed")
	}), true)
	response := httptest.NewRecorder()
	defer func() {
		value := recover()
		err, ok := value.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("panic = %v, want ErrAbortHandler", value)
		}
		if response.Body.String() != "partial" {
			t.Fatalf("partial response changed to %q", response.Body.String())
		}
	}()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
}

func TestInternalErrorDoesNotExposeDetails(t *testing.T) {
	response := httptest.NewRecorder()
	internal(response, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil), errors.New("private database path"))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private database path") {
		t.Fatalf("internal response = %d %s", response.Code, response.Body.String())
	}
}

type failedListener struct{ err error }

func (l failedListener) Accept() (net.Conn, error) { return nil, l.err }
func (failedListener) Close() error                { return nil }
func (failedListener) Addr() net.Addr              { return &net.TCPAddr{} }

func TestListenerFailureSignalsDaemon(t *testing.T) {
	s := New(nil, nil, nil, nil, nil, nil, "test")
	s.SetReady(true)
	want := errors.New("listener failed")
	s.serve("tcp", &http.Server{}, failedListener{err: want})
	select {
	case err := <-s.Errors():
		if !errors.Is(err, want) {
			t.Fatalf("listener error = %v, want %v", err, want)
		}
	default:
		t.Fatal("listener failure was not reported")
	}
	if s.ready.Load() {
		t.Fatal("server remained ready after listener failure")
	}
}

func TestStorageFailuresAreInternalErrors(t *testing.T) {
	s, _, st := setup(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path, body string
		headers                  map[string]string
	}{
		{name: "trigger", method: "POST", path: "/api/v1/jobs/job/trigger"},
		{name: "idempotent_trigger", method: "POST", path: "/api/v1/jobs/job/trigger", headers: map[string]string{"Idempotency-Key": "key"}},
		{name: "delete", method: "DELETE", path: "/api/v1/jobs/job"},
		{name: "enable", method: "POST", path: "/api/v1/jobs/job/enable"},
		{name: "save", method: "POST", path: "/api/v1/jobs", body: `{"name":"job","command":"true"}`},
		{name: "worker_start", method: "POST", path: "/api/v1/workers/worker/start"},
		{name: "worker_stop", method: "POST", path: "/api/v1/workers/worker/stop"},
		{name: "worker_restart", method: "POST", path: "/api/v1/workers/worker/restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := call(s, true, tc.method, tc.path, "", tc.body, tc.headers)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("response = %d %s, want 500", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "database") {
				t.Fatalf("storage details exposed: %s", response.Body.String())
			}
		})
	}
}

func TestDisabledTriggerRemainsExpectedConflict(t *testing.T) {
	s, _, _ := setup(t)
	response := call(s, true, "POST", "/api/v1/jobs", "", `{"name":"disabled","command":"true","enabled":false}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("save disabled definition = %d %s", response.Code, response.Body.String())
	}
	response = call(s, true, "POST", "/api/v1/jobs/disabled/trigger", "", "", nil)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "trigger_rejected") {
		t.Fatalf("disabled trigger = %d %s", response.Code, response.Body.String())
	}
}

func TestBeginShutdownRejectsNewAPIRequests(t *testing.T) {
	s := New(nil, nil, nil, nil, nil, nil, "test")
	s.SetReady(true)
	s.BeginShutdown()
	if s.ready.Load() {
		t.Fatal("server remained ready after shutdown began")
	}
	for _, path := range []string{"/api/v1/jobs", "/openapi.json"} {
		response := call(s, true, http.MethodGet, path, "", "", nil)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s response = %d %s, want 503", path, response.Code, response.Body.String())
		}
	}
	response := call(s, true, http.MethodGet, "/healthz", "", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("health probe = %d, want 200", response.Code)
	}
}
