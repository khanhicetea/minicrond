package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minicron/minicron/internal/executor"
	"github.com/minicron/minicron/internal/logstore"
	"github.com/minicron/minicron/internal/model"
	"github.com/minicron/minicron/internal/store"
	"github.com/minicron/minicron/internal/supervisor"
)

func setup(t *testing.T) (*Server, string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	ex := executor.New(st, logs, executor.Options{MaxConcurrentRuns: 4})
	sup := supervisor.New(st, ex)
	srv := New(st, logs, ex, sup, func(context.Context) error { return nil }, "test")
	token, err := srv.InitializeToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected an initial token to be issued once")
	}
	return srv, token, st
}

func call(s *Server, local bool, method, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.middleware(s.routes(), local).ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

func previewBodyWithHash(content, hash string) string {
	b, _ := json.Marshal(importRequest{Content: content, Hash: hash})
	return string(b)
}

func mustCreate(t *testing.T, s *Server, name, command string) {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"kind":"job","command":%q,"shell":"/bin/sh"}`, name, command)
	if rec := call(s, true, "PUT", "/api/v1/jobs/"+name, "", body, nil); rec.Code != 200 {
		t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body.String())
	}
}

func TestJobsExposeSchedulerNextFire(t *testing.T) {
	s, _, st := setup(t)
	body := `{"name":"scheduled","kind":"job","command":"true","shell":"/bin/sh","schedule":"@every 1m","timezone":"UTC"}`
	if rec := call(s, true, "PUT", "/api/v1/jobs/scheduled", "", body, nil); rec.Code != 200 {
		t.Fatalf("create scheduled job: %d %s", rec.Code, rec.Body.String())
	}
	def, _, err := st.Definition(t.Context(), "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UTC()
	next := anchor.Add(time.Minute).Truncate(time.Microsecond)
	if err := st.SetScheduleStateWithNext(t.Context(), def.ID, "hash", anchor, anchor, next); err != nil {
		t.Fatal(err)
	}

	rec := call(s, true, "GET", "/api/v1/jobs", "", "", nil)
	if rec.Code != 200 {
		t.Fatalf("list jobs: %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Items []struct {
			Name       string     `json:"name"`
			NextFireAt *time.Time `json:"next_fire_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].Name != "scheduled" {
		t.Fatalf("unexpected jobs response: %+v", response.Items)
	}
	if response.Items[0].NextFireAt == nil || !response.Items[0].NextFireAt.Equal(next) {
		t.Fatalf("next fire = %v, want %s", response.Items[0].NextFireAt, next)
	}
}

func TestTCPRequiresBearerToken(t *testing.T) {
	s, token, _ := setup(t)
	rec := call(s, false, "GET", "/api/v1/daemon", "", "", nil)
	if rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	if code := decode(t, rec)["error"].(map[string]any)["code"]; code != "unauthorized" {
		t.Fatalf("code %v", code)
	}
	if rec := call(s, false, "GET", "/api/v1/daemon", "wrong", "", nil); rec.Code != 401 {
		t.Fatal("bad token accepted")
	}
	if rec := call(s, false, "GET", "/api/v1/daemon", token, "", nil); rec.Code != 200 {
		t.Fatal("valid token rejected")
	}
	if rec := call(s, false, "GET", "/healthz", "", "", nil); rec.Code != 200 {
		t.Fatal("healthz must stay unauthenticated")
	}
	if rec := call(s, true, "GET", "/api/v1/daemon", "", "", nil); rec.Code != 200 {
		t.Fatal("unix socket must use peer auth, not a token")
	}
}

func TestDaemonRunAsCapabilityReflectsPrivileges(t *testing.T) {
	s, token, _ := setup(t)
	rec := call(s, false, "GET", "/api/v1/daemon", token, "", nil)
	if rec.Code != 200 {
		t.Fatalf("daemon status: %d %s", rec.Code, rec.Body.String())
	}
	capabilities, ok := decode(t, rec)["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities = %T, want array", decode(t, rec)["capabilities"])
	}
	hasRunAs := false
	for _, capability := range capabilities {
		hasRunAs = hasRunAs || capability == "run-as"
	}
	if hasRunAs != (os.Geteuid() == 0) {
		t.Fatalf("run-as capability = %v, euid = %d", hasRunAs, os.Geteuid())
	}
}

