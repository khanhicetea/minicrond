package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// QueueOptions bound the durable execution queue, the pending-retry budget and
// the background finalizer budget (ADR-9). Zero values select the defaults.
type QueueOptions struct {
	// Disabled restores the pre-queue behavior: over-capacity triggers are
	// recorded skipped/queue_full.
	Disabled bool
	// MaxItems, MaxPerJob and MaxBytes bound the persisted queue; MaxAge is how
	// long an item may wait for capacity.
	MaxItems, MaxPerJob int
	MaxBytes            int64
	MaxAge              time.Duration
	// DrainRate is the most queued runs started per second.
	DrainRate int
	// MaxPendingRetries caps retries waiting for their delay.
	MaxPendingRetries int
	// MaxFinalizers caps runs whose terminal write is retried in the
	// background; beyond it the completing job retries inline for at most
	// InlineHold, then hands off up to 4x this cap.
	MaxFinalizers int
	InlineHold    time.Duration
}

func (o QueueOptions) withDefaults() QueueOptions {
	if o.MaxItems <= 0 {
		o.MaxItems = 100
	}
	if o.MaxPerJob <= 0 {
		o.MaxPerJob = min(25, o.MaxItems)
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 256 << 10
	}
	if o.MaxAge <= 0 {
		o.MaxAge = 15 * time.Minute
	}
	if o.DrainRate <= 0 {
		o.DrainRate = 5
	}
	if o.MaxPendingRetries <= 0 {
		o.MaxPendingRetries = 500
	}
	if o.MaxFinalizers <= 0 {
		o.MaxFinalizers = 256
	}
	if o.InlineHold <= 0 {
		o.InlineHold = defaultInlineHold
	}
	return o
}

// queueState is the drain loop's in-memory view. The queue itself lives in
// SQLite; depth mirrors its row count so an empty queue costs nothing (no
// polling, no timer).
type queueState struct {
	depth   atomic.Int64
	wake    chan struct{}
	started bool // guarded by Service.mu
	// Loop-owned.
	served    map[int64]uint64 // definition -> tick of its last service
	tick      uint64
	nextStart time.Time

	expired, rejected, unavailable, dropped atomic.Int64
}

// queueable reports whether a trigger of this kind may wait in the queue.
// Startup triggers are waited on by the daemon and never queue.
func (s *Service) queueable(trigger string) bool {
	if s.queueOpt.Disabled {
		return false
	}
	switch trigger {
	case "schedule", "manual", "retry":
		return true
	}
	return false
}

func (s *Service) queueLimits() store.QueueLimits {
	o := s.queueOpt
	return store.QueueLimits{MaxItems: o.MaxItems, MaxPerJob: o.MaxPerJob, MaxBytes: o.MaxBytes, MaxAge: o.MaxAge}
}

// releaseSlot frees a capacity slot and lets the drain loop use it.
func (s *Service) releaseSlot() {
	<-s.capacity
	if s.queue.depth.Load() > 0 {
		signal(s.queue.wake)
	}
}

