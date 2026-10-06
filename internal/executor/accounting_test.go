package executor

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestExitAccountingJobsAndWorkers(t *testing.T) {
	for _, kind := range []string{model.KindJob, model.KindWorker} {
		for _, tc := range []struct{ name, command, shell, status string }{
			{"success", "i=0; while [ $i -lt 10000 ]; do i=$((i+1)); done", "/bin/sh", "succeeded"},
			{"failure", "exit 7", "/bin/sh", "failed"},
			{"start-error", "true", "/nonexistent/minicrond-test-shell", "failed"},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				events := make(chan model.Run, 1)
				st, _, s := failureService(t, Options{OnFinished: func(r model.Run, _ model.Definition) { events <- r }})
				d := model.Definition{Name: "accounting", Kind: kind, Command: tc.command, Shell: tc.shell, EnvBase: "clean", SuccessCodes: []int{0}}
				r := admitFailureRun(t, st, s, d)
				got := waitFailureRun(t, st, s, r.ID)
				if got.Status != tc.status {
					t.Fatalf("run=%+v", got)
				}
				var event model.Run
				select {
				case event = <-events:
				case <-time.After(5 * time.Second):
					t.Fatal("missing completion callback")
				}
				if tc.name == "start-error" {
					if got.ResourceUsage != nil || event.ResourceUsage != nil {
						t.Fatal("start failure has fabricated accounting")
					}
					return
				}
				if got.ResourceUsage == nil || event.ResourceUsage == nil || *got.ResourceUsage != *event.ResourceUsage {
					t.Fatalf("stored=%+v event=%+v", got.ResourceUsage, event.ResourceUsage)
				}
				if got.ResourceUsage.PeakRSSBytes <= 0 || got.ResourceUsage.UserCPUUS+got.ResourceUsage.SystemCPUUS <= 0 {
					t.Fatalf("usage=%+v", got.ResourceUsage)
				}
			})
		}
	}
}

func TestFinalizerPreservesExitAccounting(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	events := make(chan model.Run, 1)
	st, _, s := failureService(t, Options{OnFinished: func(r model.Run, _ model.Definition) { events <- r }})
	s.finishHook = func(string) error {
		if failing.Load() {
			return errors.New("storage offline")
		}
		return nil
	}
	r := admitFailureRun(t, st, s, model.Definition{Name: "final-accounting", Kind: model.KindWorker, Command: "exit 0", Shell: "/bin/sh", SuccessCodes: []int{0}})
	waitDone(t, s, r.ID)
	if !s.Finalizing(r.ID) {
		t.Fatal("run was not handed to finalizer")
	}
	failing.Store(false)
	select {
	case event := <-events:
		got, err := st.Run(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ResourceUsage == nil || event.ResourceUsage == nil || *got.ResourceUsage != *event.ResourceUsage {
			t.Fatalf("usage=%+v event=%+v", got.ResourceUsage, event.ResourceUsage)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("finalizer did not recover")
	}
}

func TestMonitorTargetsBoundedCopy(t *testing.T) {
	s := New(nil, nil, Options{})
	for _, id := range []string{"one", "two", "three"} {
		s.active[id] = &activeRun{monitor: ProcessTarget{RunID: id, PID: 123, StartID: "id"}}
	}
	targets, total := s.MonitorTargets(2)
	if len(targets) != 2 || total != 3 {
		t.Fatalf("targets=%+v total=%d", targets, total)
	}
	targets[0].PID = 999
	for _, a := range s.active {
		if a.monitor.PID != 123 {
			t.Fatal("snapshot aliases active run")
		}
	}
	targets, total = s.MonitorTargets(0)
	if len(targets) != 0 || total != 3 {
		t.Fatalf("targets=%+v total=%d", targets, total)
	}
	if exitUsage(nil) != nil || exitUsage(&os.ProcessState{}) != nil {
		t.Fatal("missing process state has accounting")
	}
}
