package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// waitFor polls cond for up to 5 s.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestCLIReusesSmallUnixConnectionSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINICRON_DATA", dir)
	t.Setenv("MINICRON_URL", "")
	t.Cleanup(closeClient)
	listener, err := net.Listen("unix", filepath.Join(dir, "minicron.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var accepted, active atomic.Int64
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{\"version\":\"test\"}\n"))
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				accepted.Add(1)
				active.Add(1)
			case http.StateClosed, http.StateHijacked:
				active.Add(-1)
			}
		},
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	runtime.GC()
	baseGoroutines, baseFDs := runtime.NumGoroutine(), openFDs()

	const requests = 1000
	for i := 0; i < requests; i++ {
		var status struct {
			Version string `json:"version"`
		}
		if err := requestJSON("GET", "/api/v1/daemon", nil, &status); err != nil || status.Version != "test" {
			t.Fatalf("request %d: version=%q err=%v", i, status.Version, err)
		}
	}
	if got := accepted.Load(); got > 2 {
		t.Fatalf("%d requests opened %d connections, want a small bounded set", requests, got)
	}

	closeClient()
	if !waitFor(func() bool { return active.Load() == 0 }) {
		t.Fatalf("%d connections still open after closeClient", active.Load())
	}
	// Server-side handler goroutines exit asynchronously after the close.
	if !waitFor(func() bool { runtime.GC(); return runtime.NumGoroutine() <= baseGoroutines+2 }) {
		t.Fatalf("goroutines %d, baseline %d", runtime.NumGoroutine(), baseGoroutines)
	}
	if baseFDs >= 0 {
		if !waitFor(func() bool { return openFDs() <= baseFDs+2 }) {
			t.Fatalf("fds %d, baseline %d", openFDs(), baseFDs)
		}
	}
}

func TestClientIsSharedPerTargetAndResetOnClose(t *testing.T) {
	t.Setenv("MINICRON_URL", "")
	t.Setenv("MINICRON_DATA", t.TempDir())
	t.Cleanup(closeClient)
	a := client()
	if client() != a {
		t.Fatal("client() must return the shared client for the same target")
	}
	t.Setenv("MINICRON_DATA", t.TempDir())
	if client() == a {
		t.Fatal("client() must not reuse a client for a different target")
	}
	closeClient()
	if cliHTTP.client != nil {
		t.Fatal("closeClient must drop the shared client")
	}
}

func logServer(t *testing.T, requests *atomic.Int64) {
	t.Helper()
	server := newLogServer(requests)
	t.Cleanup(server.Close)
	t.Setenv("MINICRON_URL", server.URL)
	t.Cleanup(closeClient)
}

func TestFollowLogsPollsAtBoundedCadence(t *testing.T) {
	var requests atomic.Int64
	logServer(t, &requests)
	stop := errors.New("stop")
	var waits []time.Duration
	err := followLogs("run", true, func(d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("followLogs: %v", err)
	}
	for _, d := range waits {
		if d < time.Second || d > 3*time.Second {
			t.Fatalf("poll interval %v outside 1-3 s", d)
		}
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("expected one request per poll (3), got %d", got)
	}
}

func TestLogsWithoutFollowDoesNotPoll(t *testing.T) {
	var requests atomic.Int64
	logServer(t, &requests)
	err := followLogs("run", false, func(time.Duration) error {
		t.Error("polled without --follow")
		return errors.New("polled")
	})
	if err != nil || requests.Load() != 1 {
		t.Fatalf("err=%v requests=%d", err, requests.Load())
	}
}

func newLogServer(requests *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("{\"items\":[]}\n"))
	}))
}

