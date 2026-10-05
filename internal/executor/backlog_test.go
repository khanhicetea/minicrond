package executor

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func failingJob(name string, retries, delaySeconds int) model.Definition {
	d := parallelJob(name, "exit 1")
	d.Retries, d.RetryDelay = retries, delaySeconds
	return d
}

func waitDone(t *testing.T, s *Service, id string) {
	t.Helper()
	if done := s.Wait(id); done != nil {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("run %s did not finish", id)
		}
	}
}

// A15: failed runs with long retry delays used to park one goroutine and timer
// each. Now one scheduler goroutine holds them in a capped heap; beyond the cap
// a retry is dropped with explicit evidence (counter, log, skipped/retry_dropped
// run), never silently.
func TestPendingRetriesAreBoundedAndOverflowIsRecorded(t *testing.T) {
	const failures, cap = 40, 5
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 8, Queue: QueueOptions{MaxPendingRetries: cap}})
	d, h := putJob(t, st, failingJob("long-delay", 1, 3600))
	before := runtime.NumGoroutine()
	for range failures {
		r, err := s.Trigger(t.Context(), d, h, "manual", nil)
		if err != nil {
			t.Fatal(err)
		}
		waitDone(t, s, r.ID)
	}
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Fatalf("goroutines grew from %d to %d with %d pending retries", before, after, failures)
	}
	eventually(t, 10*time.Second, "retry accounting", func() bool {
		return s.PendingRetries() == cap && s.RetriesDropped() == failures-cap
	})
	// Every drop leaves a terminal evidence row linked to the failed run.
	eventually(t, 10*time.Second, "retry_dropped evidence rows", func() bool {
		runs, err := st.Runs(t.Context(), "long-delay", 500)
		if err != nil {
			t.Fatal(err)
		}
		dropped := 0
		for _, r := range runs {
			if r.Status == "skipped" && r.EndReason == "retry_dropped" {
				if r.Trigger != "retry" || r.Attempt != 2 || r.ParentRunID == "" {
					t.Fatalf("evidence row = %+v", r)
				}
				dropped++
			}
		}
		return dropped == failures-cap
	})
	if diag := s.Diagnostics(t.Context()); diag.PendingRetries != cap || diag.RetriesDropped != failures-cap || diag.MaxPendingRetries != cap {
		t.Fatalf("diagnostics = %+v", diag)
	}
	// Shutdown discards the pending heap promptly (nothing is waiting on timers).
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("shutdown = %v after %v", err, time.Since(start))
	}
}

// Pending retries fire in due order from the single scheduler.
func TestRetrySchedulerFiresDueRetries(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 8})
	d, h := putJob(t, st, failingJob("soon", 1, 1))
	first, err := s.Trigger(t.Context(), d, h, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, first.ID)
	eventually(t, 10*time.Second, "retry never ran", func() bool {
		runs, _ := st.Runs(t.Context(), "soon", 10)
		for _, r := range runs {
			if r.Attempt == 2 && model.Terminal(r.Status) {
				return r.Status == "failed" && r.ParentRunID == first.ID
			}
		}
		return false
	})
}

// A15: when terminal writes fail, background finalizers are capped. Runs beyond
// the cap keep their own goroutine (and capacity slot) and retry inline, so
// nothing is dropped and the total stays bounded by max_concurrent_runs. Once
// storage recovers every run reaches its terminal state and is announced.
func TestFinalizersAreBoundedAndNothingIsDiscarded(t *testing.T) {
	const runs, finalizerCap = 6, 2
	var failing atomic.Bool
	failing.Store(true)
	var mu sync.Mutex
	finished := map[string]string{}
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: runs, Queue: QueueOptions{MaxFinalizers: finalizerCap},
		OnFinished: func(r model.Run, _ model.Definition) { mu.Lock(); finished[r.ID] = r.Status; mu.Unlock() }})
	s.finishHook = func(string) error {
		if failing.Load() {
			return errors.New("injected terminal write failure")
		}
		return nil
	}
	d, h := putJob(t, st, parallelJob("final", "true"))
	var ids []string
	for range runs {
		r, err := s.Trigger(t.Context(), d, h, "manual", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	eventually(t, 10*time.Second, "finalizer accounting", func() bool {
		background, inline := s.Finalizers()
		return background == finalizerCap && inline == runs-finalizerCap
	})
	// The inline ones still hold capacity: admissions are throttled, not unbounded.
	if got := len(s.capacity); got != runs-finalizerCap {
		t.Fatalf("capacity slots held = %d, want %d", got, runs-finalizerCap)
	}
	diag := s.Diagnostics(t.Context())
	if diag.Finalizers != finalizerCap || diag.InlineFinalizers != runs-finalizerCap || diag.MaxFinalizers != finalizerCap {
		t.Fatalf("diagnostics = %+v", diag)
	}
	owned := 0
	for _, id := range ids {
		if s.Finalizing(id) {
			owned++
		}
	}
	if owned != finalizerCap {
		t.Fatalf("%d runs report a background finalizer, want %d", owned, finalizerCap)
	}
	failing.Store(false)
	eventually(t, 20*time.Second, "terminal states after recovery", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(finished) == runs
	})
	for _, id := range ids {
		if got := runStatus(t, st, id); got.Status != "succeeded" {
			t.Fatalf("run %s = %s", id, got.Status)
		}
	}
	if background, inline := s.Finalizers(); background != 0 || inline != 0 {
		t.Fatalf("finalizers left: %d background, %d inline", background, inline)
	}
}

