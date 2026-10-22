package executor

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
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

// ADR-16: when terminal writes fail, completions are announced at once, no
// capacity slot is held for history, and the deferred-history map is capped.
// Runs beyond the cap keep executing; only their history is counted as lost.
// Once storage recovers every deferred run reaches its terminal state.
func TestFailedTerminalWritesNeverHoldCapacity(t *testing.T) {
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
	for _, id := range ids {
		waitDone(t, s, id)
	}
	// Every completion is announced while storage is still failing.
	mu.Lock()
	announced := len(finished)
	mu.Unlock()
	if announced != runs {
		t.Fatalf("announced %d of %d runs while storage failed", announced, runs)
	}
	if got := len(s.capacity); got != 0 {
		t.Fatalf("capacity slots held = %d, want 0", got)
	}
	background, inline := s.Finalizers()
	if background > finalizerCap || inline != 0 {
		t.Fatalf("finalizers = %d background, %d inline; cap %d", background, inline, finalizerCap)
	}
	// Lost history is counted per record (a run can lose its running state
	// and later its terminal state), so it is at least the runs left out.
	deferred, dropped := s.History()
	if dropped < int64(runs-background) {
		t.Fatalf("history dropped = %d (deferred %d), want %d", dropped, deferred, runs-background)
	}
	if !s.StorageDegraded() {
		t.Fatal("storage not reported degraded")
	}
	spooled := map[string]bool{}
	for _, id := range ids {
		if s.Finalizing(id) {
			spooled[id] = true
		}
	}
	failing.Store(false)
	eventually(t, 20*time.Second, "deferred history written after recovery", func() bool {
		background, _ := s.Finalizers()
		return background == 0
	})
	for id := range spooled {
		if got := runStatus(t, st, id); got.Status != "succeeded" {
			t.Fatalf("run %s = %s", id, got.Status)
		}
	}
	if s.StorageDegraded() {
		t.Fatal("storage still reported degraded after recovery")
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
	// Every accepted run eventually reaches a terminal state, or its history
	// is counted as lost because the deferred-history budget was full.
	eventually(t, 60*time.Second, "all runs terminal or counted", func() bool {
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
		background, _ := s.Finalizers()
		_, dropped := s.History()
		return background == 0 && int64(terminal)+dropped >= int64(accepted)
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

// ADR-16: with a permanently failing terminal write and a tiny history
// budget, every run still executes and is announced promptly; the map stays at
// its cap and the rest of the history is counted as lost.
func TestHistoryOverflowNeverBlocksExecution(t *testing.T) {
	const cap = 1
	var finished atomic.Int64
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 3, Queue: QueueOptions{MaxFinalizers: cap, DrainRate: 1000},
		OnFinished: func(model.Run, model.Definition) { finished.Add(1) }})
	s.finishHook = func(string) error { return errors.New("permanent update failure") }
	d, h := putJob(t, st, parallelJob("wedge", "true"))
	for range 8 {
		r, err := s.Trigger(t.Context(), d, h, "manual", nil)
		if err != nil {
			t.Fatal(err)
		}
		waitDone(t, s, r.ID)
	}
	if got := finished.Load(); got != 8 {
		t.Fatalf("announced %d of 8 runs", got)
	}
	if background, _ := s.Finalizers(); background != cap {
		t.Fatalf("background finalizers = %d, cap %d", background, cap)
	}
	if _, dropped := s.History(); dropped != 7 {
		t.Fatalf("history dropped = %d, want 7", dropped)
	}
	if got := len(s.capacity); got != 0 {
		t.Fatalf("capacity slots held = %d", got)
	}
}

// Workers never wait for their terminal write: the done signal closes
// promptly and the outcome is available in memory for supervision.
func TestWorkerFinalizationNeverHoldsInline(t *testing.T) {
	_, st, s := resilienceService(t, Options{Queue: QueueOptions{MaxFinalizers: 1}})
	s.finishHook = func(string) error { return errors.New("permanent update failure") }
	for i := range 3 {
		w := parallelJob("worker-"+string(rune('a'+i)), "true")
		w.Kind = model.KindWorker
		d, h := putJob(t, st, w)
		r, err := s.Trigger(t.Context(), d, h, "startup", nil)
		if err != nil {
			t.Fatal(err)
		}
		waitDone(t, s, r.ID) // must close promptly, not after storage recovers
		out, ok := s.TakeOutcome(r.ID)
		if !ok || out.Status != "succeeded" || out.StartedAt == nil || out.EndedAt == nil {
			t.Fatalf("outcome = %+v, %v", out, ok)
		}
		if _, again := s.TakeOutcome(r.ID); again {
			t.Fatal("outcome returned twice")
		}
	}
	if background, inline := s.Finalizers(); background != 1 || inline != 0 {
		t.Fatalf("finalizers = %d background, %d inline; want the capped 1", background, inline)
	}
}

// S5: a poison item (permanent error for one run) must not starve the others.
func TestPoisonFinalizerDoesNotStarveOthers(t *testing.T) {
	var failAll atomic.Bool
	failAll.Store(true)
	var poison atomic.Value
	poison.Store("")
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 8, Queue: QueueOptions{MaxFinalizers: 16}})
	s.finishHook = func(id string) error {
		if failAll.Load() || id == poison.Load().(string) {
			return errors.New("injected failure")
		}
		return nil
	}
	d, h := putJob(t, st, parallelJob("poisoned", "true"))
	var ids []string
	for range 5 {
		r, err := s.Trigger(t.Context(), d, h, "manual", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
		waitDone(t, s, r.ID)
	}
	if background, _ := s.Finalizers(); background != 5 {
		t.Fatalf("background = %d, want 5", background)
	}
	// The oldest item becomes permanently failing; storage works for the rest.
	poison.Store(ids[0])
	failAll.Store(false)
	eventually(t, 30*time.Second, "others finalized despite the poison item", func() bool {
		background, _ := s.Finalizers()
		return background == 1
	})
	for _, id := range ids[1:] {
		if got := runStatus(t, st, id); got.Status != "succeeded" {
			t.Fatalf("run %s = %s", id, got.Status)
		}
	}
	if !s.Finalizing(ids[0]) {
		t.Fatal("poison item left the map")
	}
}

// NIT 8 / ADR-8 2A: a log pump error after the output was read must not turn a
// successful run into failed/log_error: the real status is kept and the logs are
// marked truncated.
func TestPumpErrorKeepsExecutionStatus(t *testing.T) {
	_, st, s := resilienceService(t, Options{})
	s.pipeHook = func(w *logstore.Writer, stream logstore.Stream, r io.Reader) error {
		err := w.Pipe(stream, r)
		if err == nil && stream == logstore.Stdout {
			return errors.New("injected pump failure")
		}
		return err
	}
	d, h := putJob(t, st, parallelJob("pump-fails", "echo hi"))
	run, err := s.Trigger(t.Context(), d, h, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, run.ID)
	got := runStatus(t, st, run.ID)
	if got.Status != "succeeded" || got.EndReason != "exit" || !got.LogTruncated {
		t.Fatalf("run = %s/%s truncated=%v; want succeeded/exit truncated", got.Status, got.EndReason, got.LogTruncated)
	}
}
