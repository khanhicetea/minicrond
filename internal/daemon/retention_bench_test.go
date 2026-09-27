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

// Run with -benchtime=1x; setup creates 2,000 terminal runs.
func BenchmarkSweepRetention(b *testing.B) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	dir := b.TempDir()
	st, err := store.Open(b.Context(), dir)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		b.Fatal(err)
	}
	def, err := st.PutDefinition(b.Context(), model.Definition{Name: "retained", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}, 0, "bench")
	if err != nil {
		b.Fatal(err)
	}
	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := range 2000 {
		at := base.Add(time.Duration(i) * time.Second)
		if err := st.CreateRun(b.Context(), model.Run{ID: fmt.Sprintf("run-%04d", i), DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: at, EndedAt: &at}); err != nil {
			b.Fatal(err)
		}
	}
	d := &Daemon{cfg: &config.Config{Storage: config.Storage{KeepRunsDefault: 1, KeepForDefault: 30, AuditKeep: 1}}, store: st, logs: logs}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		d.sweepRetention(b.Context())
	}
	b.StopTimer()
	runs, err := st.Runs(b.Context(), def.Name, 10)
	if err != nil || len(runs) != 1 {
		b.Fatalf("retained %d runs, error %v", len(runs), err)
	}
}