// enqueue persists a trigger that found no capacity. It returns only after the
// run and its queue row are committed (ADR-9: persist before acknowledging).
func (s *Service) enqueue(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time, idem *IdempotencyRequest, attempt int, parent string) (model.Run, bool, error) {
	id := uuid.NewV7()
	r := model.Run{ID: id.String(), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: hash, Status: "queued", Trigger: trigger, Attempt: attempt, ParentRunID: parent, ScheduledFor: scheduled, BootID: s.bootID, QueuedAt: time.Now().UTC(), LogRef: "file:" + id.String()}
	var key *store.Idempotency
	if idem != nil && idem.Key != "" {
		key = &store.Idempotency{Principal: idem.Principal, Operation: idem.Operation, Key: idem.Key, RequestHash: idem.RequestHash}
	}
	replay, err := s.store.EnqueueRun(ctx, r, key, s.queueLimits(), d.OnOverlap == "skip")
	switch {
	case err == nil && replay != "":
		existing, err := s.store.Run(ctx, replay)
		return existing, true, err
	case err == nil:
		s.queue.depth.Add(1)
		if !s.startLoop(&s.queue.started, s.queueLoop) {
			// Shutting down: the item is durable and drains after restart.
			return r, false, nil
		}
		signal(s.queue.wake)
		return r, false, nil
	case errors.Is(err, store.ErrQueueDuplicate):
		return s.recordSkipped(ctx, d, hash, trigger, scheduled, idem, attempt, parent, "overlap_skip")
	case errors.Is(err, store.ErrQueueFull):
		s.queue.rejected.Add(1)
		if trigger == "retry" {
			// Recording skipped would silently end the retry chain: let the
			// retry scheduler defer it another retry_delay (bounded by
			// max_pending_retries, with drop evidence).
			return model.Run{}, false, errCapacity
		}
		if trigger == "manual" {
			return model.Run{}, false, fmt.Errorf("%w: %w", ErrQueueFull, err)
		}
		return s.recordSkipped(ctx, d, hash, trigger, scheduled, idem, attempt, parent, "queue_full")
	case errors.Is(err, store.ErrIdempotencyConflict), errors.Is(err, store.ErrIdempotencyKeyExists), ctx.Err() != nil:
		return r, false, err
	}
	if trigger == "schedule" && scheduled != nil {
		if existing, lookupErr := s.store.ScheduledRun(ctx, d.ID, *scheduled); lookupErr == nil {
			return existing, true, nil
		}
	}
	s.queue.unavailable.Add(1)
	if trigger == "retry" {
		return model.Run{}, false, errCapacity
	}
	return model.Run{}, false, fmt.Errorf("%w: %w", ErrQueueUnavailable, err)
}

// ResumeQueue counts the persisted queue after a restart and arms the drain
// loop when it is not empty. Items still queued were never started; the usual
// rate, capacity and expiry rules apply, so a backlog cannot start as a storm.
//
// Items left by an earlier run drain (or expire) even if the queue has since been
// disabled: disabling only stops new items from being queued.
func (s *Service) ResumeQueue(ctx context.Context) error {
	st, err := s.store.QueueStats(ctx)
	if err != nil {
		return fmt.Errorf("read execution queue: %w", err)
	}
	s.queue.depth.Store(int64(st.Depth))
	if st.Depth > 0 {
		slog.Info("execution queue recovered", "queued", st.Depth, "bytes", st.Bytes)
		if s.startLoop(&s.queue.started, s.queueLoop) {
			signal(s.queue.wake)
		}
	}
	return nil
}

const (
	queueExpireBatch = 100
	queueHeadWindow  = 64
	queueBackoffMin  = time.Second
	queueBackoffMax  = 30 * time.Second
)

// queueLoop is the single drain goroutine. It sleeps with no timer while the
// queue is empty and otherwise wakes on enqueue, on a freed capacity slot, or at
// the next rate-limit or expiry instant.
func (s *Service) queueLoop() {
	var idle idleTimer
	defer idle.stop()
	s.queue.served = make(map[int64]uint64)
	backoff := queueBackoffMin
	for {
		if s.isClosing() {
			return
		}
		var next time.Duration
		err := fault.Call(func() (err error) { next, err = s.drainStep(); return err })
		if err != nil {
			if s.isClosing() {
				return
			}
			logFailure("execution queue drain failed; retrying", err, "retry_in", backoff)
			next = backoff
			backoff = min(backoff*2, queueBackoffMax)
		} else {
			backoff = queueBackoffMin
		}
		if next == 0 {
			continue
		}
		select {
		case <-s.stopping:
			return
		case <-s.queue.wake:
		case <-idle.arm(next):
		}
	}
}