func TestSecurityHeadersAndURLCap(t *testing.T) {
	s, token, _ := setup(t)
	rec := call(s, false, "GET", "/api/v1/daemon", token, "", nil)
	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "default-src 'self'; script-src 'self'",
	} {
		if got := rec.Header().Get(header); !strings.HasPrefix(got, want) {
			t.Fatalf("%s = %q", header, got)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data:") {
		t.Fatalf("CSP must allow data: images, got %q", csp)
	}
	if allow := rec.Header().Get("Access-Control-Allow-Origin"); allow != "" {
		t.Fatalf("CORS must be disabled by default, got %q", allow)
	}
	if rec := call(s, false, "GET", "/api/v1/runs?pad="+strings.Repeat("x", 3000), token, "", nil); rec.Code != 414 {
		t.Fatalf("oversized URL: %d", rec.Code)
	}
}

func TestTokenRotationLocalOnlyAndInvalidatesOld(t *testing.T) {
	s, token, _ := setup(t)
	if rec := call(s, false, "POST", "/api/v1/token/rotate", token, "", nil); rec.Code != 403 {
		t.Fatalf("remote rotation: %d", rec.Code)
	}
	rec := call(s, true, "POST", "/api/v1/token/rotate", "", "", nil)
	if rec.Code != 200 {
		t.Fatalf("local rotation: %d", rec.Code)
	}
	next, _ := decode(t, rec)["token"].(string)
	if next == "" || next == token {
		t.Fatal("rotation must return a new token exactly once")
	}
	if rec := call(s, false, "GET", "/api/v1/daemon", token, "", nil); rec.Code != 401 {
		t.Fatal("old token still valid after rotation")
	}
	if rec := call(s, false, "GET", "/api/v1/daemon", next, "", nil); rec.Code != 200 {
		t.Fatal("new token rejected")
	}
}

// A waited-for run that failed application-wise is still a successfully
// handled HTTP request: 200 with status = "failed" (no invented status codes).
func TestTriggerWaitReturnsFailureAs200WithStatus(t *testing.T) {
	s, _, _ := setup(t)
	mustCreate(t, s, "fails", "exit 3")
	rec := call(s, true, "POST", "/api/v1/jobs/fails/trigger?wait=true&timeout=5", "", "", nil)
	if rec.Code != 200 {
		t.Fatalf("waited trigger: %d", rec.Code)
	}
	out := decode(t, rec)
	if out["status"] != "failed" {
		t.Fatalf("status %v", out["status"])
	}
	if code, _ := out["exit_code"].(float64); code != 3 {
		t.Fatalf("exit_code %v", out["exit_code"])
	}
}

func TestIdempotencyKeyScoping(t *testing.T) {
	s, _, _ := setup(t)
	mustCreate(t, s, "idem-a", "true")
	mustCreate(t, s, "idem-b", "true")
	headers := map[string]string{"Idempotency-Key": "k1"}
	first := call(s, true, "POST", "/api/v1/jobs/idem-a/trigger", "", "", headers)
	if first.Code != 202 {
		t.Fatalf("first trigger: %d", first.Code)
	}
	replay := call(s, true, "POST", "/api/v1/jobs/idem-a/trigger", "", "", headers)
	if replay.Code != 200 {
		t.Fatalf("replay: %d", replay.Code)
	}
	if decode(t, first)["run_id"] != decode(t, replay)["run_id"] {
		t.Fatal("replayed key must return the original run")
	}
	conflict := call(s, true, "POST", "/api/v1/jobs/idem-b/trigger", "", "", headers)
	if conflict.Code != 409 {
		t.Fatalf("key reuse with different target: %d", conflict.Code)
	}
}

func TestSSEStreamAndWindowedReads(t *testing.T) {
	s, _, _ := setup(t)
	mustCreate(t, s, "chatty", "echo one; echo two; echo three")
	rec := call(s, true, "POST", "/api/v1/jobs/chatty/trigger?wait=true&timeout=5", "", "", nil)
	if rec.Code != 200 {
		t.Fatalf("trigger: %d %s", rec.Code, rec.Body.String())
	}
	runID := decode(t, rec)["run_id"].(string)

	window := call(s, true, "GET", "/api/v1/runs/"+runID+"/log?after=1&limit=10", "", "", nil)
	frames, _ := decode(t, window)["items"].([]any)
	if len(frames) < 3 {
		t.Fatalf("expected stdout frames after cursor, got %d", len(frames))
	}
	if first := frames[0].(map[string]any); fmt.Sprint(first["sequence"]) != "2" {
		t.Fatalf("first sequence after cursor: %v", first["sequence"])
	}

	stream := call(s, true, "GET", "/api/v1/runs/"+runID+"/log/stream?after=2", "", "", nil)
	sse := stream.Body.String()
	if !strings.Contains(sse, "event: backlog_done") || !strings.Contains(sse, "event: done") {
		t.Fatalf("sse envelope missing: %q", sse)
	}
	// Payloads are base64 frames; assert by stable sequence cursor instead.
	if !strings.Contains(sse, `"sequence":3`) || !strings.Contains(sse, `"sequence":4`) || strings.Contains(sse, `"sequence":1`) {
		t.Fatalf("sse must replay only frames after the cursor: %q", sse)
	}

	raw := call(s, true, "GET", "/api/v1/runs/"+runID+"/log/raw", "", "", nil)
	if !strings.Contains(raw.Body.String(), "one") {
		t.Fatalf("raw body: %q", raw.Body.String())
	}
}

