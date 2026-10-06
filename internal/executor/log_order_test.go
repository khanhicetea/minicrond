package executor

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
)

func assertStartupFirst(t *testing.T, logs *logstore.Store, id string) {
	t.Helper()
	frames, err := logs.Read(id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("frames = %+v, want startup and both output streams", frames)
	}
	if frames[0].Stream != logstore.System || !strings.HasPrefix(string(frames[0].Payload), "process started as ") {
		t.Fatalf("first frame = %+v, want process-start system line", frames[0])
	}
	seen := make(map[logstore.Stream]string)
	for _, frame := range frames[1:] {
		seen[frame.Stream] = string(frame.Payload)
	}
	if seen[logstore.Stdout] != "out" || seen[logstore.Stderr] != "err" {
		t.Fatalf("output = %v, want stdout=out stderr=err", seen)
	}
}

func TestStartupLogPrecedesImmediateOutput(t *testing.T) {
	st, logs, s := failureService(t, Options{})
	for _, kind := range []string{model.KindJob, model.KindWorker} {
		t.Run(kind, func(t *testing.T) {
			d := model.Definition{Name: "immediate-" + kind, Kind: kind, Shell: "/bin/sh", Command: "echo out; echo err >&2", SuccessCodes: []int{0}}
			for range 20 {
				r := admitFailureRun(t, st, s, d)
				if got := waitFailureRun(t, st, s, r.ID); got.Status != "succeeded" {
					t.Fatalf("run = %s, want succeeded", got.Status)
				}
				assertStartupFirst(t, logs, r.ID)
			}
		})
	}
}

// Hold the system write until the child has emitted both streams. Neither
// pump may store output first, even when the startup goroutine runs late.
func TestStartupLogGatesBothOutputStreams(t *testing.T) {
	_, st, s := resilienceService(t, Options{})
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	t.Cleanup(release)
	s.systemLog = func(w *logstore.Writer, id, message string) {
		<-unblock
		writeSystem(w, id, message)
	}
	def, pidFile, spawn := gatedJob(t, "ordered-start", 0)
	def.Command = "echo out; echo err >&2; echo $$ > " + pidFile
	d, hash := putJob(t, st, def)
	r, err := s.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	spawn()
	eventually(t, 2*time.Second, "child did not emit output", func() bool { return readPID(pidFile) > 0 })
	frames, err := s.logs.Read(r.ID, 0, 100)
	if err != nil || len(frames) != 0 {
		t.Fatalf("frames before startup write = %+v, %v; want none", frames, err)
	}
	release()
	waitFailureRun(t, st, s, r.ID)
	assertStartupFirst(t, s.logs, r.ID)
}

// A metadata lock must not postpone either the startup line or capture. The
// child's output exceeds pipe capacity, so waiting for StartRun would stall it.
func TestStartupLogAndCaptureDoNotWaitForMetadata(t *testing.T) {
	dir, st, s := resilienceService(t, Options{})
	def, pidFile, spawn := gatedJob(t, "ordered-blocked-metadata", 0)
	def.Command = "echo out; echo err >&2; i=0; while [ $i -lt 10000 ]; do echo padding; i=$((i+1)); done; echo $$ > " + pidFile
	d, hash := putJob(t, st, def)
	r, err := s.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	spawn()
	eventually(t, 3*time.Second, "capture waited for blocked StartRun", func() bool { return readPID(pidFile) > 0 })
	frames, err := s.logs.Read(r.ID, 0, 10)
	if err != nil || len(frames) == 0 || frames[0].Stream != logstore.System || !strings.HasPrefix(string(frames[0].Payload), "process started as ") {
		t.Fatalf("frames while StartRun blocked = %+v, %v; want startup first", frames, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	waitFailureRun(t, st, s, r.ID)
}

// Ordering is best-effort once the sink exceeds its budget: the pumps must
// resume and finalization must not wait forever for a stalled startup write.
func TestStalledStartupLogReleasesOutputGate(t *testing.T) {
	_, st, s := resilienceService(t, Options{})
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	s.systemLog = func(w *logstore.Writer, id, message string) { <-unblock }
	s.systemWriteJoin = 50 * time.Millisecond
	r := admitFailureRun(t, st, s, model.Definition{Name: "stalled-start-output", Kind: model.KindJob, Shell: "/bin/sh", Command: "echo out; echo err >&2", SuccessCodes: []int{0}})
	if got := waitFailureRun(t, st, s, r.ID); got.Status != "succeeded" {
		t.Fatalf("run = %s, want succeeded", got.Status)
	}
	frames, err := s.logs.Read(r.ID, 0, 100)
	if err != nil || len(frames) != 2 {
		t.Fatalf("output after gate expiry = %+v, %v; want both streams", frames, err)
	}
	if s.logs.Active(r.ID) != nil {
		t.Fatal("stalled startup write prevented sealing")
	}
}
