//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
)

// Run with -benchtime=1x. This uses the real Unix API, executor, log archive,
// metrics query, and run-list query while eight clients submit 128 jobs.
func BenchmarkDaemonLoaded(b *testing.B) {
	benchmarkDaemonLoaded(b, 0)
}

// The second case adds a 30-day dashboard window with 30,000 older runs and
// 500 recent runs before submitting the same loaded workload.
func BenchmarkDaemonLoadedHistory(b *testing.B) {
	benchmarkDaemonLoaded(b, 30_000)
}

func benchmarkDaemonLoaded(b *testing.B, olderRuns int) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	dir := b.TempDir()
	configPath := filepath.Join(dir, "minicron.toml")
	if err := os.WriteFile(configPath, []byte("[server]\ntcp_enabled=false\n"), 0o600); err != nil {
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
	readyBy := time.Now().Add(10 * time.Second)
	for {
		d.mu.Lock()
		ready := d.running
		d.mu.Unlock()
		if ready {
			break
		}
		select {
		case err := <-done:
			stopped = true
			b.Fatalf("daemon stopped before ready: %v", err)
		default:
		}
		if time.Now().After(readyBy) {
			b.Fatal("daemon did not initialize")
		}
		time.Sleep(time.Millisecond)
	}
	// Wait for startup maintenance before measuring the loaded period.
	time.Sleep(100 * time.Millisecond)
	socket := filepath.Join(dataDir, "minicron.sock")
	transport := &http.Transport{MaxIdleConns: 32, MaxIdleConnsPerHost: 32, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	request := func(method, path string, body []byte) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(b.Context(), method, "http://minicron"+path, bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return data, resp.StatusCode, err
	}
	job := model.Definition{Name: "loaded", Kind: model.KindJob, OnOverlap: "parallel",
		Command: `i=0; while [ "$i" -lt 32 ]; do printf '%01024d\n' "$i"; i=$((i+1)); done`}
	definition, err := json.Marshal(job)
	if err != nil {
		b.Fatal(err)
	}
	data, status, err := request(http.MethodPost, "/api/v1/jobs", definition)
	if err != nil || status != http.StatusOK {
		b.Fatalf("create job: status=%d error=%v body=%s", status, err, data)
	}
	metricsRange := "1h"
	if olderRuns > 0 {
		def, _, err := d.store.Definition(b.Context(), job.Name)
		if err != nil {
			b.Fatal(err)
		}
		now := time.Now().UTC()
		for i := range olderRuns + 500 {
			at := now.Add(-60 * 24 * time.Hour)
			if i >= olderRuns {
				at = now.Add(-24 * time.Hour)
			}
			at = at.Add(time.Duration(i) * time.Microsecond)
			run := model.Run{ID: fmt.Sprintf("history-%05d", i), DefinitionID: def.ID, Job: def.Name,
				Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: at, EndedAt: &at}
			if err := d.store.CreateRun(b.Context(), run); err != nil {
				b.Fatal(err)
			}
		}
		metricsRange = "30d"
	}
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
	beforeWrite, haveWrite := linuxProcessWriteBytes()
	runtime.GC()
	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		b.Fatal(err)
	}
	type result struct {
		kind    string
		elapsed time.Duration
		err     error
	}
	results := make(chan result, 256)
	start := make(chan struct{})
	var workers sync.WaitGroup
	b.ResetTimer()
	started := time.Now()
	for range 8 {
		workers.Go(func() {
			<-start
			for range 16 {
				began := time.Now()
				data, status, err := request(http.MethodPost, "/api/v1/jobs/loaded/trigger?wait=true&timeout=10", nil)
				if err == nil && status == http.StatusOK {
					var run model.Run
					err = json.Unmarshal(data, &run)
					if err == nil && run.Status != "succeeded" {
						err = fmt.Errorf("run %s ended %s", run.ID, run.Status)
					}
				} else if err == nil {
					err = fmt.Errorf("trigger status %d: %s", status, data)
				}
				results <- result{kind: "run", elapsed: time.Since(began), err: err}
			}
		})
	}
	workers.Go(func() {
		<-start
		for range 64 {
			for _, endpoint := range []struct{ kind, path string }{
				{"metrics", "/api/v1/metrics/runs?range=" + metricsRange},
				{"list", "/api/v1/runs?limit=50"},
			} {
				began := time.Now()
				data, status, err := request(http.MethodGet, endpoint.path, nil)
				if err == nil && status != http.StatusOK {
					err = fmt.Errorf("GET %s status %d: %s", endpoint.path, status, data)
				}
				results <- result{kind: endpoint.kind, elapsed: time.Since(began), err: err}
			}
		}
	})
	close(start)
	workers.Wait()
	b.StopTimer()
	duration := time.Since(started)
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		b.Fatal(err)
	}
	afterWrite, haveAfterWrite := linuxProcessWriteBytes()
	close(results)
	latencies := map[string][]time.Duration{}
	for result := range results {
		if result.err != nil {
			b.Fatal(result.err)
		}
		latencies[result.kind] = append(latencies[result.kind], result.elapsed)
	}
	for kind, samples := range latencies {
		slices.Sort(samples)
		p95 := samples[(95*len(samples)-1)/100]
		b.ReportMetric(float64(p95.Microseconds())/1000, kind+"-p95-ms")
	}
	if len(latencies["run"]) != 128 || len(latencies["metrics"]) != 64 || len(latencies["list"]) != 64 {
		b.Fatalf("incomplete samples: %v", latencies)
	}
	// Finished runs are archived in the background.
	var runs, chunks int64
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		runs, chunks, _, err = d.ldb.Stats(b.Context())
		if err != nil || runs == 128 || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || runs != 128 || chunks < 128 {
		b.Fatalf("archive has %d runs and %d chunks: %v", runs, chunks, err)
	}
	cpuUS := (after.Utime.Sec-before.Utime.Sec+after.Stime.Sec-before.Stime.Sec)*1_000_000 + after.Utime.Usec - before.Utime.Usec + after.Stime.Usec - before.Stime.Usec
	b.ReportMetric(float64(cpuUS)/float64(duration.Microseconds())*1000, "cpu-ms/s")
	b.ReportMetric(128/duration.Seconds(), "runs/s")
	b.ReportMetric(float64(walSize(mainWAL)+walSize(logWAL)-beforeWAL)/128, "wal-B/run")
	if haveWrite && haveAfterWrite && afterWrite > beforeWrite {
		b.ReportMetric(float64(afterWrite-beforeWrite)/128, "write-B/run")
	}
	b.ReportMetric(float64(after.Maxrss)/1024, "rss-MiB")
	runtime.GC()
	var heap runtime.MemStats
	runtime.ReadMemStats(&heap)
	b.ReportMetric(float64(heap.HeapAlloc)/(1<<20), "heap-MiB")
}

