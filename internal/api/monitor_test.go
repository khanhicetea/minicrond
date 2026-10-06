package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/procstats"
)

func monitorResponse(t *testing.T, s *Server, token string) monitorSnapshot {
	t.Helper()
	rec := call(s, false, "GET", "/api/v1/monitor", token, "", nil)
	if rec.Code != 200 {
		t.Fatalf("monitor: %d %s", rec.Code, rec.Body.String())
	}
	var result monitorSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMonitorAuthenticatedAndCached(t *testing.T) {
	s, token, _ := setup(t)
	if rec := call(s, false, "GET", "/api/v1/monitor", "", "", nil); rec.Code != 401 {
		t.Fatalf("unauthenticated status=%d", rec.Code)
	}
	first := monitorResponse(t, s, token)
	if first.Supported != procstats.Supported || first.SampledAt.IsZero() || first.Items == nil || first.Goroutines <= 0 || first.HeapBytes == 0 {
		t.Fatalf("snapshot=%+v", first)
	}
	if procstats.Supported && (first.Daemon == nil || first.Daemon.RSSBytes <= 0) {
		t.Fatalf("daemon=%+v", first.Daemon)
	}
	second := monitorResponse(t, s, token)
	if !first.SampledAt.Equal(second.SampledAt) {
		t.Fatal("nearby readers repeated sampling")
	}
}

func TestMonitorExpiryAndCanceledCollection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ex := executor.New(nil, nil, executor.Options{})
		var share monitorShare
		first, err := share.get(t.Context(), ex)
		if err != nil {
			t.Fatal(err)
		}
		second, err := share.get(t.Context(), ex)
		if err != nil || !second.SampledAt.Equal(first.SampledAt) {
			t.Fatalf("cache=%+v %v", second, err)
		}
		time.Sleep(monitorTTL)
		synctest.Wait()
		share.mu.Lock()
		retained := share.snapshot.Items != nil || share.expiry != nil || !share.at.IsZero()
		share.mu.Unlock()
		if retained {
			t.Fatal("cache retained without viewers")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := share.get(ctx, ex); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled collection=%v", err)
		}
		share.mu.Lock()
		defer share.mu.Unlock()
		if share.expiry != nil || share.snapshot.Items != nil {
			t.Fatal("canceled read populated cache")
		}
	})
}

func TestMonitorAdmissionAndConcurrentCollectorRefused(t *testing.T) {
	s, token, _ := setup(t)
	s.SetReadLimits(ReadLimits{Slots: 1})
	release, err := s.gate().acquire(t.Context(), logPageCost, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := call(s, false, "GET", "/api/v1/monitor", token, "", nil)
	release()
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("busy=%d %s", rec.Code, rec.Body.String())
	}
	s.monitoring.mu.Lock()
	rec = call(s, false, "GET", "/api/v1/monitor", token, "", nil)
	s.monitoring.mu.Unlock()
	if rec.Code != 503 {
		t.Fatalf("collector busy=%d", rec.Code)
	}
	if inflight, _, waiting := s.gate().stats(); inflight != 0 || waiting != 0 {
		t.Fatalf("gate inflight=%d waiting=%d", inflight, waiting)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequest("GET", "/api/v1/monitor", nil).WithContext(ctx)
	s.monitor(httptest.NewRecorder(), req)
	if inflight, _, _ := s.gate().stats(); inflight != 0 {
		t.Fatal("canceled request leaked slot")
	}
}

func TestMonitorRunningChildAndExitHistory(t *testing.T) {
	s, token, st := setup(t)
	d, err := st.PutDefinition(t.Context(), model.Definition{Name: "monitored", Kind: model.KindWorker, Command: "exec sleep 30", Shell: "/bin/sh", SuccessCodes: []int{0}}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	d, hash, err := st.Definition(t.Context(), d.Name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.exec.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		targets, _ := s.exec.MonitorTargets(monitorLimit)
		if len(targets) == 1 && targets[0].PID > 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("child did not start")
		}
	}
	snapshot := monitorResponse(t, s, token)
	if snapshot.Active != 1 || len(snapshot.Items) != 1 || snapshot.Items[0].RunID != r.ID {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if procstats.Supported && snapshot.Items[0].Stats == nil {
		t.Fatal("running child stats missing")
	}
	done := s.exec.Wait(r.ID)
	if err := s.exec.Stop(r.ID); err != nil {
		t.Fatal(err)
	}
	if done != nil {
		select {
		case <-done:
		case <-deadline.C:
			t.Fatal("child did not stop")
		}
	}
	rec := call(s, false, "GET", "/api/v1/runs/"+r.ID, token, "", nil)
	var history model.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || history.Status != "stopped" || history.ResourceUsage == nil {
		t.Fatalf("history=%+v HTTP=%d", history, rec.Code)
	}
}
