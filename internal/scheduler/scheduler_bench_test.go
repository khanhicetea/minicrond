package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func BenchmarkNextFire(b *testing.B) {
	d := model.Definition{Schedule: "*/5 9-17 * * 1-5", Timezone: "UTC"}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for b.Loop() {
		if _, err := nextFire(d, after, time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
}

// Run with -benchtime=1x. Each case spans a full 30-second clock check.
func BenchmarkSchedulerIdle(b *testing.B) {
	for _, jobs := range []int{0, 100} {
		b.Run(fmt.Sprintf("jobs_%d", jobs), func(b *testing.B) {
			if b.N != 1 {
				b.Skip("run with -benchtime=1x")
			}
			dir := b.TempDir()
			st, err := store.Open(b.Context(), dir)
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			for i := range jobs {
				d := model.Definition{Name: fmt.Sprintf("job-%03d", i), Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Schedule: "0 0 1 1 *", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
				if _, err := st.PutDefinition(b.Context(), d, 0, "bench"); err != nil {
					b.Fatal(err)
				}
			}
			defs, err := st.Definitions(b.Context())
			if err != nil {
				b.Fatal(err)
			}
			s := New(st, nil)
			defer s.Stop()
			if err := s.Reload(b.Context(), defs); err != nil {
				b.Fatal(err)
			}
			if jobs > 0 {
				deadline := time.Now().Add(10 * time.Second)
				for {
					next, err := st.ScheduleNextBatch(b.Context())
					if err != nil {
						b.Fatal(err)
					}
					if len(next) == jobs {
						break
					}
					if time.Now().After(deadline) {
						b.Fatal("scheduler did not initialize all jobs")
					}
					time.Sleep(time.Millisecond)
				}
			}
			walPath := filepath.Join(dir, "minicron.db-wal")
			walSize := func() int64 {
				info, err := os.Stat(walPath)
				if err == nil {
					return info.Size()
				}
				if os.IsNotExist(err) {
					return 0
				}
				b.Fatal(err)
				return 0
			}
			runtime.GC()
			var before, after syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
				b.Fatal(err)
			}
			beforeWAL := walSize()
			b.ResetTimer()
			for b.Loop() {
				time.Sleep(31 * time.Second)
			}
			b.StopTimer()
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
				b.Fatal(err)
			}
			var heap runtime.MemStats
			runtime.ReadMemStats(&heap)
			cpuUS := (after.Utime.Sec-before.Utime.Sec+after.Stime.Sec-before.Stime.Sec)*1_000_000 + after.Utime.Usec - before.Utime.Usec + after.Stime.Usec - before.Stime.Usec
			b.ReportMetric(float64(cpuUS)/31_000, "cpu-ms/s")
			b.ReportMetric(float64(heap.HeapAlloc)/(1<<20), "heap-MiB")
			b.ReportMetric(float64(walSize()-beforeWAL), "wal-B")
		})
	}
}

func BenchmarkNextFireAnnual(b *testing.B) {
	for _, zone := range []string{"UTC", "America/New_York"} {
		b.Run(zone, func(b *testing.B) {
			d := model.Definition{Schedule: "0 0 1 1 *", Timezone: zone}
			c, err := compileSchedule(d)
			if err != nil {
				b.Fatal(err)
			}
			after := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := c.nextFire(after, time.Time{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkReloadUnchanged(b *testing.B) {
	benchmarkReload(b, false)
}

func BenchmarkReloadChanged(b *testing.B) {
	benchmarkReload(b, true)
}

func benchmarkReload(b *testing.B, changed bool) {
	st, err := store.Open(b.Context(), b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	for i := range 100 {
		d := model.Definition{Name: fmt.Sprintf("job-%03d", i), Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Schedule: "0 0 1 1 *", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
		if _, err := st.PutDefinition(b.Context(), d, 0, "bench"); err != nil {
			b.Fatal(err)
		}
	}
	defs, err := st.Definitions(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	s := New(st, nil)
	defer s.Stop()
	if err := s.Reload(b.Context(), defs); err != nil {
		b.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		next, err := st.ScheduleNextBatch(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		if len(next) == len(defs) {
			break
		}
		if time.Now().After(deadline) {
			b.Fatal("scheduler did not initialize all jobs")
		}
		time.Sleep(time.Millisecond)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if changed {
			for i := range defs {
				defs[i].Revision++
			}
		}
		if err := s.Reload(b.Context(), defs); err != nil {
			b.Fatal(err)
		}
	}
}