// A15 regression: sustained fast failures with long retry delays while the
// metadata store keeps failing terminal writes. Goroutines, pending retries and
// finalizers stay within their budgets throughout and the overload outcome is
// the documented one (drops with evidence, inline retries), not unbounded growth.
func TestSustainedFailuresWithDegradedMetadataStayBounded(t *testing.T) {
	const total, slots, retryCap, finalizerCap = 48, 6, 4, 3
	failedOnce := sync.Map{}
	var degraded atomic.Bool
	degraded.Store(true)
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: slots, Queue: QueueOptions{MaxPendingRetries: retryCap, MaxFinalizers: finalizerCap, DrainRate: 1000}})
	// The first terminal write of every run fails, the retry of it succeeds.
	s.finishHook = func(id string) error {
		if !degraded.Load() {
			return nil
		}
		if _, seen := failedOnce.LoadOrStore(id, true); !seen {
			return errors.New("injected terminal write failure")
		}
		return nil
	}
	d, h := putJob(t, st, failingJob("storm", 1, 3600))
	base := runtime.NumGoroutine()
	var peakGoroutines, peakRetries, peakFinalizers atomic.Int64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			peakGoroutines.Store(max(peakGoroutines.Load(), int64(runtime.NumGoroutine())))
			peakRetries.Store(max(peakRetries.Load(), int64(s.PendingRetries())))
			background, _ := s.Finalizers()
			peakFinalizers.Store(max(peakFinalizers.Load(), int64(background)))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	var accepted, rejected int
	for range total {
		_, err := s.Trigger(t.Context(), d, h, "manual", nil)
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrQueueFull):
			rejected++
		default:
			t.Fatal(err)
		}
	}
	// Every accepted run eventually reaches a terminal state.
	eventually(t, 60*time.Second, "all runs terminal", func() bool {
		runs, err := st.Runs(t.Context(), "storm", 500)
		if err != nil {
			t.Fatal(err)
		}
		terminal := 0
		for _, r := range runs {
			if r.Attempt == 1 && model.Terminal(r.Status) {
				terminal++
			}
		}
		return terminal == accepted
	})
	close(stop)
	<-sampled
	if peakRetries.Load() > retryCap || peakFinalizers.Load() > finalizerCap {
		t.Fatalf("budgets exceeded: retries %d (cap %d), finalizers %d (cap %d)", peakRetries.Load(), retryCap, peakFinalizers.Load(), finalizerCap)
	}
	// At most one execute goroutine per capacity slot plus the three loops and
	// their helpers, independent of how many runs failed.
	if limit := int64(base) + int64(slots)*8 + 20; peakGoroutines.Load() > limit {
		t.Fatalf("peak goroutines %d, want at most %d for %d failures", peakGoroutines.Load(), limit, total)
	}
	if s.RetriesDropped() == 0 {
		t.Fatal("expected overflowing retries to be dropped with evidence")
	}
	t.Logf("accepted %d, rejected %d, retries dropped %d, peaks: goroutines %d (base %d), retries %d, finalizers %d",
		accepted, rejected, s.RetriesDropped(), peakGoroutines.Load(), base, peakRetries.Load(), peakFinalizers.Load())
}

// ADR-8 2A for every completion path: a log Seal failure never changes the
// execution result, whether the run failed on its own or could not start.
func TestSealFailureKeepsFailureAndStartErrorStatus(t *testing.T) {
	_, st, s := resilienceService(t, Options{})
	s.sealHook = func(id string) error {
		_ = s.logs.Seal(id)
		return errors.New("injected seal failure")
	}
	failing, fh := putJob(t, st, parallelJob("seal-fail-exit", "exit 3"))
	badStart := parallelJob("seal-fail-start", "")
	badStart.Argv = []string{"/nonexistent/program"}
	bad, bh := putJob(t, st, badStart)
	for _, tc := range []struct {
		d           model.Definition
		h           string
		status, why string
	}{{failing, fh, "failed", "exit_nonzero"}, {bad, bh, "failed", "start_error"}} {
		run, err := s.Trigger(t.Context(), tc.d, tc.h, "manual", nil)
		if err != nil {
			t.Fatal(err)
		}
		waitDone(t, s, run.ID)
		stored := runStatus(t, st, run.ID)
		if stored.Status != tc.status || stored.EndReason != tc.why || !stored.LogTruncated {
			t.Fatalf("%s = %s/%s truncated=%v; want %s/%s truncated", tc.d.Name, stored.Status, stored.EndReason, stored.LogTruncated, tc.status, tc.why)
		}
	}
}
