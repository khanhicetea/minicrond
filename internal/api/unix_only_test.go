package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/store"
)

func TestUnixOnlyListenerAndTokenTransition(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(st, nil, nil, nil, nil, nil, "test")
	s.SetTCPEnabled(false)
	if token, err := s.InitializeToken(t.Context()); err != nil || token != "" {
		t.Fatalf("Unix-only token initialization = %q, %v", token, err)
	}
	if _, err := st.Meta(t.Context(), "token_hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Unix-only mode created token hash: %v", err)
	}
	if rec := call(s, true, "POST", "/api/v1/token/rotate", "", "", nil); rec.Code != 404 {
		t.Fatalf("Unix-only token rotation = %d", rec.Code)
	}
	status := decode(t, call(s, true, "GET", "/api/v1/daemon", "", "", nil))
	if status["tcp_enabled"] != false || status["token_fingerprint"] != nil {
		t.Fatalf("Unix-only status exposed token settings: %v", status)
	}

	// Reserve then release a TCP port. Start must not bind it in Unix-only mode.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bind := probe.Addr().String()
	_ = probe.Close()
	socket := filepath.Join(dir, "minicron.sock")
	if err := s.Start(bind, socket); err != nil {
		t.Fatal(err)
	}
	s.SetReady(true)
	defer s.Shutdown(context.Background())
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", info, err)
	}
	if conn, err := net.DialTimeout("tcp", bind, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("Unix-only daemon unexpectedly opened TCP")
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}, Timeout: time.Second}
	for _, path := range []string{"/", "/openapi.json", "/api/v1/daemon"} {
		resp, err := client.Get("http://minicron" + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket path remained after shutdown: %v", err)
	}

	// Re-enabling TCP issues a token once. A later Unix-only interval retains
	// its hash, so re-enabling again does not silently replace that token.
	s.SetTCPEnabled(true)
	token, err := s.InitializeToken(t.Context())
	if err != nil || token == "" {
		t.Fatalf("enabling TCP did not issue token: %q, %v", token, err)
	}
	s.SetTCPEnabled(false)
	if next, err := s.InitializeToken(t.Context()); err != nil || next != "" {
		t.Fatalf("Unix-only interval changed token: %q, %v", next, err)
	}
	s.SetTCPEnabled(true)
	if next, err := s.InitializeToken(t.Context()); err != nil || next != "" {
		t.Fatalf("re-enabling TCP replaced token: %q, %v", next, err)
	}
	if rec := call(s, false, "GET", "/api/v1/daemon", token, "", nil); rec.Code != 200 {
		t.Fatalf("retained token rejected: %d", rec.Code)
	}
}

func TestUnixOnlySSEAndDownloadWithoutBearer(t *testing.T) {
	s, _, _ := setup(t)
	s.SetTCPEnabled(false)
	mustCreate(t, s, "streamed", "echo streamed")
	run := call(s, true, "POST", "/api/v1/jobs/streamed/trigger?wait=true&timeout=5", "", "", nil)
	if run.Code != 200 {
		t.Fatalf("trigger: %d %s", run.Code, run.Body.String())
	}
	runID := decode(t, run)["run_id"].(string)
	socket := filepath.Join(t.TempDir(), "minicron.sock")
	if err := s.Start("unused", socket); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}, Timeout: time.Second}
	for path, want := range map[string]string{
		"/api/v1/runs/" + runID + "/log/stream": "event: backlog_done",
		"/api/v1/runs/" + runID + "/log/raw":    "streamed",
	} {
		resp, err := client.Get("http://minicron" + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Fatalf("GET %s: status=%d, body=%q, err=%v", path, resp.StatusCode, body, err)
		}
	}
}

func TestMultipleUnixOnlyInstancesShareNoTCPPort(t *testing.T) {
	for range 2 {
		s := New(nil, nil, nil, nil, nil, nil, "test")
		s.SetTCPEnabled(false)
		if err := s.Start("127.0.0.1:7423", filepath.Join(t.TempDir(), "minicron.sock")); err != nil {
			t.Fatalf("Unix-only instance startup: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	}
}
