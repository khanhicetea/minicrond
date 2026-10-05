package api

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/sqlite"
	"github.com/khanhicetea/minicrond/internal/store"
	"github.com/khanhicetea/minicrond/internal/supervisor"
)

func queueServer(t *testing.T, queue executor.QueueOptions) (*Server, *store.Store, string) {
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
	ex := executor.New(st, logs, executor.Options{MaxConcurrentRuns: 1, Queue: queue})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ex.Shutdown(ctx)
	})
	callback := func(context.Context) error { return nil }
	srv := New(st, logs, ex, supervisor.New(st, ex), callback, callback, "test")
	if _, err := srv.InitializeToken(t.Context()); err != nil {
		t.Fatal(err)
	}
	return srv, st, dir
}

// A queued trigger is acknowledged with 202 and a queued run; a full queue is an
// explicit 429 with Retry-After; diagnostics expose depth and the rejection.
func TestTriggerQueuesAndRejectsWhenFull(t *testing.T) {
	s, _, _ := queueServer(t, executor.QueueOptions{MaxItems: 1})
	mustCreate(t, s, "blocker", "sleep 5")
	mustCreate(t, s, "a", "true")
	mustCreate(t, s, "b", "true")
	if rec := call(s, true, "POST", "/api/v1/jobs/blocker/trigger", "", "", nil); rec.Code != 202 {
		t.Fatalf("blocker: %d %s", rec.Code, rec.Body.String())
	}
	// Pending runs also count as queued in metrics; wait for the blocker to run.
	deadline := time.Now().Add(5 * time.Second)
	for {
		items := decode(t, call(s, true, "GET", "/api/v1/runs?job=blocker", "", "", nil))["items"].([]any)
		if len(items) == 1 && items[0].(map[string]any)["status"] == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocker never started: %v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
	queued := call(s, true, "POST", "/api/v1/jobs/a/trigger", "", "", nil)
	if queued.Code != 202 || decode(t, queued)["status"] != "queued" {
		t.Fatalf("over-capacity trigger: %d %s", queued.Code, queued.Body.String())
	}
	full := call(s, true, "POST", "/api/v1/jobs/b/trigger", "", "", nil)
	if full.Code != 429 || full.Header().Get("Retry-After") == "" {
		t.Fatalf("full queue: %d retry-after=%q %s", full.Code, full.Header().Get("Retry-After"), full.Body.String())
	}
	errBody := decode(t, full)["error"].(map[string]any)
	if errBody["code"] != "queue_full" {
		t.Fatalf("error = %v", errBody)
	}
	run := call(s, true, "GET", "/api/v1/runs/"+decode(t, queued)["run_id"].(string), "", "", nil)
	if run.Code != 200 || decode(t, run)["status"] != "queued" {
		t.Fatalf("queued run lookup: %d %s", run.Code, run.Body.String())
	}
	info := decode(t, call(s, true, "GET", "/api/v1/daemon", "", "", nil))
	diag := info["diagnostics"].(map[string]any)
	q := diag["execution_queue"].(map[string]any)
	if q["depth"].(float64) != 1 || q["rejected"].(float64) != 1 || q["max_items"].(float64) != 1 || q["enabled"] != true {
		t.Fatalf("execution_queue = %v", q)
	}
	for _, key := range []string{"pending_retries", "finalizers", "log_archive"} {
		if _, ok := diag[key]; !ok {
			t.Fatalf("diagnostics missing %q: %v", key, diag)
		}
	}
	metrics := decode(t, call(s, true, "GET", "/api/v1/metrics/runs?range=15m", "", "", nil))
	if metrics["queued"].(float64) != 1 {
		t.Fatalf("run metrics queued = %v", metrics["queued"])
	}
	active := call(s, true, "GET", "/api/v1/runs?filter=active", "", "", nil)
	if items := decode(t, active)["items"].([]any); len(items) != 2 {
		t.Fatalf("active filter = %d items, want the running and the queued run", len(items))
	}
}

// If the queue cannot be persisted the API says so (503) instead of pretending.
func TestTriggerReportsQueueStorageFailure(t *testing.T) {
	s, st, dir := queueServer(t, executor.QueueOptions{})
	mustCreate(t, s, "blocker", "sleep 5")
	mustCreate(t, s, "a", "true")
	if rec := call(s, true, "POST", "/api/v1/jobs/blocker/trigger", "", "", nil); rec.Code != 202 {
		t.Fatalf("blocker: %d", rec.Code)
	}
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	if _, err := saboteur.Exec("ALTER TABLE exec_queue RENAME TO exec_queue_offline"); err != nil {
		t.Fatal(err)
	}
	rec := call(s, true, "POST", "/api/v1/jobs/a/trigger", "", "", nil)
	if rec.Code != 503 || decode(t, rec)["error"].(map[string]any)["code"] != "queue_unavailable" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("storage failure: %d %s", rec.Code, rec.Body.String())
	}
	if runs, err := st.Runs(t.Context(), "a", 10); err != nil || len(runs) != 0 {
		t.Fatalf("failed trigger left runs: %+v, %v", runs, err)
	}
}

