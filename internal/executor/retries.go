package executor

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
)

// retryItem is a failed run waiting for its retry delay. It carries identity
// only: the current definition is read again when the retry is due.
type retryItem struct {
	due       time.Time
	runID     string
	name      string
	defID     int64
	revision  int64
	hash      string
	attempt   int // attempt number of the failed run
	scheduled *time.Time
	delay     time.Duration
}

type retryHeap []retryItem

func (h retryHeap) Len() int           { return len(h) }
func (h retryHeap) Less(i, j int) bool { return h[i].due.Before(h[j].due) }
func (h retryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *retryHeap) Push(x any)        { *h = append(*h, x.(retryItem)) }
func (h *retryHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = retryItem{}
	*h = old[:n-1]
	return it
}

// retryEvidenceCap bounds dropped retries whose evidence row is not yet written.
const retryEvidenceCap = 64

// retryBatch bounds how many due retries one loop pass triggers before it
// re-reads the clock and the stop signal.
const retryBatch = 16

// retryState is the one global retry scheduler: a min-heap served by a single
// goroutine whose timer is armed for the earliest due item only. Pending
// retries are in memory, so (as before) they do not survive a restart.
type retryState struct {
	mu       sync.Mutex
	heap     retryHeap
	started  bool // guarded by Service.mu
	wake     chan struct{}
	dropped  atomic.Int64
	evidence []retryItem // newest-wins ring of drops awaiting a run row
	lost     atomic.Int64
}

// scheduleRetry queues the next attempt of a failed job run. At the global cap
// the retry is dropped with explicit evidence (never silently): an error log,
// the retries_dropped counter and, written by the scheduler goroutine, a
// terminal skipped/retry_dropped run for that attempt.
func (s *Service) scheduleRetry(r model.Run, d model.Definition) {
	if d.Kind != model.KindJob || r.Status != "failed" || r.Attempt > d.Retries {
		return
	}
	delay := time.Duration(d.RetryDelay) * time.Second
	if delay <= 0 {
		delay = 5 * time.Second
	}
	s.addRetry(retryItem{due: time.Now().Add(delay), runID: r.ID, name: d.Name, defID: d.ID, revision: r.Revision, hash: r.DefinitionHash, attempt: r.Attempt, scheduled: cloneTime(r.ScheduledFor), delay: delay})
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// startLoop starts fn once as a joined background goroutine, unless shutdown
// has begun. started is guarded by s.mu.
func (s *Service) startLoop(started *bool, fn func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	if !*started {
		*started = true
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			fn()
		}()
	}
	return true
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Service) addRetry(it retryItem) {
	if !s.startLoop(&s.retries.started, s.retryLoop) {
		return
	}
	s.retries.mu.Lock()
	if len(s.retries.heap) >= s.queueOpt.MaxPendingRetries {
		n := s.retries.dropped.Add(1)
		if len(s.retries.evidence) >= retryEvidenceCap {
			s.retries.evidence = s.retries.evidence[1:]
			s.retries.lost.Add(1)
		}
		s.retries.evidence = append(s.retries.evidence, it)
		s.retries.mu.Unlock()
		if n == 1 || n%1000 == 0 {
			slog.Error("pending retry budget exhausted; retry dropped (recorded as skipped/retry_dropped)", "job", it.name, "run", it.runID, "limit", s.queueOpt.MaxPendingRetries, "dropped_total", n)
		}
		signal(s.retries.wake)
		return
	}
	heap.Push(&s.retries.heap, it)
	s.retries.mu.Unlock()
	signal(s.retries.wake)
}

// PendingRetries reports retries waiting for their delay.
func (s *Service) PendingRetries() int {
	s.retries.mu.Lock()
	defer s.retries.mu.Unlock()
	return len(s.retries.heap)
}

// RetriesDropped reports retries dropped at the pending-retry cap since start.
func (s *Service) RetriesDropped() int64 { return s.retries.dropped.Load() }