// busyLogServer refuses the first `refusals` requests with the daemon's
// read-admission 503 and then serves one page followed by empty pages.
func busyLogServer(t *testing.T, refusals int, retryAfter string, requests *atomic.Int64, afters *[]string) {
	t.Helper()
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(requests.Add(1))
		mu.Lock()
		*afters = append(*afters, r.URL.Query().Get("after"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n <= refusals {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"read_busy","message":"too many expensive reads in progress; retry shortly"}}` + "\n"))
			return
		}
		if r.URL.Query().Get("after") == "0" {
			_, _ = w.Write([]byte(`{"items":[{"sequence":1,"stream":1,"payload":"aGk="}]}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}` + "\n"))
	}))
	t.Cleanup(server.Close)
	t.Setenv("MINICRON_URL", server.URL)
	t.Cleanup(closeClient)
}

func TestFollowLogsRetriesReadBusyAtSameCursor(t *testing.T) {
	var requests atomic.Int64
	var afters []string
	busyLogServer(t, 2, "3", &requests, &afters)
	var waits []time.Duration
	err := followLogs("run", false, func(d time.Duration) error {
		waits = append(waits, d)
		return nil
	})
	if err != nil {
		t.Fatalf("followLogs: %v", err)
	}
	if len(waits) != 2 || waits[0] != 3*time.Second || waits[1] != 3*time.Second {
		t.Fatalf("waits = %v, want two Retry-After pauses of 3s", waits)
	}
	// Two refusals and the page at after=0, then the empty page after it.
	if got := requests.Load(); got != 4 || afters[0] != "0" || afters[1] != "0" || afters[2] != "0" || afters[3] != "1" {
		t.Fatalf("requests=%d afters=%v", got, afters)
	}
}

func TestFollowLogsClampsRetryAfter(t *testing.T) {
	for header, want := range map[string]time.Duration{"": time.Second, "0": time.Second, "junk": time.Second, "3600": maxBusyRetry} {
		var requests atomic.Int64
		var afters []string
		busyLogServer(t, 1, header, &requests, &afters)
		var waits []time.Duration
		if err := followLogs("run", false, func(d time.Duration) error { waits = append(waits, d); return nil }); err != nil {
			t.Fatalf("Retry-After %q: %v", header, err)
		}
		if len(waits) != 1 || waits[0] != want {
			t.Errorf("Retry-After %q: waited %v, want %v", header, waits, want)
		}
		closeClient()
	}
}

func TestFollowLogsGivesUpAfterBoundedBusyAttemptsWithoutFollow(t *testing.T) {
	var requests atomic.Int64
	var afters []string
	busyLogServer(t, 1000, "1", &requests, &afters)
	waits := 0
	err := followLogs("run", false, func(time.Duration) error { waits++; return nil })
	var busy *busyError
	if !errors.As(err, &busy) || !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "read_busy") {
		t.Fatalf("followLogs = %v, want the 503 error", err)
	}
	if waits != maxBusyAttempts || requests.Load() != maxBusyAttempts+1 {
		t.Fatalf("%d waits, %d requests; want %d and %d", waits, requests.Load(), maxBusyAttempts, maxBusyAttempts+1)
	}
}

func TestFollowLogsKeepsRetryingBusyWhileFollowing(t *testing.T) {
	var requests atomic.Int64
	var afters []string
	busyLogServer(t, 2*maxBusyAttempts, "1", &requests, &afters)
	stop := errors.New("stop")
	waits := 0
	err := followLogs("run", true, func(time.Duration) error {
		// After the refusals, the page and one empty poll, stop following.
		if waits++; waits == 2*maxBusyAttempts+1 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || requests.Load() != int64(2*maxBusyAttempts)+2 {
		t.Fatalf("err=%v requests=%d", err, requests.Load())
	}
}

func TestFollowLogsDoesNotRetryOtherErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound, http.StatusServiceUnavailable} {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			// A 503 for another reason (not_ready, stream_capacity) is not read admission.
			_, _ = w.Write([]byte(`{"error":{"code":"not_ready","message":"daemon is shutting down"}}`))
		}))
		t.Setenv("MINICRON_URL", server.URL)
		err := followLogs("run", true, func(time.Duration) error { t.Error("waited"); return errors.New("waited") })
		closeClient()
		server.Close()
		if err == nil || requests.Load() != 1 {
			t.Errorf("status %d: err=%v requests=%d", status, err, requests.Load())
		}
	}
}