// S2: the Stop button works for a queued run (202, then stopped/queue_cancelled).
func TestStopQueuedRunViaAPI(t *testing.T) {
	s, st, _ := queueServer(t, executor.QueueOptions{})
	mustCreate(t, s, "blocker", "sleep 5")
	mustCreate(t, s, "a", "true")
	if rec := call(s, true, "POST", "/api/v1/jobs/blocker/trigger", "", "", nil); rec.Code != 202 {
		t.Fatalf("blocker: %d", rec.Code)
	}
	queued := decode(t, call(s, true, "POST", "/api/v1/jobs/a/trigger", "", "", nil))
	id := queued["run_id"].(string)
	if queued["status"] != "queued" {
		t.Fatalf("status = %v", queued["status"])
	}
	if rec := call(s, true, "POST", "/api/v1/runs/"+id+"/stop", "", "", nil); rec.Code != 202 {
		t.Fatalf("stop queued: %d %s", rec.Code, rec.Body.String())
	}
	got, err := st.Run(t.Context(), id)
	if err != nil || got.Status != "stopped" || got.EndReason != "queue_cancelled" {
		t.Fatalf("run = %+v, %v", got, err)
	}
}

// S4: following a queued run must not end with "done" before it ever starts;
// the stream keeps polling and delivers the output and the final "done".
func TestSSEStreamOfQueuedRunWaitsForStart(t *testing.T) {
	fastStreamPoll(t)
	s, st, _ := queueServer(t, executor.QueueOptions{DrainRate: 100})
	mustCreate(t, s, "blocker", "sleep 30")
	mustCreate(t, s, "later", "echo finally-ran")
	blocker := decode(t, call(s, true, "POST", "/api/v1/jobs/blocker/trigger", "", "", nil))["run_id"].(string)
	queued := decode(t, call(s, true, "POST", "/api/v1/jobs/later/trigger", "", "", nil))
	id := queued["run_id"].(string)
	if queued["status"] != "queued" {
		t.Fatalf("status = %v", queued["status"])
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/api/v1/runs/"+id+"/log/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.middleware(s.routes(), true).ServeHTTP(rec, req)
	}()
	select {
	case <-finished:
		t.Fatalf("stream of a queued run ended before it started: %q", rec.Body.String())
	case <-time.After(500 * time.Millisecond):
	}
	// Free the slot: the queued run starts, runs and finishes.
	if rec := call(s, true, "POST", "/api/v1/runs/"+blocker+"/stop", "", "", nil); rec.Code != 202 {
		t.Fatalf("stop blocker: %d", rec.Code)
	}
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("stream never finished after the run completed")
	}
	got, err := st.Run(t.Context(), id)
	if err != nil || got.Status != "succeeded" {
		t.Fatalf("run = %+v, %v", got, err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: line") || !strings.Contains(body, "event: done") {
		t.Fatalf("stream body = %q", body)
	}
}
