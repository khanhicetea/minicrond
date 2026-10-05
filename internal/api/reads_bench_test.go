package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
	"github.com/khanhicetea/minicrond/internal/supervisor"
)

// BenchmarkMetricsEndpointParallel measures many simultaneous dashboards
// asking for the same view. "shared" is the endpoint: one computation per burst
// whose result is reused for the TTL. "uncoalesced" runs the store aggregation
// and JSON encoding once per request, as the endpoint did before coalescing.
func BenchmarkMetricsEndpointParallel(b *testing.B) {
	dir := b.TempDir()
	st, err := store.Open(b.Context(), dir)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		b.Fatal(err)
	}
	ex := executor.New(st, logs, executor.Options{MaxConcurrentRuns: 4})
	b.Cleanup(func() { ex.Shutdown(context.Background()) })
	callback := func(context.Context) error { return nil }
	s := New(st, logs, ex, supervisor.New(st, ex), callback, callback, "bench")
	def, err := st.PutDefinition(b.Context(), model.Definition{Name: "bench", Kind: model.KindJob, Command: "true", Shell: "/bin/sh"}, 0, "bench")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range 3000 {
		at := now.Add(-time.Duration(i) * time.Second / 2)
		end := at.Add(time.Second)
		run := model.Run{ID: fmt.Sprintf("bench-%d", i), DefinitionID: def.ID, Job: "bench", Kind: def.Kind, Status: "succeeded", Trigger: "manual", QueuedAt: at, StartedAt: &at, EndedAt: &end}
		if err := st.CreateRun(b.Context(), run); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("shared", func(b *testing.B) {
		b.ReportAllocs()
		b.SetParallelism(8)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if rec := call(s, true, "GET", "/api/v1/metrics/runs?range=1h", "", "", nil); rec.Code != 200 {
					b.Errorf("metrics: %d", rec.Code)
					return
				}
			}
		})
	})
	b.Run("uncoalesced", func(b *testing.B) {
		b.ReportAllocs()
		b.SetParallelism(8)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				at := time.Now()
				stats, err := st.RunMetrics(b.Context(), at.Add(-time.Hour), at, 48)
				if err != nil {
					b.Errorf("metrics: %v", err)
					return
				}
				if err := json.NewEncoder(io.Discard).Encode(stats); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}
