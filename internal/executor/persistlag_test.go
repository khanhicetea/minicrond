package executor

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
)

// A normal completion records its end-to-durable-commit latency.
func TestPersistenceLagIsRecordedForACompletedRun(t *testing.T) {
	finished := make(chan struct{}, 1)
	_, st, s := resilienceService(t, Options{OnFinished: func(model.Run, model.Definition) { finished <- struct{}{} }})
	if got := s.PersistenceLag(); got.Persisted != 0 || got.Pending != 0 {
		t.Fatalf("lag before any run: %+v", got)
	}
	d, hash := putJob(t, st, model.Definition{Name: "lag", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", OnOverlap: "skip", SuccessCodes: []int{0}})
	if _, err := s.Trigger(t.Context(), d, hash, "manual", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("run never finished")
	}
	got := s.PersistenceLag()
	if got.Persisted != 1 || got.Pending != 0 || got.MaxMS < 0 || got.MaxMS > 5000 || got.LastMS != got.MaxMS {
		t.Fatalf("lag = %+v", got)
	}
}

// Runs whose terminal state is stuck behind the background finalizer are
// reported with the age of the oldest one; the lag of a late commit is long.
func TestPersistenceLagReportsPendingFinalizers(t *testing.T) {
	var p persistLag
	older := time.Now().Add(-3 * time.Second)
	p.startPending(time.Now().Add(-time.Second))
	p.startPending(older)
	p.startPending(time.Now())
	got := p.snapshot()
	if got.Pending != 3 || got.PendingOldestAgeMS < 2900 {
		t.Fatalf("pending = %+v", got)
	}
	for range 3 {
		p.endPending()
	}
	p.observe(older) // the finalizer finally committed a run that ended 3 s ago
	got = p.snapshot()
	if got.Pending != 0 || got.PendingOldestAgeMS != 0 || got.Persisted != 1 || got.MaxMS < 2900 {
		t.Fatalf("after commit: %+v", got)
	}
}

// During a metadata outage past the inline budget the run shows up as a pending
// finalizer; once storage returns the late commit appears as a long lag.
func TestPersistenceLagDuringStorageOutage(t *testing.T) {
	finished := make(chan struct{}, 1)
	dir, st, s := resilienceService(t, Options{OnFinished: func(model.Run, model.Definition) { finished <- struct{}{} }})
	d, hash := putJob(t, st, model.Definition{Name: "lag-outage", Kind: model.KindJob, Command: "sleep 0.5", Shell: "/bin/sh", OnOverlap: "skip", SuccessCodes: []int{0}})
	run, err := s.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "run never started", func() bool {
		r, err := st.Run(t.Context(), run.ID)
		return err == nil && r.Status == "running"
	})
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	if _, err := saboteur.Exec("ALTER TABLE runs RENAME TO runs_offline"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "the failed persistence never showed as a pending finalizer", func() bool { return s.PersistenceLag().Pending == 1 })
	time.Sleep(time.Second)
	if got := s.PersistenceLag(); got.PendingOldestAgeMS < 1000 || got.Persisted != 0 {
		t.Fatalf("during outage: %+v", got)
	}
	if _, err := saboteur.Exec("ALTER TABLE runs_offline RENAME TO runs"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("terminal state never persisted after recovery")
	}
	got := s.PersistenceLag()
	if got.Pending != 0 || got.Persisted != 1 || got.MaxMS < 1000 {
		t.Fatalf("after recovery: %+v", got)
	}
}
