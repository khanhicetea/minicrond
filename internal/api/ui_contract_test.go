package api

import (
	"bytes"
	"mime"
	"regexp"
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/logstore"
)

// Bento vendors these files, not Server.ui's response. Keep its literal HTML
// rewrites working; changes require a release note (docs/embedding-ui.md).
func TestVendoredUIContract(t *testing.T) {
	page, err := webAssets.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(page, []byte(`<base href="/" />`)) != 1 || len(regexp.MustCompile(`(?i)<base\b`).FindAll(page, -1)) != 1 {
		t.Fatal(`index.html must contain exactly one <base href="/" />`)
	}
	for _, entry := range []struct {
		tag, pattern string
	}{
		{"script", `^<script\b[^>]*\bsrc="\./(app-[A-Za-z0-9_-]+\.js)"[^>]*>\s*</script>$`},
		{"link", `^<link\b[^>]*\brel="stylesheet"[^>]*\bhref="\./(style-[A-Za-z0-9_-]+\.css)"[^>]*>$`},
	} {
		tags := regexp.MustCompile(`(?i)<` + entry.tag + `\b[^>]*>`)
		if entry.tag == "script" {
			tags = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
		}
		matches := tags.FindAll(page, -1)
		if len(matches) != 1 {
			t.Fatalf("index.html has %d %s tags, want one", len(matches), entry.tag)
		}
		match := regexp.MustCompile(entry.pattern).FindSubmatch(matches[0])
		if match == nil {
			t.Fatalf("unexpected %s entry: %s", entry.tag, matches[0])
		}
		if _, err := webAssets.ReadFile("assets/" + string(match[1])); err != nil {
			t.Fatal(err)
		}
	}
	if regexp.MustCompile(`(?i)<style\b`).Match(page) {
		t.Fatal("index.html must use only the external style-*.css stylesheet")
	}
	for name, asset := range cachedAssets() {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		for _, path := range []string{"/healthz", "/readyz", "/openapi.json"} {
			if bytes.Contains(asset.body, []byte(path)) {
				t.Errorf("SPA bundle %s references non-API route %s", name, path)
			}
		}
	}
}

func TestGatewayAPIContentTypes(t *testing.T) {
	s, token, _ := setup(t)
	id, writer := liveRun(t, s, logstore.WriterOptions{})
	if err := writer.Write(logstore.Stdout, []byte("<html><script>alert(1)</script></html>"), 0); err != nil {
		t.Fatal(err)
	}
	// Seal the log so the SSE request returns without waiting for polling.
	if err := s.logs.Close(id); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"", "/tools/minicron"} {
		if err := s.SetBasePath(base); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			method, path, token, mediaType string
			status                         int
		}{
			{"GET", "/api/v1/daemon", token, "application/json", 200},
			{"GET", "/api/v1/daemon", "", "application/json", 401},
			{"GET", "/api/v1/missing", token, "application/json", 404},
			{"POST", "/api/v1/jobs", token, "application/json", 422},
			{"GET", "/api/v1/export", token, "application/toml", 200},
			{"GET", "/api/v1/export?format=json", token, "application/json", 200},
			{"GET", "/api/v1/runs/" + id + "/log/raw", token, "text/plain", 200},
			{"GET", "/api/v1/runs/" + id + "/log/stream", token, "text/event-stream", 200},
			{"GET", "/api/v1/jobs/../daemon", token, "text/plain", 307},
		} {
			t.Run(base+tc.method+tc.path, func(t *testing.T) {
				rec := call(s, false, tc.method, base+tc.path, tc.token, "", nil)
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
				}
				got, _, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
				if err != nil || got != tc.mediaType {
					t.Fatalf("Content-Type = %q (%v), want %s", rec.Header().Get("Content-Type"), err, tc.mediaType)
				}
				if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Fatal("API response must disable content sniffing")
				}
			})
		}
	}
}