// drainStep advances the queue by at most one start. It returns how long to
// wait before the next step: 0 to run again at once, > 0 to sleep at most that
// long (a wake ends the sleep early), < 0 to sleep until woken.
func (s *Service) drainStep() (time.Duration, error) {
	if s.queue.depth.Load() <= 0 {
		return -1, nil
	}
	ctx := s.stopCtx
	now := time.Now()
	ids, err := s.store.ExpireQueued(ctx, now, queueExpireBatch)
	if err != nil {
		return 0, fmt.Errorf("expire queued runs: %w", err)
	}
	if n := len(ids); n > 0 {
		s.queue.depth.Add(int64(-n))
		s.queue.expired.Add(int64(n))
		slog.Warn("queued runs expired before capacity freed up", "count", n, "max_age", s.queueOpt.MaxAge)
		if n == queueExpireBatch {
			return 0, nil
		}
	}
	if s.queue.depth.Load() <= 0 {
		return -1, nil
	}
	if wait := time.Until(s.queue.nextStart); wait > 0 {
		return s.untilExpiry(ctx, wait)
	}
	select {
	case s.capacity <- struct{}{}:
	default:
		return s.untilExpiry(ctx, -1) // woken by releaseSlot
	}
	consumedSlot := false
	defer func() {
		if !consumedSlot {
			<-s.capacity
		}
	}()
	if err := s.acquireAdmission(ctx); err != nil {
		return -1, nil // shutdown
	}
	var failedStart *model.Run
	var failedDef model.Definition
	defer func() {
		<-s.admission
		if failedStart != nil {
			s.notifyFinished(*failedStart, failedDef)
			s.scheduleRetry(*failedStart, failedDef)
		}
	}()
	if s.isClosing() {
		return -1, nil
	}
	item, err := s.nextItem(ctx)
	if err != nil {
		return 0, err
	}
	if item == nil {
		// The mirror drifted (for example an expiry raced); resync.
		st, err := s.store.QueueStats(ctx)
		if err != nil {
			return 0, err
		}
		s.queue.depth.Store(int64(st.Depth))
		return 0, nil
	}
	s.queue.tick++
	s.queue.served[item.DefinitionID] = s.queue.tick
	started, def, failed, err := s.startQueued(ctx, item)
	if err != nil {
		return 0, err
	}
	s.queue.depth.Add(-1)
	if len(s.queue.served) > 4096 {
		clear(s.queue.served)
	}
	if started {
		consumedSlot = true
		failedStart, failedDef = failed, def
		s.queue.nextStart = time.Now().Add(time.Second / time.Duration(s.queueOpt.DrainRate))
	}
	return 0, nil
}

// untilExpiry bounds a sleep by the earliest queued expiry, so an expired item
// is ended on time even when capacity never frees up. base < 0 means "no other
// reason to wake".
func (s *Service) untilExpiry(ctx context.Context, base time.Duration) (time.Duration, error) {
	at, ok, err := s.store.NextQueueExpiry(ctx)
	if err != nil {
		return 0, fmt.Errorf("read next queue expiry: %w", err)
	}
	if !ok {
		return base, nil
	}
	until := max(time.Until(at), time.Millisecond)
	if base < 0 || until < base {
		return until, nil
	}
	return base, nil
}

// nextItem picks the head of the least recently served definition (ties by
// age), which gives round-robin fairness across definitions and FIFO within one.
func (s *Service) nextItem(ctx context.Context) (*store.QueueItem, error) {
	heads, err := s.store.QueueHeads(ctx, queueHeadWindow)
	if err != nil {
		return nil, fmt.Errorf("read queue heads: %w", err)
	}
	var best *store.QueueItem
	for i := range heads {
		h := &heads[i]
		if best == nil || s.queue.served[h.DefinitionID] < s.queue.served[best.DefinitionID] {
			best = h
		}
	}
	return best, nil
}

