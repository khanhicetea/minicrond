package supervisor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
)

func TestExhaustionRetriesStorageWithoutRestarting(t *testing.T) {
	dir, st, _, s := setupWithExecutor(t)
	d := workerDef("storage-failure", "", []string{"/bin/false"}, 30, 1)
	var err error
	d, err = st.PutDefinition(t.Context(), d, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_exhaustion BEFORE INSERT ON meta WHEN NEW.key LIKE 'worker_exhaustion:%' BEGIN SELECT RAISE(ABORT, 'injected exhaustion failure'); END`); err != nil {
		t.Fatal(err)
	}
	fake := &instantExecutor{store: st, status: "failed"}
	s.exec = fake
	notified := make(chan model.WorkerExhaustion, 1)
	s.onExhausted = func(_ model.Definition, o model.WorkerExhaustion) { notified <- o }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.loop(ctx, d) }()
	defer func() { cancel(); <-done }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !s.State(d.Name).ExhaustionPending {
		select {
		case <-deadline.C:
			t.Fatal("no pending state")
		case <-tick.C:
		}
	}
	select {
	case <-notified:
		t.Fatal("alert before persistence")
	default:
	}
	if _, err := db.Exec(`DROP TRIGGER reject_exhaustion`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notified:
	case <-deadline.C:
		t.Fatal("no alert after recovery")
	}
	<-done
	if fake.starts != 1 || s.State(d.Name).ExhaustionPending {
		t.Fatalf("starts=%d state=%+v", fake.starts, s.State(d.Name))
	}
}
