package logstore

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// The diagnostics report every tier's footprint, including quarantine, and are
// computed on demand: a second call within the cache window does not walk again.
func TestDiagnosticsReportTierBytes(t *testing.T) {
	f := newBudgetFixture(t)
	f.completedRun("archived", 100<<10)
	live, err := f.s.Open("live", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close("live")
	if err := live.Write(Stdout, payload(40<<10), 0); err != nil {
		t.Fatal(err)
	}
	f.s.AttachDB(nil)
	w, err := f.s.Open("sealed", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, payload(30<<10), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close("sealed"); err != nil {
		t.Fatal(err)
	}
	f.s.AttachDB(f.ldb)
	addQuarantined(t, f.s, "q.corrupt", 7000, 2*time.Hour)
	f.policy(1<<30, 1<<20)
	f.enforce()

	d := f.s.Diagnostics(t.Context())
	if d.Disk.ArchiveBytes < 100<<10 || d.Disk.HotBytes < 40<<10 || d.Disk.HotRuns != 1 ||
		d.Disk.SealedBytes < 30<<10 || d.Disk.SealedRuns != 1 || d.Disk.QuarantineBytes != 7000 || d.Disk.QuarantineEntries != 1 {
		t.Fatalf("tier bytes = %+v", d.Disk)
	}
	if d.Disk.QuarantineOldestS < 2*3600-5 || d.Disk.LogBytes != d.Disk.ArchiveBytes+d.Disk.SealedBytes+d.Disk.HotBytes {
		t.Fatalf("derived fields = %+v", d.Disk)
	}
	if !d.Disk.BudgetEnabled || d.Disk.BudgetBytes != 1<<30 || d.Disk.Passes != 1 || d.Disk.FreeBytes == 0 {
		t.Fatalf("policy fields = %+v", d.Disk)
	}
	// The measurement is cached: new data inside the window is not seen yet.
	addQuarantined(t, f.s, "q2.corrupt", 1000, time.Hour)
	if again := f.s.Diagnostics(t.Context()); again.Disk.QuarantineBytes != 7000 {
		t.Fatalf("diagnostics walked again within the cache window: %d", again.Disk.QuarantineBytes)
	}
	// JSON shape consumed by the daemon endpoint.
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"hot_bytes"`, `"sealed_bytes"`, `"quarantine_bytes"`, `"archive_bytes"`, `"maintenance"`, `"writer_lock_waits"`, `"capture"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("diagnostics JSON lacks %s", key)
		}
	}
}

func TestDiagnosticsReportCaptureFailures(t *testing.T) {
	setCaptureRetry(t, time.Hour, time.Hour)
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := s.Open("healthy", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := s.Open("broken", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := healthy.Write(Stdout, []byte("ok"), 0); err != nil {
		t.Fatal(err)
	}
	breakStorage(t, broken)
	for range 5 {
		broken.capture(Stdout, []byte("lost line"), 0)
	}
	c := s.CaptureDiagnostics()
	if c.FailuresTotal != 1 || c.ActiveWriters != 2 || c.Degraded != 1 || len(c.DegradedRuns) != 1 {
		t.Fatalf("capture = %+v", c)
	}
	r := c.DegradedRuns[0]
	if r.RunID != "broken" || r.DroppedFrames != 5 || r.DroppedBytes == 0 || r.Error == "" {
		t.Fatalf("degraded run = %+v", r)
	}
	if c.DroppedFrames != 5 || c.DroppedBytes != r.DroppedBytes {
		t.Fatalf("totals = %+v", c)
	}
	// Totals survive the run; the live list does not.
	if err := s.Close("broken"); err != nil {
		t.Fatal(err)
	}
	c = s.CaptureDiagnostics()
	if c.Degraded != 0 || c.FailuresTotal != 1 || c.DroppedFrames != 5 {
		t.Fatalf("after close: %+v", c)
	}
}

// A writer busy under its lock (an archive batch holds it across a database
// transaction) must not stall diagnostics.
func TestCaptureDiagnosticsNeverWaitsForABusyWriter(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("busy", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close("busy")
	w.mu.Lock()
	done := make(chan CaptureDiagnostics)
	go func() { done <- s.CaptureDiagnostics() }()
	select {
	case c := <-done:
		if c.Unavailable != 1 {
			t.Fatalf("capture = %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics blocked on a busy writer")
	}
	w.mu.Unlock()
}

// Writes that blocked on the run lock are counted and timed; uncontended
// writes are not.
func TestWriterLockWaitsAreRecordedOnlyWhenBlocked(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close("run")
	for range 20 {
		if err := w.Write(Stdout, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.WriterLockWaits(); got.Count != 0 {
		t.Fatalf("uncontended writes recorded waits: %+v", got)
	}
	// Hold the run lock as an archive batch would.
	lock := s.lockRun("run", true)
	var wg sync.WaitGroup
	wg.Go(func() { _ = w.Write(Stdout, []byte("blocked"), 0) })
	time.Sleep(60 * time.Millisecond)
	s.unlockRun("run", lock, true)
	wg.Wait()
	got := s.WriterLockWaits()
	if got.Count != 1 || got.MaxMS < 50 || got.TotalMS < 50 {
		t.Fatalf("lock waits = %+v; want one wait of about 60 ms", got)
	}
	// And the same for the writer's own mutex.
	w.mu.Lock()
	wg.Go(func() { _ = w.Write(Stdout, []byte("blocked 2"), 0) })
	time.Sleep(60 * time.Millisecond)
	w.mu.Unlock()
	wg.Wait()
	if got := s.WriterLockWaits(); got.Count != 2 {
		t.Fatalf("writer mutex wait not recorded: %+v", got)
	}
}

func TestMaintenanceDurationsAreRecorded(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.MaintenanceStats()) != 0 {
		t.Fatal("stats before any task ran")
	}
	s.RecordMaintenance("retention", 30*time.Millisecond)
	s.RecordMaintenance("retention", 10*time.Millisecond)
	s.RecordMaintenance("log_prune", time.Second)
	got := s.MaintenanceStats()
	r := got["retention"]
	if r.Runs != 2 || r.LastMS != 10 || r.MaxMS != 30 || r.TotalMS != 40 || r.LastAt.IsZero() || got["log_prune"].MaxMS != 1000 {
		t.Fatalf("stats = %+v", got)
	}
	got["retention"] = MaintenanceStat{}
	if s.MaintenanceStats()["retention"].Runs != 2 {
		t.Fatal("MaintenanceStats returned shared state")
	}
}
