package api

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
)

// The daemon endpoint exposes the log-storage and persistence observations the
// audit asks for: tier bytes, capture failures, maintenance durations,
// writer-lock waits and terminal-persistence lag.
func TestDaemonDiagnosticsExposeLogStorageAndPersistence(t *testing.T) {
	s, token, _ := setup(t)
	w, err := s.logs.Open("diag-run", "job", model.KindJob, logstore.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.logs.Close("diag-run")
	if err := w.Write(logstore.Stdout, []byte("hello diagnostics"), 0); err != nil {
		t.Fatal(err)
	}
	s.logs.RecordMaintenance("retention", 12*time.Millisecond)
	rec := call(s, false, "GET", "/api/v1/daemon", token, "", nil)
	if rec.Code != 200 {
		t.Fatalf("daemon: %d %s", rec.Code, rec.Body.String())
	}
	diagnostics, ok := decode(t, rec)["diagnostics"].(map[string]any)
	if !ok {
		t.Fatal("no diagnostics object")
	}
	storage, ok := diagnostics["log_storage"].(map[string]any)
	if !ok {
		t.Fatalf("diagnostics.log_storage missing: %v", diagnostics)
	}
	disk, _ := storage["disk"].(map[string]any)
	for _, key := range []string{"hot_bytes", "sealed_bytes", "archive_bytes", "quarantine_bytes", "log_bytes", "pressure", "insufficient", "pruned_runs"} {
		if _, ok := disk[key]; !ok {
			t.Errorf("log_storage.disk lacks %q: %v", key, disk)
		}
	}
	if disk["hot_runs"] != float64(1) || disk["hot_bytes"].(float64) <= 0 {
		t.Fatalf("live buffer not accounted: %v", disk)
	}
	capture, _ := storage["capture"].(map[string]any)
	if capture["active_writers"] != float64(1) || capture["failures_total"] != float64(0) {
		t.Fatalf("capture = %v", capture)
	}
	maintenance, _ := storage["maintenance"].(map[string]any)
	if r, _ := maintenance["retention"].(map[string]any); r["runs"] != float64(1) || r["last_ms"] != float64(12) {
		t.Fatalf("maintenance = %v", maintenance)
	}
	if _, ok := storage["writer_lock_waits"].(map[string]any); !ok {
		t.Fatalf("writer_lock_waits missing: %v", storage)
	}
	persistence, _ := diagnostics["terminal_persistence"].(map[string]any)
	for _, key := range []string{"persisted", "last_ms", "max_ms", "pending", "pending_oldest_age_ms"} {
		if _, ok := persistence[key]; !ok {
			t.Errorf("terminal_persistence lacks %q: %v", key, persistence)
		}
	}
}
