package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A13: the caller's context is canceled right after the import commits. The
// reconcile must still run to completion on a daemon-owned context, retrying
// transient failures, with no further API call.
func TestImportApplyReconcilesAfterCallerCancelsAndRetries(t *testing.T) {
	s, _, st := setup(t)
	t.Cleanup(s.super.Shutdown)
	old := reconcileBackoff
	reconcileBackoff = time.Millisecond
	t.Cleanup(func() { reconcileBackoff = old })

	// Mirror the daemon: authoritative query, then reload supervision. A
	// canceled context fails the query exactly as it does in production.
	var attempts atomic.Int32
	var cancelRequest context.CancelFunc
	s.reconcile = func(ctx context.Context) error {
		n := attempts.Add(1)
		if n == 1 {
			cancelRequest() // the client disconnects immediately after commit
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == 1 {
			return errors.New("transient scheduler reload failure")
		}
		defs, err := st.Definitions(ctx)
		if err != nil {
			return err
		}
		s.super.Reload(defs)
		return nil
	}
	apply := func(content string) *httptest.ResponseRecorder {
		t.Helper()
		attempts.Store(0)
		reqCtx, cancel := context.WithCancel(t.Context())
		cancelRequest = cancel
		defer cancel()
		hash := decode(t, call(s, true, "POST", "/api/v1/import/preview", "", mustJSON(t, importRequest{Content: content}), nil))["content_hash"].(string)
		req := httptest.NewRequest("POST", "/api/v1/import/apply", strings.NewReader(previewBodyWithHash(content, hash))).WithContext(reqCtx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.middleware(s.routes(), true).ServeHTTP(rec, req)
		return rec
	}

	if rec := apply("[[worker]]\nname='w'\ncommand='sleep 30'\ngrace=1\n"); rec.Code != 200 {
		t.Fatalf("first apply: %d %s", rec.Code, rec.Body.String())
	}
	if attempts.Load() < 2 {
		t.Fatalf("reconcile attempts=%d, want a retry after the transient failure", attempts.Load())
	}
	waitFor(t, "worker loop started", func() bool { return s.super.State("w").Active })

	// Replace/disable the worker; only the import call is made.
	if rec := apply("[[worker]]\nname='w'\ncommand='sleep 30'\ngrace=1\nenabled=false\n"); rec.Code != 200 {
		t.Fatalf("second apply: %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "old worker loop stopped", func() bool { return !s.super.State("w").Active })
}

func TestImportApplyReconcileGivesUpAfterBoundedAttempts(t *testing.T) {
	s, _, _ := setup(t)
	old := reconcileBackoff
	reconcileBackoff = time.Millisecond
	t.Cleanup(func() { reconcileBackoff = old })
	var attempts atomic.Int32
	s.reconcile = func(context.Context) error { attempts.Add(1); return errors.New("down") }
	content := "[[job]]\nname='x'\ncommand='true'\n"
	hash := decode(t, call(s, true, "POST", "/api/v1/import/preview", "", mustJSON(t, importRequest{Content: content}), nil))["content_hash"].(string)
	rec := call(s, true, "POST", "/api/v1/import/apply", "", previewBodyWithHash(content, hash), nil)
	if rec.Code != 500 {
		t.Fatalf("persistent reconcile failure must surface: %d", rec.Code)
	}
	if got := attempts.Load(); got != reconcileAttempts {
		t.Fatalf("attempts=%d, want %d", got, reconcileAttempts)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
