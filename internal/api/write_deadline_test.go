package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newRealServer serves the API through net/http with the production write
// timeout behavior, which a response recorder cannot exercise.
func newRealServer(t *testing.T, s *Server, writeTimeout time.Duration) *httptest.Server {
	t.Helper()
	previous := responseWriteTimeout
	responseWriteTimeout = writeTimeout
	t.Cleanup(func() { responseWriteTimeout = previous })
	server := httptest.NewUnstartedServer(s.middleware(s.routes(), true))
	server.Config.WriteTimeout = responseWriteTimeout
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// A waited trigger for a run longer than the server write timeout must still
// deliver its terminal response.
func TestWaitedTriggerOutlivesWriteTimeout(t *testing.T) {
	s, _, _ := setup(t)
	mustCreate(t, s, "slowish", "sleep 1.5")
	server := newRealServer(t, s, 500*time.Millisecond)
	resp, err := http.Post(server.URL+"/api/v1/jobs/slowish/trigger?wait=true&timeout=10", "application/json", nil)
	if err != nil {
		t.Fatalf("waited trigger lost its response: %v", err)
	}
	defer resp.Body.Close()
	var run struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || run.Status != "succeeded" {
		t.Fatalf("waited trigger = %d/%s", resp.StatusCode, run.Status)
	}
}

// A raw download renews its write deadline while the client keeps reading.
func TestRawDownloadOutlivesWriteTimeout(t *testing.T) {
	s, _, _ := setup(t)
	// About 20 MiB, more than loopback socket buffers can absorb at once.
	mustCreate(t, s, "chatty-raw", `line=$(head -c 200000 /dev/zero | tr '\0' x); for i in $(seq 100); do echo "$line"; done; echo end`)
	server := newRealServer(t, s, 200*time.Millisecond)
	resp, err := http.Post(server.URL+"/api/v1/jobs/chatty-raw/trigger?wait=true&timeout=20", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		ID string `json:"run_id"`
	}
	err = json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	if err != nil || run.ID == "" {
		t.Fatalf("trigger: %v %+v", err, run)
	}
	resp, err = http.Get(server.URL + "/api/v1/runs/" + run.ID + "/log/raw")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body strings.Builder
	buf := make([]byte, 64<<10)
	for {
		// A slow reader stretches the download past the original deadline.
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("raw download interrupted after %d bytes: %v", body.Len(), err)
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.HasSuffix(body.String(), "\nend\n") {
		t.Fatalf("raw download incomplete: %d bytes: %.300q", body.Len(), body.String())
	}
}
