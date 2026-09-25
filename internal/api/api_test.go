package api

import (
	"bytes"
	"compress/gzip"
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

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
	"github.com/khanhicetea/minicrond/internal/supervisor"
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
	// Cleanup runs in LIFO order, so stop all executions before closing st.
	t.Cleanup(func() { ex.Shutdown(context.Background()) })
	sup := supervisor.New(st, ex)
	callback := func(context.Context) error { return nil }
	srv := New(st, logs, ex, sup, callback, callback, "test")
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

func TestAlertChannelsAndValidation(t *testing.T) {
	s, token, _ := setup(t)
	s.SetAlertChannels(func() []config.AlertChannel {
		return []config.AlertChannel{{Name: "ops", Type: "telegram", BatchWindow: 10, BotToken: "env:SECRET"}}
	})
	list := call(s, false, "GET", "/api/v1/alert-channels", token, "", nil)
	if list.Code != 200 || strings.Contains(list.Body.String(), "SECRET") || !strings.Contains(list.Body.String(), `"batch_window":10`) {
		t.Fatalf("channel list: %d %s", list.Code, list.Body.String())
	}
	tested := false
	s.SetAlertTest(func(_ context.Context, name string) error {
		tested = name == "ops"
		return nil
	})
	if response := call(s, false, "POST", "/api/v1/alert-channels/ops/test", token, "", nil); response.Code != 200 || !tested {
		t.Errorf("test alert: %d %s", response.Code, response.Body.String())
	}
	if response := call(s, false, "POST", "/api/v1/alert-channels/typo/test", token, "", nil); response.Code != 404 {
		t.Errorf("unknown channel: %d %s", response.Code, response.Body.String())
	}
	if response := call(s, false, "GET", "/api/v1/metrics/alerts", token, "", nil); response.Code != 200 || !strings.Contains(response.Body.String(), "queue_depth") {
		t.Errorf("alert metrics: %d %s", response.Code, response.Body.String())
	}
	for _, body := range []string{
		`{"name":"example","command":"true","alerts":["typo"]}`,
		`{"name":"example","command":"true","alerts":["ops","ops"]}`,
	} {
		response := call(s, false, "POST", "/api/v1/jobs", token, body, nil)
		if response.Code != 422 {
			t.Errorf("expected invalid alerts: %d %s", response.Code, response.Body.String())
		}
	}
	preview := call(s, false, "POST", "/api/v1/import/preview", token, previewBodyWithHash("[[job]]\nname='example'\ncommand='true'\nalerts=['typo']\n", ""), nil)
	if preview.Code != 422 {
		t.Errorf("expected invalid import: %d %s", preview.Code, preview.Body.String())
	}
}

func TestLogMaxUnits(t *testing.T) {
	s, token, _ := setup(t)
	body := `{"name":"sized","kind":"job","command":"true","log_max":10}`
	rec := call(s, false, "POST", "/api/v1/jobs", token, body, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"log_max":10`) {
		t.Fatalf("create sized job: %d %s", rec.Code, rec.Body.String())
	}
	for _, size := range []string{`"10MiB"`, `1048577`, `-1`} {
		body = `{"name":"invalid","kind":"job","command":"true","log_max":` + size + `}`
		rec = call(s, false, "POST", "/api/v1/jobs", token, body, nil)
		if rec.Code != 422 {
			t.Errorf("log_max=%s: %d %s", size, rec.Code, rec.Body.String())
		}
	}
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

func TestReload(t *testing.T) {
	called := false
	s := &Server{reload: func(context.Context) error {
		called = true
		return nil
	}}

	rec := call(s, true, "POST", "/api/v1/daemon/reload", "", "", nil)
	if rec.Code != 200 {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("reload callback was not called")
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

func TestWorkerStatesOnlyIncludesWorkerDefinitions(t *testing.T) {
	s, token, _ := setup(t)
	worker := `{"name":"worker-one","kind":"worker","command":"true","shell":"/bin/sh"}`
	job := `{"name":"job-one","kind":"job","command":"true","shell":"/bin/sh"}`
	for _, definition := range []struct{ name, body string }{{"worker-one", worker}, {"job-one", job}} {
		rec := call(s, true, "PUT", "/api/v1/jobs/"+definition.name, "", definition.body, nil)
		if rec.Code != 200 {
			t.Fatalf("create %s: %d %s", definition.name, rec.Code, rec.Body.String())
		}
	}
	rec := call(s, false, "GET", "/api/v1/workers/states", token, "", nil)
	if rec.Code != 200 {
		t.Fatalf("worker states: %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Items map[string]supervisor.State `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items["worker-one"] != (supervisor.State{}) {
		t.Fatalf("unexpected worker states: %+v", response.Items)
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

func TestHashedAssetCompressionAndCaching(t *testing.T) {
	s, _, _ := setup(t)
	var name string
	for candidate := range cachedAssets() {
		if strings.HasPrefix(candidate, "app-") && strings.HasSuffix(candidate, ".js") {
			name = candidate
			break
		}
	}
	if name == "" {
		t.Fatal("missing hashed app asset")
	}
	rec := call(s, false, "GET", "/assets/"+name, "", "", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset headers: %d %+v", rec.Code, rec.Header())
	}
	reader, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, cachedAssets()[name].body) {
		t.Fatal("compressed asset did not match embedded file")
	}
	cache := call(s, false, "GET", "/assets/"+name, "", "", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": rec.Header().Get("ETag")})
	if cache.Code != 304 {
		t.Fatalf("conditional asset request: %d", cache.Code)
	}
	identity := call(s, false, "GET", "/assets/"+name, "", "", map[string]string{"Accept-Encoding": "gzip;q=0"})
	if identity.Code != 200 || identity.Header().Get("Content-Encoding") != "" {
		t.Fatalf("gzip;q=0 must receive identity bytes: %d %+v", identity.Code, identity.Header())
	}
}

func TestRunsPaginationAndFilter(t *testing.T) {
	s, token, st := setup(t)
	empty := call(s, false, "GET", "/api/v1/runs?job=pages&limit=2", token, "", nil)
	if empty.Code != 200 {
		t.Fatalf("empty page: %d %s", empty.Code, empty.Body.String())
	}
	items, ok := decode(t, empty)["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("empty run list must be an array: %s", empty.Body.String())
	}
	mustCreate(t, s, "pages", "true")
	def, _, err := st.Definition(t.Context(), "pages")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	for i, status := range []string{"failed", "succeeded", "running"} {
		run := model.Run{ID: fmt.Sprintf("page-%d", i), DefinitionID: def.ID, Job: "pages", Kind: model.KindJob, Revision: 1, Status: status, Trigger: "manual", QueuedAt: base.Add(time.Duration(i) * time.Second)}
		if err := st.CreateRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
	first := call(s, false, "GET", "/api/v1/runs?job=pages&limit=2", token, "", nil)
	if first.Code != 200 {
		t.Fatalf("first page: %d %s", first.Code, first.Body.String())
	}
	var page struct {
		Items      []model.Run `json:"items"`
		NextBefore string      `json:"next_before"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextBefore != page.Items[1].ID {
		t.Fatalf("first page = %+v", page)
	}
	second := call(s, false, "GET", "/api/v1/runs?job=pages&limit=2&before="+page.NextBefore, token, "", nil)
	if second.Code != 200 || !strings.Contains(second.Body.String(), `"run_id":"page-0"`) {
		t.Fatalf("second page: %d %s", second.Code, second.Body.String())
	}
	filtered := call(s, false, "GET", "/api/v1/runs?job=pages&filter=active", token, "", nil)
	if filtered.Code != 200 || !strings.Contains(filtered.Body.String(), `"run_id":"page-2"`) || strings.Contains(filtered.Body.String(), `"run_id":"page-1"`) {
		t.Fatalf("active filter: %d %s", filtered.Code, filtered.Body.String())
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

func TestImportPreviewApplyHashBinding(t *testing.T) {
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
}

func TestDefinitionMutationReconcilesWithoutReloadingSettings(t *testing.T) {
	s, _, _ := setup(t)
	reloads, reconciles := 0, 0
	s.reload = func(context.Context) error { reloads++; return nil }
	s.reconcile = func(context.Context) error { reconciles++; return nil }

	rec := call(s, true, "PUT", "/api/v1/jobs/hello", "", `{"name":"hello","kind":"job","command":"true"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if reloads != 0 || reconciles != 1 {
		t.Fatalf("reloads=%d reconciles=%d, want 0 and 1", reloads, reconciles)
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

// A wait=true trigger must not hold the idempotency mutex while waiting, or
// every other keyed trigger would queue behind it.
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
