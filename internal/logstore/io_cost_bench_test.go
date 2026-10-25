package logstore

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/model"
)

// TestIOCostPerRun measures the disk cost of storing run logs: block-device
// flushes (a system-wide fsync proxy, as in docs/benchmarks) and bytes this
// process sent to storage, per run. It needs a real disk and a quiet host, so
// it runs only when MINICRON_IO_BENCH_DIR names a directory on that disk and
// MINICRON_IO_BENCH_DEV the block device (for example sda).
// MINICRON_IO_BENCH_CORPUS may name a text file whose lines are used as output.
//
// "split" mode stores runs with no archive attached, then archives them in a
// second step, to attribute cost to the file buffer and to the archive.
// "live" mode runs the archiver in the background as the daemon does.
func TestIOCostPerRun(t *testing.T) {
	root, dev := os.Getenv("MINICRON_IO_BENCH_DIR"), os.Getenv("MINICRON_IO_BENCH_DEV")
	if root == "" || dev == "" {
		t.Skip("set MINICRON_IO_BENCH_DIR and MINICRON_IO_BENCH_DEV")
	}
	corpus := benchCorpus(t)
	type scenario struct {
		name  string
		runs  int
		par   int
		lines int // stdout lines per run
	}
	scenarios := []scenario{
		{"short (3 lines)", 300, 10, 3},
		{"silent (exit line only)", 300, 10, 0},
		{"large (~4 MiB raw)", 6, 2, 36000},
	}
	reps, _ := strconv.Atoi(os.Getenv("MINICRON_IO_BENCH_REPS"))
	reps = max(reps, 1)
	for _, sc := range scenarios {
		for _, mode := range []string{"split", "live"} {
			for rep := range reps {
				dir := filepath.Join(root, fmt.Sprintf("io-%d", time.Now().UnixNano()))
				res := runIOScenario(t, dir, dev, mode, sc.runs, sc.par, sc.lines, corpus)
				os.RemoveAll(dir)
				for _, r := range res {
					t.Logf("%-24s %-5s rep %d  %-8s flushes/run %6.2f  write KiB/run %8.1f  writeIOs/run %6.2f",
						sc.name, mode, rep+1, r.phase, float64(r.flushes)/float64(sc.runs), float64(r.bytes)/1024/float64(sc.runs), float64(r.ios)/float64(sc.runs))
				}
			}
		}
	}
}

type ioPhase struct {
	phase               string
	flushes, ios, bytes int64
}

func benchCorpus(t *testing.T) [][]byte {
	if p := os.Getenv("MINICRON_IO_BENCH_CORPUS"); p != "" {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var out [][]byte
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			out = append(out, append([]byte(nil), sc.Bytes()...))
		}
		return out
	}
	var out [][]byte
	for i := range 5000 {
		out = append(out, fmt.Appendf(nil, "2026-10-25T12:00:%02d host app[%d]: processed request id=%d status=200 bytes=%d", i%60, 1000+i%7, i, i*37%9000))
	}
	return out
}

func ioCounters(t *testing.T, dev string) (flushes, ios, bytes int64) {
	b, err := os.ReadFile("/sys/block/" + dev + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b))
	flushes, _ = strconv.ParseInt(f[15], 10, 64)
	ios, _ = strconv.ParseInt(f[4], 10, 64)
	pio, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(pio), "\n") {
		if v, ok := strings.CutPrefix(l, "write_bytes: "); ok {
			bytes, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return
}

func runIOScenario(t *testing.T, dir, dev, mode string, runs, par, lines int, corpus [][]byte) []ioPhase {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ldb, err := logdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ldb.Close()
	s, err := New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "live" {
		s.AttachDB(ldb)
		s.StartArchiver()
	}
	// Let earlier writeback settle so it is not attributed to this phase.
	syncAndSettle()
	f0, i0, b0 := ioCounters(t, dev)
	var wg sync.WaitGroup
	sem := make(chan struct{}, par)
	for n := range runs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			id := fmt.Sprintf("run-%06d", n)
			w, err := s.Open(id, "job", model.KindJob, WriterOptions{MaxBytes: 100 << 20, MaxLine: 64 << 10})
			if err != nil {
				t.Error(err)
				return
			}
			var buf bytes.Buffer
			for i := range lines {
				buf.Write(corpus[(n*lines+i)%len(corpus)])
				buf.WriteByte('\n')
			}
			if err := w.Pipe(Stdout, io.MultiReader(&buf)); err != nil {
				t.Error(err)
			}
			if err := w.Write(System, []byte("exited with status 0 after 12ms"), 0); err != nil {
				t.Error(err)
			}
			if err := s.Seal(id); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var out []ioPhase
	if mode == "live" {
		if err := s.StopArchiver(t.Context()); err != nil {
			t.Fatal(err)
		}
		syncAndSettle()
		f1, i1, b1 := ioCounters(t, dev)
		out = append(out, ioPhase{"total", f1 - f0, i1 - i0, b1 - b0})
	} else {
		s.SyncAll()
		syncAndSettle()
		f1, i1, b1 := ioCounters(t, dev)
		out = append(out, ioPhase{"buffer", f1 - f0, i1 - i0, b1 - b0})
		s.AttachDB(ldb)
		if err := s.ArchiveOrphans(); err != nil {
			t.Fatal(err)
		}
		syncAndSettle()
		f2, i2, b2 := ioCounters(t, dev)
		out = append(out, ioPhase{"archive", f2 - f1, i2 - i1, b2 - b1})
		out = append(out, ioPhase{"total", f2 - f0, i2 - i0, b2 - b0})
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "logs")); len(entries) > 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if !(len(names) == 1 && names[0] == QuarantineDir) {
			t.Errorf("%d buffers left after archival: %v", len(names), names[:min(len(names), 5)])
		}
	}
	return out
}

// syncAndSettle forces the kernel's delayed writeback of what the step wrote,
// so the counters attribute it to that step. Files a step created and deleted
// before this point are never written back and correctly not counted. The
// sync itself adds a few flushes per phase, spread over all runs.
func syncAndSettle() {
	syscall.Sync()
	time.Sleep(time.Second)
}
