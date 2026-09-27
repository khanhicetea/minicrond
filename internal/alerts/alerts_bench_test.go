//go:build linux

package alerts

import (
	"context"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// Run with -benchtime=1x. The 31-second sample spans what would have been
// 310 wakeups under the former 100 ms ticker.
func BenchmarkDispatcherIdle(b *testing.B) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	d, err := New(nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := d.Close(context.Background()); err != nil {
			b.Error(err)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	time.Sleep(31 * time.Second)
	b.StopTimer()
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		b.Fatal(err)
	}
	cpuUS := (after.Utime.Sec-before.Utime.Sec+after.Stime.Sec-before.Stime.Sec)*1_000_000 + after.Utime.Usec - before.Utime.Usec + after.Stime.Usec - before.Stime.Usec
	b.ReportMetric(float64(cpuUS)/31_000, "cpu-ms/s")
	runtime.GC()
	var heap runtime.MemStats
	runtime.ReadMemStats(&heap)
	b.ReportMetric(float64(heap.HeapAlloc)/(1<<20), "heap-MiB")
}
