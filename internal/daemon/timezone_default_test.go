package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Definitions saved or imported through the API without a timezone take
// scheduler.timezone (UTC only when nothing is configured), follow a reload,
// and never override a timezone the definition names itself.
func TestAPIDefinitionsDefaultToSchedulerTimezone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicrond.toml")
	write := func(timezone string) {
		t.Helper()
		content := "[server]\ntcp_enabled=false\n"
		if timezone != "" {
			content += "[scheduler]\ntimezone='" + timezone + "'\n"
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("Asia/Ho_Chi_Minh")
	ctx, cancel := context.WithCancel(t.Context())
	d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		running := d.running
		d.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	socket := filepath.Join(d.DataDir, "minicrond.sock")
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request := func(method, route string, body any) map[string]any {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reader = bytes.NewReader(b)
		} else {
			reader = bytes.NewReader(nil)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, "http://minicrond"+route, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d %v", method, route, resp.StatusCode, out)
		}
		return out
	}
	timezoneOf := func(name string) string {
		t.Helper()
		return request("GET", "/api/v1/jobs/"+name, nil)["definition"].(map[string]any)["timezone"].(string)
	}
	importJob := func(name string) {
		t.Helper()
		content := "[[job]]\nname='" + name + "'\ncommand='true'\nschedule='@every 1h'\n"
		preview := request("POST", "/api/v1/import/preview", map[string]any{"content": content})
		request("POST", "/api/v1/import/apply", map[string]any{"content": content, "hash": preview["content_hash"]})
	}
	save := func(name string, extra map[string]any) {
		t.Helper()
		body := map[string]any{"name": name, "command": "true", "schedule": "@every 1h"}
		for k, v := range extra {
			body[k] = v
		}
		request("PUT", "/api/v1/jobs/"+name, body)
	}

	if got := request("GET", "/api/v1/daemon", nil)["timezone"]; got != "Asia/Ho_Chi_Minh" {
		t.Fatalf("daemon timezone = %v", got)
	}
	save("saved", nil)
	save("own", map[string]any{"timezone": "Europe/Berlin"})
	importJob("imported")
	for name, want := range map[string]string{"saved": "Asia/Ho_Chi_Minh", "own": "Europe/Berlin", "imported": "Asia/Ho_Chi_Minh"} {
		if got := timezoneOf(name); got != want {
			t.Errorf("%s timezone = %q, want %q", name, got, want)
		}
	}

	// A reload that changes scheduler.timezone applies to later definitions
	// (and to the value the UI prefills); existing ones keep theirs.
	write("")
	if err := d.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := request("GET", "/api/v1/daemon", nil)["timezone"]; got != "UTC" {
		t.Fatalf("daemon timezone after reload = %v", got)
	}
	save("after-reload", nil)
	if got := timezoneOf("after-reload"); got != "UTC" {
		t.Errorf("after-reload timezone = %q, want UTC", got)
	}
	if got := timezoneOf("saved"); got != "Asia/Ho_Chi_Minh" {
		t.Errorf("existing definition changed to %q", got)
	}
}
