package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
)

// gatedJob returns a job whose spawn is held until the returned release
// function writes to the env_file FIFO, so a test can arrange blocking
// persistence between admission and spawn. The child records its pid.
func gatedJob(t *testing.T, name string, timeout int) (model.Definition, string, func()) {
	t.Helper()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "env.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "pid")
	d := model.Definition{Name: name, Kind: model.KindJob, Command: "echo $$ > " + pidFile + "; exec sleep 30", Shell: "/bin/sh", EnvFile: fifo, Timeout: timeout, Grace: 0, OnOverlap: "skip", SuccessCodes: []int{0}}
	release := func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = f.WriteString("GATE=1\n")
		_ = f.Close()
	}
	return d, pidFile, release
}

func readPID(pidFile string) int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func processGone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

func waitProcessGone(t *testing.T, pidFile string, limit time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if pid := readPID(pidFile); pid > 0 && processGone(pid) {
			return
		}
		if time.Now().After(deadline) {
			if pid := readPID(pidFile); pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
			t.Fatal(what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A10: the runtime timeout starts at spawn. A metadata write transaction held
// by another connection must neither delay it nor leave the child running.
func TestTimeoutNotDelayedByBlockedStartPersistence(t *testing.T) {
	dir, st, s := resilienceService(t, Options{})
	def, pidFile, release := gatedJob(t, "blocked-start", 1)
	d, hash := putJob(t, st, def)
	run, err := s.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	tx, err := saboteur.BeginTx(t.Context(), nil) // immediate: holds the write lock
	if err != nil {
		t.Fatal(err)
	}
	released := time.Now()
	release()
	waitProcessGone(t, pidFile, 2500*time.Millisecond, "child outlived its 1s timeout while StartRun was blocked")
	if elapsed := time.Since(released); elapsed < 900*time.Millisecond {
		t.Fatalf("child died after %s, before its timeout", elapsed)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "run never reached a terminal state", func() bool {
		r, err := st.Run(t.Context(), run.ID)
		return err == nil && model.Terminal(r.Status)
	})
	stored, err := st.Run(t.Context(), run.ID)
	if err != nil || stored.Status != "timeout" || stored.StartedAt == nil {
		t.Fatalf("stored run = %s/%s started=%v, %v; want timeout with a start time", stored.Status, stored.EndReason, stored.StartedAt, err)
	}
}

// A10: a stalled log sink (system lines) must not delay timeout enforcement.
func TestTimeoutNotDelayedByBlockedSystemLog(t *testing.T) {
	_, st, s := resilienceService(t, Options{})
	unblock := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(unblock) }) })
	orig := systemLog
	systemLog = func(w *logstore.Writer, runID, message string) {
		<-unblock
		orig(w, runID, message)
	}
	t.Cleanup(func() { systemLog = orig })
	def, pidFile, release := gatedJob(t, "blocked-log", 1)
	d, hash := putJob(t, st, def)
	run, err := s.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	waitProcessGone(t, pidFile, 2500*time.Millisecond, "child outlived its 1s timeout while system log writes were blocked")
	once.Do(func() { close(unblock) })
	if done := s.Wait(run.ID); done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("run never completed after the log sink recovered")
		}
	}
	stored, err := st.Run(t.Context(), run.ID)
	if err != nil || stored.Status != "timeout" {
		t.Fatalf("stored run = %s/%s, %v; want timeout", stored.Status, stored.EndReason, err)
	}
}

// A04: shutdown must not wait for the admission mutex. A trigger stuck in
// storage setup is bounded by the shutdown context, and once it resumes it is
// refused instead of registering a run after shutdown.
func TestShutdownDoesNotWaitForStuckAdmission(t *testing.T) {
	dir, st, s := resilienceService(t, Options{})
	d, hash := putJob(t, st, model.Definition{Name: "stuck-admission", Kind: model.KindJob, Command: "sleep 30", Shell: "/bin/sh", OnOverlap: "skip", SuccessCodes: []int{0}})
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	tx, err := saboteur.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	triggered := make(chan error, 1)
	go func() {
		_, err := s.Trigger(t.Context(), d, hash, "manual", nil) // blocks in CreateRun holding admission
		triggered <- err
	}()
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = s.Shutdown(ctx)
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s waiting for a stuck admission", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want deadline exceeded while a trigger is still using storage", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-triggered:
		if !errors.Is(err, ErrShutdown) {
			t.Fatalf("trigger after shutdown began = %v, want ErrShutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("trigger never returned")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v", err)
	}
	if got := s.Active(d.Name); got != 0 {
		t.Fatalf("active after refused trigger = %d", got)
	}
	runs, err := st.Runs(t.Context(), d.Name, 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("runs = %+v, %v; want the refused run finalized as failed", runs, err)
	}
}
