//go:build linux

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Run with -benchtime=1x. Each case spans a full scheduler clock check.
func BenchmarkDaemonIdle(b *testing.B) {
	for _, jobs := range []int{0, 100} {
		b.Run(fmt.Sprintf("jobs_%d", jobs), func(b *testing.B) {
			if b.N != 1 {
				b.Skip("run with -benchtime=1x")
			}
			dir := b.TempDir()
			var cfg strings.Builder
			cfg.WriteString("[server]\ntcp_enabled=false\n")
			for i := range jobs {
				fmt.Fprintf(&cfg, "\n[[job]]\nname='job-%03d'\ncommand='true'\nschedule='0 0 1 1 *'\ntimezone='UTC'\n", i)
			}
			configPath := filepath.Join(dir, "minicron.toml")
			if err := os.WriteFile(configPath, []byte(cfg.String()), 0o600); err != nil {
				b.Fatal(err)
			}
			dataDir := filepath.Join(dir, "data")
			d := &Daemon{ConfigPath: configPath, DataDir: dataDir}
			ctx, cancel := context.WithCancel(b.Context())
			done := make(chan error, 1)
			go func() { done <- d.Run(ctx) }()
			stopped := false
			defer func() {
				cancel()
				if !stopped {
					if err := <-done; err != nil {
						b.Error(err)
					}
				}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				d.mu.Lock()
				ready := d.running
				st := d.store
				d.mu.Unlock()
				if ready {
					if jobs == 0 {
						break
					}
					next, err := st.ScheduleNextBatch(b.Context())
					if err != nil {
						b.Fatal(err)
					}
					if len(next) == jobs {
						break
					}
				}
				select {
				case err := <-done:
					stopped = true
					b.Fatalf("daemon stopped before ready: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					b.Fatal("daemon did not initialize")
				}
				time.Sleep(time.Millisecond)
			}
			// Let the one-time maintenance sweep finish before measuring idle work.
			time.Sleep(100 * time.Millisecond)
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
			mainWAL := filepath.Join(dataDir, "minicron.db-wal")
			logWAL := filepath.Join(dataDir, "minicron-logs.db-wal")
			beforeWAL := walSize(mainWAL) + walSize(logWAL)
			runtime.GC()
			var before, after syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for range b.N {
				time.Sleep(31 * time.Second)
			}
			b.StopTimer()
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
				b.Fatal(err)
			}
			cpuUS := (after.Utime.Sec-before.Utime.Sec+after.Stime.Sec-before.Stime.Sec)*1_000_000 + after.Utime.Usec - before.Utime.Usec + after.Stime.Usec - before.Stime.Usec
			b.ReportMetric(float64(cpuUS)/31_000, "cpu-ms/s")
			b.ReportMetric(float64(walSize(mainWAL)+walSize(logWAL)-beforeWAL), "wal-B")
			b.ReportMetric(float64(after.Maxrss)/1024, "rss-MiB")
			runtime.GC()
			var heap runtime.MemStats
			runtime.ReadMemStats(&heap)
			b.ReportMetric(float64(heap.HeapAlloc)/(1<<20), "heap-MiB")
		})
	}
}
