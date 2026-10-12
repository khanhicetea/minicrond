package store

import (
	"errors"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestWorkerExhaustionSurvivesReopenAndRejectsOldRevision(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.PutDefinition(t.Context(), model.Definition{Name: "worker", Kind: model.KindWorker, Command: "false"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	outcome := model.WorkerExhaustion{Reason: "start_error", Attempts: 5, Revision: d.Revision, At: time.Now().UTC()}
	if err := st.SaveWorkerExhaustion(t.Context(), d, outcome); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	saved, err := st.WorkerExhaustions(t.Context())
	if err != nil || saved[d.Name].Attempts != 5 {
		t.Fatalf("reopen: %v %v", saved, err)
	}
	changed := d
	changed.Command = "true"
	if _, err := st.PutDefinition(t.Context(), changed, d.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveWorkerExhaustion(t.Context(), d, outcome); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("old revision: %v", err)
	}
	saved, err = st.WorkerExhaustions(t.Context())
	if err != nil || len(saved) != 0 {
		t.Fatalf("old state exposed: %v %v", saved, err)
	}
	if err := st.ClearWorkerExhaustion(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
}