func (s *Service) retryLoop() {
	var idle idleTimer
	defer idle.stop()
	for {
		if err := fault.Call(func() error { s.retryStep(); return nil }); err != nil {
			logFailure("retry scheduler panicked", err)
		}
		if s.isClosing() {
			return
		}
		s.retries.mu.Lock()
		wait := time.Duration(-1)
		if len(s.retries.heap) > 0 {
			wait = max(time.Until(s.retries.heap[0].due), 0)
		}
		pendingEvidence := len(s.retries.evidence) > 0
		s.retries.mu.Unlock()
		if pendingEvidence || wait == 0 {
			continue
		}
		select {
		case <-s.stopping:
			return
		case <-s.retries.wake:
		case <-idle.arm(wait):
		}
	}
}

// retryStep writes pending drop evidence, then triggers up to retryBatch due
// retries.
func (s *Service) retryStep() {
	s.persistRetryEvidence()
	now := time.Now()
	var due []retryItem
	s.retries.mu.Lock()
	for len(s.retries.heap) > 0 && len(due) < retryBatch && !s.retries.heap[0].due.After(now) {
		due = append(due, heap.Pop(&s.retries.heap).(retryItem))
	}
	s.retries.mu.Unlock()
	for _, it := range due {
		if s.isClosing() {
			return
		}
		if err := fault.Call(func() error { s.retryNow(it); return nil }); err != nil {
			logFailure("job retry panicked", err, "run", it.runID, "job", it.name)
		}
	}
}

// retryNow triggers one due retry against the current definition: disabled,
// deleted or reduced budgets must not launch it.
func (s *Service) retryNow(it retryItem) {
	current, hash, err := s.store.Definition(s.stopCtx, it.name)
	if err != nil {
		if s.stopCtx.Err() == nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, sql.ErrNoRows) {
			logFailure("reading retry definition failed", err, "job", it.name, "run", it.runID)
		}
		return
	}
	if current.ID != it.defID || current.Kind != model.KindJob || !current.IsEnabled() || it.attempt > current.Retries {
		return
	}
	_, _, err = s.trigger(s.stopCtx, current, hash, "retry", it.scheduled, nil, it.attempt+1, it.runID)
	switch {
	case errors.Is(err, errCapacity):
		// Queue disabled and every slot busy: wait another retry delay.
		slog.Info("job retry deferred: concurrent run capacity is full", "job", it.name, "run", it.runID, "attempt", it.attempt+1)
		it.due = time.Now().Add(it.delay)
		s.addRetry(it)
	case err == nil, errors.Is(err, ErrShutdown), s.stopCtx.Err() != nil:
	default:
		slog.Error("job retry trigger failed", "job", it.name, "run", it.runID, "error", err)
	}
}

// persistRetryEvidence writes a terminal skipped/retry_dropped run for each
// dropped retry. It runs on the scheduler goroutine, off the completion path.
func (s *Service) persistRetryEvidence() {
	s.retries.mu.Lock()
	items := s.retries.evidence
	s.retries.evidence = nil
	s.retries.mu.Unlock()
	for i, it := range items {
		if s.isClosing() {
			return
		}
		now := time.Now().UTC()
		id := uuid.NewV7().String()
		run := model.Run{ID: id, DefinitionID: it.defID, Job: it.name, Kind: model.KindJob, Revision: it.revision, DefinitionHash: it.hash, Status: "skipped", EndReason: "retry_dropped", Trigger: "retry", Attempt: it.attempt + 1, ParentRunID: it.runID, ScheduledFor: it.scheduled, BootID: s.bootID, QueuedAt: now, EndedAt: &now}
		ctx, cancel := context.WithTimeout(s.stopCtx, 5*time.Second)
		err := s.store.CreateRun(ctx, run)
		cancel()
		if err != nil {
			s.retries.lost.Add(int64(len(items) - i))
			logFailure("recording dropped retry failed; the drop is only in the log and counters", err, "run", it.runID, "job", it.name)
			return
		}
	}
}

// idleTimer is a reusable timer for loops that sleep until the next due time.
type idleTimer struct{ t *time.Timer }

// arm returns a channel that fires after d, or a nil channel (never) for d < 0.
func (i *idleTimer) arm(d time.Duration) <-chan time.Time {
	if d < 0 {
		if i.t != nil {
			i.t.Stop()
		}
		return nil
	}
	if i.t == nil {
		i.t = time.NewTimer(d)
	} else {
		i.t.Reset(d)
	}
	return i.t.C
}

func (i *idleTimer) stop() {
	if i.t != nil {
		i.t.Stop()
	}
}
