package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// S6: `minicrond run NAME --wait` keeps polling a queued run until it is
// terminal and prints the final run, not the interim 202 body.
func TestRunWaitPollsQueuedRunToTerminal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINICRON_DATA", dir)
	t.Setenv("MINICRON_URL", "")
	t.Cleanup(closeClient)
	listener, err := net.Listen("unix", filepath.Join(dir, "minicron.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int64
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/trigger") && r.URL.Query().Get("wait") == "true":
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"run_id":"r1","status":"queued"}`))
		case r.Method == "GET" && r.URL.Path == "/api/v1/runs/r1":
			status := "queued"
			switch n := gets.Add(1); {
			case n == 2:
				status = "running"
			case n >= 3:
				status = "succeeded"
			}
			_, _ = w.Write([]byte(`{"run_id":"r1","status":"` + status + `"}`))
		default:
			w.WriteHeader(404)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	var sleeps atomic.Int64
	old := triggerWaitSleep
	triggerWaitSleep = func(time.Duration) error { sleeps.Add(1); return nil }
	t.Cleanup(func() { triggerWaitSleep = old })

	r, w, _ := os.Pipe()
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	err = trigger([]string{"job", "--wait"})
	os.Stdout, os.Stderr = origStdout, origStderr
	w.Close()
	var out bytes.Buffer
	_, _ = io.Copy(&out, r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"status": "succeeded"`) || sleeps.Load() < 2 {
		t.Fatalf("sleeps=%d output:\n%s", sleeps.Load(), out.String())
	}
	if !strings.Contains(out.String(), "is queued; waiting") {
		t.Fatalf("missing waiting message:\n%s", out.String())
	}
}
