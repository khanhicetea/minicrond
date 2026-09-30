package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func failureService(t *testing.T, opt Options) (*store.Store, *logstore.Store, *Service) {
	t.Helper()
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, logs, opt)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return st, logs, s
}

func admitFailureRun(t *testing.T, st *store.Store, s *Service, d model.Definition) model.Run {
	t.Helper()
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	d, hash, err := st.Definition(t.Context(), d.Name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitFailureRun(t *testing.T, st *store.Store, s *Service, id string) model.Run {
	t.Helper()
	if done := s.Wait(id); done != nil {
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			t.Fatal("run did not finish within deadline")
		}
	}
	r, err := st.Run(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClosedOutputPipesDoNotDisableTimeout(t *testing.T) {
	st, _, s := failureService(t, Options{})
	d := model.Definition{Name: "closed-pipes", Kind: model.KindJob, Shell: "/bin/sh", Command: "exec 1>&- 2>&-; sleep 30", Timeout: 1, SuccessCodes: []int{0}, Timezone: "UTC"}
	r := admitFailureRun(t, st, s, d)
	finished := waitFailureRun(t, st, s, r.ID)
	if finished.Status != "timeout" {
		t.Fatalf("status = %s, want timeout", finished.Status)
	}
}

func TestExecutionPanicPersistsFailureAndReleasesResources(t *testing.T) {
	st, logs, s := failureService(t, Options{MaxConcurrentRuns: 1})
	oldLookup := lookupCurrentUser
	t.Cleanup(func() { lookupCurrentUser = oldLookup })
	lookupCurrentUser = func() (*user.User, error) { panic("identity callback failed") }
	d := model.Definition{Name: "panic", Kind: model.KindJob, Shell: "/bin/sh", Command: "true", SuccessCodes: []int{0}, Timezone: "UTC"}
	r := admitFailureRun(t, st, s, d)
	finished := waitFailureRun(t, st, s, r.ID)
	if finished.Status != "failed" || finished.EndReason != "internal_error" {
		t.Fatalf("panic result = %s/%s", finished.Status, finished.EndReason)
	}
	if s.Active(d.Name) != 0 || logs.Active(r.ID) != nil || len(s.capacity) != 0 {
		t.Fatal("panic leaked run resources")
	}
	lookupCurrentUser = oldLookup
	r = admitFailureRun(t, st, s, model.Definition{Name: "after-panic", Kind: model.KindJob, Shell: "/bin/sh", Command: "true", SuccessCodes: []int{0}, Timezone: "UTC"})
	if finished := waitFailureRun(t, st, s, r.ID); finished.Status != "succeeded" {
		t.Fatalf("next run status = %s", finished.Status)
	}
}

func TestCompletionPanicDoesNotPreventRetryOrShutdown(t *testing.T) {
	events := make(chan model.Run, 2)
	st, _, s := failureService(t, Options{MaxConcurrentRuns: 1, OnFinished: func(r model.Run, d model.Definition) {
		events <- r.Clone()
		*r.EndedAt = time.Time{}
		d.SuccessCodes[0] = 1
		panic("completion callback failed")
	}})
	d := model.Definition{Name: "retry-panic", Kind: model.KindJob, Shell: "/bin/sh", Command: "exit 1", Retries: 1, RetryDelay: 1, SuccessCodes: []int{0}, Timezone: "UTC"}
	admitFailureRun(t, st, s, d)
	for attempt := 1; attempt <= 2; attempt++ {
		timer := time.NewTimer(4 * time.Second)
		select {
		case r := <-events:
			if r.Status != "failed" || r.Attempt != attempt || r.EndedAt.IsZero() {
				t.Fatalf("completion = %#v", r)
			}
			waitFailureRun(t, st, s, r.ID)
		case <-timer.C:
			t.Fatal("callback panic suppressed retry")
		}
		timer.Stop()
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownDeadlineOverridesLongGrace(t *testing.T) {
	st, logs, s := failureService(t, Options{})
	d := model.Definition{Name: "long-grace", Kind: model.KindJob, Shell: "/bin/sh", Command: "trap '' TERM; echo ready; exec sleep 30", Grace: 3600, SuccessCodes: []int{0}, Timezone: "UTC"}
	r := admitFailureRun(t, st, s, d)
	readyTimer := time.NewTimer(3 * time.Second)
	defer readyTimer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
ready:
	for {
		frames, err := logs.Read(r.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			if frame.Stream == logstore.Stdout && string(frame.Payload) == "ready" {
				break ready
			}
		}
		select {
		case <-readyTimer.C:
			t.Fatal("process did not become ready")
		case <-ticker.C:
		}
	}
	// A canceled shutdown context must force termination even when graceful
	// stopping has started and the configured grace is an hour.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown = %v", err)
	}
	finished := waitFailureRun(t, st, s, r.ID)
	if finished.Status != "stopped" {
		t.Fatalf("status = %s", finished.Status)
	}
}

func TestCompletedRunTerminatesBackgroundDescendants(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses procfs to distinguish running processes from zombies")
	}
	st, logs, s := failureService(t, Options{})
	d := model.Definition{Name: "background", Kind: model.KindJob, Shell: "/bin/sh", Command: "sleep 30 >/dev/null 2>&1 & echo $!", SuccessCodes: []int{0}, Timezone: "UTC"}
	r := admitFailureRun(t, st, s, d)
	if finished := waitFailureRun(t, st, s, r.ID); finished.Status != "succeeded" {
		t.Fatalf("status = %s", finished.Status)
	}
	frames, err := logs.Read(r.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for _, frame := range frames {
		if frame.Stream == logstore.Stdout {
			pid, err = strconv.Atoi(strings.TrimSpace(string(frame.Payload)))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if pid <= 0 {
		t.Fatal("missing descendant pid")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		_, rest, ok := strings.CutLast(string(stat), ")")
		if ok && strings.HasPrefix(strings.TrimSpace(rest), "Z ") {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("completed run left a live descendant")
		case <-ticker.C:
		}
	}
}
