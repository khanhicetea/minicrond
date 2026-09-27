package daemon

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
	_ "modernc.org/sqlite"
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

func TestSweepRetentionBatchFailureFallsBack(t *testing.T) {
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
	def, err := st.PutDefinition(t.Context(), model.Definition{Name: "fallback", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		at := base.Add(time.Duration(i) * time.Second)
		if err := st.CreateRun(t.Context(), model.Run{ID: fmt.Sprintf("run-%d", i), DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: at, EndedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	control, err := sql.Open("sqlite", filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.ExecContext(t.Context(), `CREATE TRIGGER abort_oldest BEFORE DELETE ON runs
		WHEN OLD.run_id='run-0' BEGIN SELECT RAISE(ABORT, 'injected retention failure'); END`); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: &config.Config{Storage: config.Storage{KeepRunsDefault: 1, KeepForDefault: 30, AuditKeep: 1}}, store: st, logs: logs}
	d.sweepRetention(t.Context())
	runs, err := st.Runs(t.Context(), def.Name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "run-2" || runs[1].ID != "run-0" {
		t.Fatalf("fallback retained runs = %v", runs)
	}
}

func TestSweepRetentionArchiveBatchFailureFallsBack(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ldb, err := logdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ldb.Close()
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	logs.AttachDB(ldb)
	def, err := st.PutDefinition(t.Context(), model.Definition{Name: "archive-fallback", Kind: model.KindJob,
		Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		id := fmt.Sprintf("run-%d", i)
		at := base.Add(time.Duration(i) * time.Second)
		if err := st.CreateRun(t.Context(), model.Run{ID: id, DefinitionID: def.ID, Job: def.Name,
			Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: at, EndedAt: &at}); err != nil {
			t.Fatal(err)
		}
		if err := ldb.PutChunks(t.Context(), id, def.Name, def.Kind, at,
			[]logdb.Chunk{{Number: 1, First: 1, Last: 1, RawBytes: 32, Blob: []byte("compressed")}}); err != nil {
			t.Fatal(err)
		}
	}
	control, err := sql.Open("sqlite", filepath.Join(dir, "minicron-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.ExecContext(t.Context(), `CREATE TRIGGER abort_archive_oldest BEFORE DELETE ON log_runs
		WHEN OLD.run_id='run-0' BEGIN SELECT RAISE(ABORT, 'injected archive deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: &config.Config{Storage: config.Storage{KeepRunsDefault: 1, KeepForDefault: 30, AuditKeep: 1}}, store: st, logs: logs}
	d.sweepRetention(t.Context())
	runs, err := st.Runs(t.Context(), def.Name, 10)
	if err != nil || len(runs) != 2 || runs[0].ID != "run-2" || runs[1].ID != "run-0" {
		t.Fatalf("retained runs = %v: %v", runs, err)
	}
	archivedRuns, _, _, err := ldb.Stats(t.Context())
	if err != nil || archivedRuns != 2 {
		t.Fatalf("retained archive runs = %d: %v", archivedRuns, err)
	}
}
