package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func BenchmarkRunMetricsHistory(b *testing.B) {
	benchmarkRunMetrics(b, 30000, 500)
}

func BenchmarkDeleteRunsPage(b *testing.B) {
	const pageSize = 128
	for _, batch := range []bool{false, true} {
		name := "individual"
		if batch {
			name = "batch"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			s, err := Open(b.Context(), dir)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if _, err := s.db.ExecContext(b.Context(), "PRAGMA wal_autocheckpoint=0"); err != nil {
				b.Fatal(err)
			}
			d := model.Definition{Name: "page", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
			d, err = s.PutDefinition(b.Context(), d, 0, "bench")
			if err != nil {
				b.Fatal(err)
			}
			pages := make([][]string, b.N)
			tx, err := s.db.BeginTx(b.Context(), nil)
			if err != nil {
				b.Fatal(err)
			}
			stmt, err := tx.PrepareContext(b.Context(), `INSERT INTO runs(run_id,definition_id,job,kind,revision,definition_hash,status,trigger,queued_us,ended_us)
				VALUES(?,?,'page','job',1,'hash','succeeded','manual',?,?)`)
			if err != nil {
				b.Fatal(err)
			}
			at := time.Now().Add(-time.Hour).UnixMicro()
			for page := range pages {
				ids := make([]string, pageSize)
				for i := range ids {
					id := fmt.Sprintf("run-%d-%d", page, i)
					ids[i] = id
					if _, err := stmt.ExecContext(b.Context(), id, d.ID, at, at); err != nil {
						b.Fatal(err)
					}
				}
				pages[page] = ids
			}
			if err := stmt.Close(); err != nil {
				b.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			walPath := filepath.Join(dir, "minicron.db-wal")
			walSize := func() int64 {
				info, err := os.Stat(walPath)
				if err != nil {
					b.Fatal(err)
				}
				return info.Size()
			}
			before := walSize()
			b.ReportAllocs()
			b.ResetTimer()
			for page := range b.N {
				if batch {
					if err := s.DeleteRuns(b.Context(), pages[page]); err != nil {
						b.Fatal(err)
					}
				} else {
					for _, id := range pages[page] {
						if err := s.DeleteRun(b.Context(), id); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(walSize()-before)/float64(b.N*pageSize), "wal-B/run")
		})
	}
}

func BenchmarkRunMetricsWindow(b *testing.B) {
	benchmarkRunMetrics(b, 0, 30000)
}

func benchmarkRunMetrics(b *testing.B, oldCount, recentCount int) {
	s, err := Open(b.Context(), b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	d := model.Definition{Name: "metric", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
	d, err = s.PutDefinition(b.Context(), d, 0, "bench")
	if err != nil {
		b.Fatal(err)
	}
	tx, err := s.db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.PrepareContext(b.Context(), `INSERT INTO runs(run_id,definition_id,job,kind,revision,definition_hash,status,trigger,queued_us,started_us,ended_us)
		VALUES(?,?,?,'job',1,'hash','succeeded','manual',?,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for i := range oldCount + recentCount {
		queued := now.Add(-60 * 24 * time.Hour)
		if i >= oldCount {
			queued = now.Add(-time.Duration(i-oldCount) * time.Second)
		}
		us := queued.UnixMicro()
		if _, err := stmt.ExecContext(b.Context(), fmt.Sprintf("run-%d", i), d.ID, d.Name, us, us+1000, us+2000); err != nil {
			b.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	plan, err := s.db.QueryContext(b.Context(), `EXPLAIN QUERY PLAN SELECT job,status,queued_us,started_us,ended_us FROM runs WHERE queued_us>=? OR ended_us>=? OR status IN ('pending','running')`, now.Add(-24*time.Hour).UnixMicro(), now.Add(-24*time.Hour).UnixMicro())
	if err != nil {
		b.Fatal(err)
	}
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			b.Fatal(err)
		}
		b.Log(detail)
	}
	if err := plan.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		metrics, err := s.RunMetrics(b.Context(), now.Add(-24*time.Hour), now, 48)
		if err != nil || metrics.Total != recentCount {
			b.Fatalf("metrics total=%d err=%v", metrics.Total, err)
		}
	}
}

func BenchmarkRunTransitionIndexCost(b *testing.B) {
	benchmarkRunTransitionIndexCost(b, "idx_runs_end_time", "end_index")
}

func BenchmarkRunTransitionRetentionIndexCost(b *testing.B) {
	benchmarkRunTransitionIndexCost(b, "idx_runs_retention", "retention_index")
}

func benchmarkRunTransitionIndexCost(b *testing.B, indexName, label string) {
	for _, withIndex := range []bool{false, true} {
		name := "without_" + label
		if withIndex {
			name = "with_" + label
		}
		b.Run(name, func(b *testing.B) {
			s, err := Open(b.Context(), b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if !withIndex {
				if _, err := s.db.ExecContext(b.Context(), "DROP INDEX "+indexName); err != nil {
					b.Fatal(err)
				}
			}
			d := model.Definition{Name: "job", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
			d, err = s.PutDefinition(b.Context(), d, 0, "bench")
			if err != nil {
				b.Fatal(err)
			}
			at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
			i := 0
			beforeUsed := benchmarkUsedPageBytes(b, s)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				run := model.Run{ID: fmt.Sprintf("run-%d", i), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: "hash", Status: "pending", Trigger: "manual", QueuedAt: at}
				if err := s.CreateRun(b.Context(), run); err != nil {
					b.Fatal(err)
				}
				if err := s.FinishRun(b.Context(), run.ID, "succeeded", "exit", nil, "", at.Add(time.Second), 0, false); err != nil {
					b.Fatal(err)
				}
				i++
			}
			b.StopTimer()
			b.ReportMetric(float64(benchmarkUsedPageBytes(b, s)-beforeUsed)/float64(b.N), "db-B/run")
		})
	}
}

// Run with -benchtime=1x. The timed Open applies migration 8 to a database
// with all earlier schema changes already present.
func BenchmarkEndTimeIndexMigration(b *testing.B) {
	for _, rows := range []int{0, 30000} {
		b.Run(fmt.Sprintf("rows_%d", rows), func(b *testing.B) {
			if b.N != 1 {
				b.Skip("run with -benchtime=1x")
			}
			dir := b.TempDir()
			s, err := Open(b.Context(), dir)
			if err != nil {
				b.Fatal(err)
			}
			def, err := s.PutDefinition(b.Context(), model.Definition{Name: "history", Kind: model.KindJob,
				Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}, 0, "bench")
			if err != nil {
				b.Fatal(err)
			}
			tx, err := s.db.BeginTx(b.Context(), nil)
			if err != nil {
				b.Fatal(err)
			}
			stmt, err := tx.PrepareContext(b.Context(), `INSERT INTO runs(run_id,definition_id,job,kind,revision,definition_hash,status,trigger,queued_us,ended_us)
				VALUES(?,?,'history','job',1,'hash','succeeded','manual',?,?)`)
			if err != nil {
				b.Fatal(err)
			}
			at := time.Now().Add(-24 * time.Hour).UnixMicro()
			for i := range rows {
				if _, err := stmt.ExecContext(b.Context(), fmt.Sprintf("run-%d", i), def.ID, at+int64(i), at+int64(i)); err != nil {
					b.Fatal(err)
				}
			}
			if err := stmt.Close(); err != nil {
				b.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			if _, err := s.db.ExecContext(b.Context(), "DROP INDEX idx_runs_end_time"); err != nil {
				b.Fatal(err)
			}
			if _, err := s.db.ExecContext(b.Context(), "DROP TABLE exec_queue"); err != nil {
				b.Fatal(err)
			}
			if _, err := s.db.ExecContext(b.Context(), "PRAGMA user_version=7"); err != nil {
				b.Fatal(err)
			}
			beforeUsed := benchmarkUsedPageBytes(b, s)
			if err := s.Close(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			s, err = Open(b.Context(), dir)
			b.StopTimer()
			if err != nil {
				b.Fatal(err)
			}
			if version, err := s.SchemaVersion(b.Context()); err != nil || version != SchemaVersion {
				b.Fatalf("schema version = %d, %v", version, err)
			}
			afterUsed := benchmarkUsedPageBytes(b, s)
			if err := s.Close(); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(afterUsed-beforeUsed), "index-B")
		})
	}
}

func benchmarkUsedPageBytes(b *testing.B, s *Store) int64 {
	b.Helper()
	var pages, free, pageSize int64
	for _, query := range []struct {
		text string
		out  *int64
	}{
		{"PRAGMA page_count", &pages},
		{"PRAGMA freelist_count", &free},
		{"PRAGMA page_size", &pageSize},
	} {
		if err := s.db.QueryRowContext(b.Context(), query.text).Scan(query.out); err != nil {
			b.Fatal(err)
		}
	}
	return (pages - free) * pageSize
}