func TestImportPreviewApplyHashBindingAndAuthority(t *testing.T) {
	s, _, _ := setup(t)
	content := "[[job]]\nname = \"imported\"\nargv = [\"/bin/echo\", \"hi\"]\n"
	previewBody := func(c string) string { b, _ := json.Marshal(importRequest{Content: c}); return string(b) }
	if rec := call(s, true, "POST", "/api/v1/import/preview", "", previewBody("[[job]]\nbad"), nil); rec.Code != 422 {
		t.Fatalf("invalid preview: %d", rec.Code)
	}
	rec := call(s, true, "POST", "/api/v1/import/preview", "", previewBody(content), nil)
	if rec.Code != 200 {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	hash := decode(t, rec)["content_hash"].(string)
	if rec := call(s, true, "POST", "/api/v1/import/apply", "", previewBodyWithHash("x", hash), nil); rec.Code != 409 {
		t.Fatalf("hash mismatch must be rejected: %d", rec.Code)
	}
	if rec := call(s, true, "POST", "/api/v1/import/apply", "", previewBodyWithHash(content, hash), nil); rec.Code != 200 {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}

	// A file claiming the imported (db-authority) name is an authority
	// conflict and the whole file sync rolls back.
	fileDef := model.Definition{Name: "imported", Kind: model.KindJob, Authority: "file", SourceFile: "jobs.toml",
		Command: "true", Shell: "/bin/sh", Timezone: "UTC", Timeout: "0", Grace: "0", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
	if err := s.store.SyncFiles(t.Context(), []model.Definition{fileDef}, false); err == nil {
		t.Fatal("expected authority conflict at file level")
	}
	// A genuinely file-authority definition cannot be edited through the API.
	linked := fileDef
	linked.Name = "linked"
	if err := s.store.SyncFiles(t.Context(), []model.Definition{linked}, false); err != nil {
		t.Fatal(err)
	}
	if rec := call(s, true, "PUT", "/api/v1/jobs/linked", "", `{"name":"linked","kind":"job","command":"true","shell":"/bin/sh"}`, nil); rec.Code != 409 {
		t.Fatalf("file-authority edit: %d", rec.Code)
	}
}

func TestOptimisticRevisionConflict(t *testing.T) {
	s, _, _ := setup(t)
	mustCreate(t, s, "rev", "true")
	body := `{"name":"rev","kind":"job","command":"true","shell":"/bin/sh"}`
	if rec := call(s, true, "PUT", "/api/v1/jobs/rev", "", body, map[string]string{"If-Match": `"99"`}); rec.Code != 412 {
		t.Fatalf("stale If-Match: %d", rec.Code)
	}
}

func TestMissingRunReturnsEnvelope(t *testing.T) {
	s, _, _ := setup(t)
	rec := call(s, true, "GET", "/api/v1/runs/01980000-0000-7000-8000-000000000000", "", "", nil)
	if rec.Code != 404 {
		t.Fatalf("missing run: %d", rec.Code)
	}
	if decode(t, rec)["error"] == nil {
		t.Fatal("errors must use the stable envelope")
	}
}

// A wait=true trigger polls for up to `timeout` seconds; it must not hold the
// idempotency mutex while waiting, or every other keyed trigger would queue
// behind it.
func TestWaitTriggerDoesNotHoldIdempotencyLock(t *testing.T) {
	s, _, _ := setup(t)
	mustCreate(t, s, "slow", "sleep 2")
	mustCreate(t, s, "quick", "true")
	go call(s, true, "POST", "/api/v1/jobs/slow/trigger?wait=true&timeout=3", "", "", map[string]string{"Idempotency-Key": "waiter"})
	time.Sleep(200 * time.Millisecond) // let the waiter settle into its poll loop
	start := time.Now()
	rec := call(s, true, "POST", "/api/v1/jobs/quick/trigger", "", "", map[string]string{"Idempotency-Key": "other"})
	if rec.Code != 202 {
		t.Fatalf("concurrent keyed trigger: %d %s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("keyed trigger blocked %s behind a waiting trigger", elapsed)
	}
}
