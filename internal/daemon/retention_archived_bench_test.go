//go:build linux

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// Run with -benchtime=1x; setup creates 2,000 terminal runs with one archived
// chunk each. The benchmark times the real daemon retention sweep.
func BenchmarkSweepRetentionArchived(b *testing.B) {
	benchmarkSweepRetentionArchived(b, 2000, []byte("compressed"))
}

// Run with -benchtime=1x; setup creates 128 terminal runs with a 1 MiB archive
// chunk each, exercising one full retention page of large logs.
func BenchmarkSweepRetentionLargeArchive(b *testing.B) {
	benchmarkSweepRetentionArchived(b, 128, make([]byte, 1<<20))
}

func benchmarkSweepRetentionArchived(b *testing.B, runCount int, blob []byte) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	dir := b.TempDir()
	st, err := store.Open(b.Context(), dir)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	ldb, err := logdb.Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer ldb.Close()
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		b.Fatal(err)
	}
	logs.AttachDB(ldb)
	def, err := st.PutDefinition(b.Context(), model.Definition{Name: "retained", Kind: model.KindJob,
		Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}, 0, "bench")
	if err != nil {
		b.Fatal(err)
	}
	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := range runCount {
		id := fmt.Sprintf("run-%04d", i)
		at := base.Add(time.Duration(i) * time.Second)
		if err := st.CreateRun(b.Context(), model.Run{ID: id, DefinitionID: def.ID, Job: def.Name,
			Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: at, EndedAt: &at}); err != nil {
			b.Fatal(err)
		}
		chunk := logdb.Chunk{Number: 1, First: 1, Last: 1, RawBytes: int64(len(blob)), Blob: blob}
		if err := ldb.PutChunks(b.Context(), id, def.Name, def.Kind, at, []logdb.Chunk{chunk}); err != nil {
			b.Fatal(err)
		}
	}
	d := &Daemon{cfg: &config.Config{Storage: config.Storage{KeepRunsDefault: 1, KeepForDefault: 30, AuditKeep: 1}},
		store: st, logs: logs}
	walSize := func(path string) int64 {
		info, err := os.Stat(path)
		if err == nil {
			return info.Size()
		}
		if os.IsNotExist(err) {
			return 0
		}
		b.Fatal(err)
		return 0
	}
	beforeWAL := walSize(filepath.Join(dir, "minicron.db-wal")) + walSize(filepath.Join(dir, "minicron-logs.db-wal"))
	beforeWrite, haveWrite := linuxProcessWriteBytes()
	b.SetBytes(int64((runCount - 1) * len(blob)))
	b.ReportAllocs()
	b.ResetTimer()
	d.sweepRetention(b.Context())
	b.StopTimer()
	afterWrite, haveAfterWrite := linuxProcessWriteBytes()
	runs, err := st.Runs(b.Context(), def.Name, 10)
	if err != nil || len(runs) != 1 || runs[0].ID != fmt.Sprintf("run-%04d", runCount-1) {
		b.Fatalf("retained runs = %v: %v", runs, err)
	}
	archivedRuns, chunks, _, err := ldb.Stats(b.Context())
	if err != nil || archivedRuns != 1 || chunks != 1 {
		b.Fatalf("retained archive = %d runs, %d chunks: %v", archivedRuns, chunks, err)
	}
	b.ReportMetric(float64(walSize(filepath.Join(dir, "minicron.db-wal"))+walSize(filepath.Join(dir, "minicron-logs.db-wal"))-beforeWAL)/float64(runCount-1), "wal-B/run")
	if haveWrite && haveAfterWrite && afterWrite > beforeWrite {
		b.ReportMetric(float64(afterWrite-beforeWrite)/float64(runCount-1), "write-B/run")
	}
}