// /proc/self/io is Linux process I/O accounting, including database and log
// file writes made by the daemon. It is omitted when procfs is unavailable.
func linuxProcessWriteBytes() (int64, bool) {
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "write_bytes:"); ok {
			bytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			return bytes, err == nil
		}
	}
	return 0, false
}

// Run with -benchtime=1x. Each of 64 API readers replays the same 4 MiB
// archived log through SSE, then receives backlog_done and done.
func BenchmarkDaemonArchivedSSE64(b *testing.B) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	const runID = "01800000-0000-7000-8000-000000000001"
	dir := b.TempDir()
	configPath := filepath.Join(dir, "minicron.toml")
	if err := os.WriteFile(configPath, []byte("[server]\ntcp_enabled=false\n"), 0o600); err != nil {
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
	readyBy := time.Now().Add(10 * time.Second)
	for {
		d.mu.Lock()
		ready := d.running
		d.mu.Unlock()
		if ready {
			break
		}
		select {
		case err := <-done:
			stopped = true
			b.Fatalf("daemon stopped before ready: %v", err)
		default:
		}
		if time.Now().After(readyBy) {
			b.Fatal("daemon did not initialize")
		}
		time.Sleep(time.Millisecond)
	}
	def, err := d.store.PutDefinition(b.Context(), model.Definition{Name: "sse", Kind: model.KindJob,
		Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}, 0, "bench")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	if err := d.store.CreateRun(b.Context(), model.Run{ID: runID, DefinitionID: def.ID, Job: def.Name,
		Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: now, EndedAt: &now}); err != nil {
		b.Fatal(err)
	}
	w, err := d.logs.Open(runID, def.Name, def.Kind, logstore.WriterOptions{MaxBytes: 100 << 20, MaxLine: 4096})
	if err != nil {
		b.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 4096)
	for range 1024 {
		if err := w.Write(logstore.Stdout, payload, 0); err != nil {
			b.Fatal(err)
		}
	}
	if err := d.logs.Close(runID); err != nil {
		b.Fatal(err)
	}
	runs, chunks, _, err := d.ldb.Stats(b.Context())
	if err != nil || runs != 1 || chunks < 4 {
		b.Fatalf("archive has %d runs and %d chunks: %v", runs, chunks, err)
	}
	socket := filepath.Join(dataDir, "minicron.sock")
	transport := &http.Transport{MaxIdleConns: 64, MaxIdleConnsPerHost: 64, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	runtime.GC()
	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		b.Fatal(err)
	}
	latencies := make(chan time.Duration, 64)
	errs := make(chan error, 64)
	start := make(chan struct{})
	var readers sync.WaitGroup
	b.ResetTimer()
	started := time.Now()
	for range 64 {
		readers.Go(func() {
			<-start
			began := time.Now()
			req, err := http.NewRequestWithContext(b.Context(), http.MethodGet, "http://minicron/api/v1/runs/"+runID+"/log/stream", nil)
			if err != nil {
				errs <- err
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("SSE status %d", resp.StatusCode)
				return
			}
			var lines, backlogDone, doneEvents int
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				switch data := scanner.Bytes(); {
				case bytes.Equal(data, []byte("event: line")):
					lines++
				case bytes.Equal(data, []byte("event: backlog_done")):
					backlogDone++
				case bytes.Equal(data, []byte("event: done")):
					doneEvents++
				}
			}
			if err := scanner.Err(); err != nil {
				errs <- err
				return
			}
			if lines != 1024 || backlogDone != 1 || doneEvents != 1 {
				errs <- fmt.Errorf("SSE events: lines=%d backlog_done=%d done=%d", lines, backlogDone, doneEvents)
				return
			}
			latencies <- time.Since(began)
		})
	}
	close(start)
	readers.Wait()
	b.StopTimer()
	duration := time.Since(started)
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		b.Fatal(err)
	}
	close(errs)
	for err := range errs {
		b.Fatal(err)
	}
	close(latencies)
	samples := make([]time.Duration, 0, 64)
	for elapsed := range latencies {
		samples = append(samples, elapsed)
	}
	if len(samples) != 64 {
		b.Fatalf("completed %d of 64 streams", len(samples))
	}
	slices.Sort(samples)
	cpuUS := (after.Utime.Sec-before.Utime.Sec+after.Stime.Sec-before.Stime.Sec)*1_000_000 + after.Utime.Usec - before.Utime.Usec + after.Stime.Usec - before.Stime.Usec
	b.ReportMetric(float64(cpuUS)/float64(duration.Microseconds())*1000, "cpu-ms/s")
	b.ReportMetric(float64(samples[60].Microseconds())/1000, "sse-p95-ms")
	b.ReportMetric(float64(64*4)/duration.Seconds(), "MiB/s")
	b.ReportMetric(float64(after.Maxrss)/1024, "rss-MiB")
	runtime.GC()
	var heap runtime.MemStats
	runtime.ReadMemStats(&heap)
	b.ReportMetric(float64(heap.HeapAlloc)/(1<<20), "heap-MiB")
}
