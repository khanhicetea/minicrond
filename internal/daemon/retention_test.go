package daemon

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func TestSweepRetentionProcessesMultiplePages(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	def, err := st.PutDefinition(t.Context(), model.Definition{
		Name: "many", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC",
		OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0},
	}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 270 {
		at := base.Add(time.Duration(i) * time.Second)
		if err := st.CreateRun(t.Context(), model.Run{
			ID: fmt.Sprintf("run-%03d", i), DefinitionID: def.ID, Job: def.Name,
			Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: at, EndedAt: &at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveIdempotency(t.Context(), "client", "trigger", "key", "hash", "run-005"); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: &config.Config{Storage: config.Storage{
		KeepRunsDefault: 2, KeepForDefault: 30, AuditKeep: 1,
	}}, store: st, logs: logs}
	d.sweepRetention(t.Context())
	runs, err := st.Runs(t.Context(), def.Name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].ID != "run-269" || runs[1].ID != "run-268" || runs[2].ID != "run-005" {
		t.Fatalf("remaining runs = %v", runs)
	}
}