// startQueued revalidates one queued item against the current definition and
// either starts it (started = true, the capacity slot now belongs to the run)
// or ends it as skipped. The caller holds admission and a capacity slot. A
// returned error leaves the item queued.
func (s *Service) startQueued(ctx context.Context, item *store.QueueItem) (started bool, def model.Definition, failedStart *model.Run, err error) {
	run, err := s.store.Run(ctx, item.RunID)
	if err != nil {
		return false, def, nil, fmt.Errorf("read queued run %s: %w", item.RunID, err)
	}
	drop := func(reason string) (bool, model.Definition, *model.Run, error) {
		if err := s.store.DropQueued(ctx, run.ID, reason, time.Now()); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
			return false, def, nil, fmt.Errorf("end queued run %s: %w", run.ID, err)
		}
		s.queue.dropped.Add(1)
		slog.Warn("queued run dropped", "run", run.ID, "job", run.Job, "reason", reason)
		return false, def, nil, nil
	}
	if run.Status != "queued" {
		return drop("queue_inconsistent")
	}
	if time.Now().UnixMicro() >= item.ExpiresUS {
		s.queue.expired.Add(1)
		return drop("queue_expired")
	}
	current, hash, err := s.store.Definition(ctx, run.Job)
	switch {
	case errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist):
		return drop("definition_removed")
	case err != nil:
		return false, def, nil, fmt.Errorf("read queued definition %s: %w", run.Job, err)
	case current.ID != item.DefinitionID || current.Kind != model.KindJob:
		return drop("definition_removed")
	case !current.IsEnabled():
		return drop("definition_disabled")
	case run.Trigger == "retry" && run.Attempt-1 > current.Retries:
		return drop("retry_budget")
	case current.OnOverlap == "skip" && s.Active(current.Name) > 0:
		return drop("overlap_skip")
	}
	if err := s.store.DequeueRun(ctx, run.ID, current.Revision, hash, s.bootID); err != nil {
		if errors.Is(err, store.ErrInvalidTransition) {
			return false, def, nil, nil // ended elsewhere (expiry); nothing to start
		}
		return false, def, nil, fmt.Errorf("dequeue run %s: %w", run.ID, err)
	}
	run.Status, run.Revision, run.DefinitionHash, run.BootID = "pending", current.Revision, hash, s.bootID
	failed, launchErr := s.launch(run, current)
	if launchErr != nil && !errors.Is(launchErr, ErrShutdown) {
		slog.Error("starting queued run failed", "run", run.ID, "job", run.Job, "error", launchErr)
	}
	return true, current, failed, nil
}

// QueueDiagnostics describes the execution queue for diagnostics endpoints.
type QueueDiagnostics struct {
	Enabled          bool    `json:"enabled"`
	Depth            int     `json:"depth"`
	Bytes            int64   `json:"bytes"`
	OldestAgeSeconds float64 `json:"oldest_age_s"`
	MaxItems         int     `json:"max_items"`
	MaxPerJob        int     `json:"max_per_job"`
	MaxBytes         int64   `json:"max_bytes"`
	MaxAgeSeconds    float64 `json:"max_age_s"`
	DrainRate        int     `json:"drain_rate"`
	// Counters count events since the daemon started; durable evidence is the
	// terminal skipped runs they correspond to.
	Expired     int64 `json:"expired"`
	Rejected    int64 `json:"rejected"`
	Unavailable int64 `json:"unavailable"`
	Dropped     int64 `json:"dropped"`
}

// Diagnostics reports the pending-work budgets: the execution queue, retries
// waiting for their delay and runs awaiting terminal persistence.
type Diagnostics struct {
	Queue             QueueDiagnostics `json:"execution_queue"`
	PendingRetries    int              `json:"pending_retries"`
	MaxPendingRetries int              `json:"max_pending_retries"`
	RetriesDropped    int64            `json:"retries_dropped"`
	Finalizers        int              `json:"finalizers"`
	InlineFinalizers  int              `json:"inline_finalizers"`
	MaxFinalizers     int              `json:"max_finalizers"`
}

// Diagnostics computes the report on demand from the read pool.
func (s *Service) Diagnostics(ctx context.Context) Diagnostics {
	o := s.queueOpt
	q := QueueDiagnostics{
		Enabled: !o.Disabled, Depth: int(s.queue.depth.Load()), MaxItems: o.MaxItems, MaxPerJob: o.MaxPerJob, MaxBytes: o.MaxBytes,
		MaxAgeSeconds: o.MaxAge.Seconds(), DrainRate: o.DrainRate, Expired: s.queue.expired.Load(), Rejected: s.queue.rejected.Load(),
		Unavailable: s.queue.unavailable.Load(), Dropped: s.queue.dropped.Load(),
	}
	if !o.Disabled || q.Depth > 0 {
		if st, err := s.store.QueueStatsRead(ctx); err == nil {
			q.Depth, q.Bytes = st.Depth, st.Bytes
			if st.OldestUS > 0 {
				q.OldestAgeSeconds = time.Since(time.UnixMicro(st.OldestUS)).Seconds()
			}
		}
	}
	background, inline := s.Finalizers()
	return Diagnostics{Queue: q, PendingRetries: s.PendingRetries(), MaxPendingRetries: o.MaxPendingRetries, RetriesDropped: s.RetriesDropped(),
		Finalizers: background, InlineFinalizers: inline, MaxFinalizers: o.MaxFinalizers}
}
