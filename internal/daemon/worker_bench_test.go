//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
)

// Run with -benchtime=1x. The writer stays active across three archive
// checkpoints, while each SSE client must receive all 2,000 frames in order.
func BenchmarkWorkerLiveSSE(b *testing.B) {
	for _, readerCount := range []int{0, 16} {
		b.Run(fmt.Sprintf("readers_%d", readerCount), func(b *testing.B) {
			benchmarkWorkerLiveSSE(b, readerCount)
		})
	}
}

func benchmarkWorkerLiveSSE(b *testing.B, readerCount int) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	const runID = "01800000-0000-7000-8000-000000000002"
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
	def, err := d.store.PutDefinition(b.Context(), model.Definition{Name: "streaming-worker", Kind: model.KindWorker,
		Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}, 0, "bench")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	if err := d.store.CreateRun(b.Context(), model.Run{ID: runID, DefinitionID: def.ID, Job: def.Name,
		Kind: def.Kind, Revision: def.Revision, Status: "running", Trigger: "manual", QueuedAt: now}); err != nil {
		b.Fatal(err)
	}
	w, err := d.logs.Open(runID, def.Name, def.Kind, logstore.WriterOptions{MaxBytes: 64 << 20, MaxLine: 1024})
	if err != nil {
		b.Fatal(err)
	}
	socket := filepath.Join(dataDir, "minicron.sock")
	transport := &http.Transport{MaxIdleConns: readerCount, MaxIdleConnsPerHost: readerCount,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	ready := make(chan struct{}, readerCount)
	errs := make(chan error, readerCount)
	var readers sync.WaitGroup
	for range readerCount {
		readers.Go(func() {
			req, err := http.NewRequestWithContext(b.Context(), http.MethodGet,
				"http://minicron/api/v1/runs/"+runID+"/log/stream", nil)
			if err != nil {
				errs <- err
				ready <- struct{}{}
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				ready <- struct{}{}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("SSE status %d", resp.StatusCode)
				ready <- struct{}{}
				return
			}
			var lines, backlogDone, doneEvents, dropped int
			var last uint64
			var sequenceErr error
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				data := scanner.Bytes()
				if value, ok := bytes.CutPrefix(data, []byte("id: ")); ok {
					sequence, err := strconv.ParseUint(string(value), 10, 64)
					if err != nil || sequence != last+1 {
						sequenceErr = fmt.Errorf("SSE sequence after %d: %q (%v)", last, value, err)
						break
					}
					last = sequence
				}
				switch {
				case bytes.Equal(data, []byte("event: line")):
					lines++
				case bytes.Equal(data, []byte("event: backlog_done")):
					backlogDone++
					ready <- struct{}{}
				case bytes.Equal(data, []byte("event: done")):
					doneEvents++
				case bytes.Equal(data, []byte("event: dropped")):
					dropped++
				}
			}
			if sequenceErr != nil {
				errs <- sequenceErr
			} else if err := scanner.Err(); err != nil {
				errs <- err
			} else if lines != 2000 || last != 2000 || backlogDone != 1 || doneEvents != 1 || dropped != 0 {
				errs <- fmt.Errorf("SSE events: lines=%d last=%d backlog_done=%d done=%d dropped=%d", lines, last, backlogDone, doneEvents, dropped)
			}
		})
	}
	for range readerCount {
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			b.Fatal("SSE reader did not finish its backlog")
		}
	}
	select {
	case err := <-errs:
		b.Fatal(err)
	default:
	}
	beforeWrite, haveWrite := linuxProcessWriteBytes()
	runtime.GC()
	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		b.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 1024)
	b.ReportAllocs()
	b.ResetTimer()
	started := time.Now()
	for i := range 2000 {
		if err := w.Write(logstore.Stdout, payload, 0); err != nil {
			b.Fatal(err)
		}
		if (i+1)%500 == 0 && i+1 < 2000 {
			if err := w.Flush(d.logs); err != nil {
				b.Fatal(err)
			}
		}
		time.Sleep(time.Millisecond)
	}
	if err := d.logs.Close(runID); err != nil {
		b.Fatal(err)
	}
	readers.Wait()
	b.StopTimer()
	duration := time.Since(started)
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		b.Fatal(err)
	}
	afterWrite, haveAfterWrite := linuxProcessWriteBytes()
	close(errs)
	for err := range errs {
		b.Fatal(err)
	}
	runs, chunks, _, err := d.ldb.Stats(b.Context())
	if err != nil || runs != 1 || chunks != 4 {
		b.Fatalf("archive has %d runs and %d chunks: %v", runs, chunks, err)
	}
	archiveReader := d.logs.NewStreamReader(runID)
	defer archiveReader.Close()
	var archived uint64
	for {
		frames, err := archiveReader.ReadContext(b.Context(), archived, 5000)
		if err != nil {
			b.Fatal(err)
		}
		if len(frames) == 0 {
			break
		}
		for _, frame := range frames {
			if frame.Sequence != archived+1 {
				b.Fatalf("archive sequence %d after %d", frame.Sequence, archived)
			}
			archived = frame.Sequence
		}
	}
	if archived != 2000 {
		b.Fatalf("archive ended at %d", archived)
	}
	archiveReader.Close()
	cpuUS := (after.Utime.Sec-before.Utime.Sec+after.Stime.Sec-before.Stime.Sec)*1_000_000 + after.Utime.Usec - before.Utime.Usec + after.Stime.Usec - before.Stime.Usec
	b.ReportMetric(2000/duration.Seconds(), "frames/s")
	b.ReportMetric(float64(cpuUS)/float64(duration.Microseconds())*1000, "cpu-ms/s")
	b.ReportMetric(float64(after.Maxrss)/1024, "rss-MiB")
	if haveWrite && haveAfterWrite && afterWrite > beforeWrite {
		b.ReportMetric(float64(afterWrite-beforeWrite)/2000, "write-B/frame")
	}
	runtime.GC()
	var heap runtime.MemStats
	runtime.ReadMemStats(&heap)
	b.ReportMetric(float64(heap.HeapAlloc)/(1<<20), "heap-MiB")
}
