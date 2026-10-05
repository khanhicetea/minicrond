package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"uuid"

	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// Options configures the execution service.
type Options struct {
	// MaxConcurrentRuns bounds simultaneously running jobs. Workers are not
	// counted against this limit (they have their own capacity story).
	MaxConcurrentRuns int
	// MaxLineBytes is the global logs.max_line cap applied to every run's
	// single-line truncation. Defaults to 256 KiB when zero.
	MaxLineBytes int64
	// OnFinished is called after a terminal run transition is persisted.
	// Implementations should return quickly and do slow work asynchronously.
	OnFinished func(model.Run, model.Definition)
}

type Service struct {
	store      *store.Store
	logs       *logstore.Store
	bootID     string
	capacity   chan struct{}
	maxLine    int
	onFinished func(model.Run, model.Definition)
	// admission serializes trigger setup with shutdown and overlap checks.
	admission sync.Mutex
	closing   bool
	mu        sync.Mutex
	active    map[string]*activeRun
	byJob     map[string]int
	retryStop chan struct{}
	retryDone chan struct{}
	retryWG   sync.WaitGroup
	// persistLag observes terminal-state persistence latency (diagnostics).
	persistLag persistLag
}
type activeRun struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
	pgid   int
	force  chan struct{}
	forced bool
}

// forcedRunJoin bounds how long shutdown waits for runs to finish cleanup
// after its deadline has forced them to stop.
const forcedRunJoin = 3 * time.Second

var ErrStopped = errors.New("operator stop")
var ErrDisabled = errors.New("definition is disabled")
var ErrShutdown = errors.New("daemon shutdown")

// errCapacity defers a retry attempt that found the concurrency gate full.
// Recording it as skipped would silently end the job's retry budget.
var errCapacity = errors.New("concurrent run capacity is full")
var lookupCurrentUser = user.Current

func New(st *store.Store, logs *logstore.Store, opt Options) *Service {
	if opt.MaxConcurrentRuns <= 0 {
		opt.MaxConcurrentRuns = 32
	}
	if opt.MaxLineBytes <= 0 {
		opt.MaxLineBytes = 256 << 10
	}
	bootID := kernelBootID()
	if bootID == "" {
		bootID = uuid.NewV7().String()
	}
	return &Service{store: st, logs: logs, bootID: bootID, capacity: make(chan struct{}, opt.MaxConcurrentRuns), maxLine: int(opt.MaxLineBytes), onFinished: opt.OnFinished, active: make(map[string]*activeRun), byJob: make(map[string]int), retryStop: make(chan struct{}), retryDone: make(chan struct{})}
}
func (s *Service) Active(job string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.byJob[job] }

// Wait closes after process cleanup, log finalization, and terminal-state
// persistence have been attempted. Persistence failures are logged; callers
// must read the stored state. It returns nil for unknown or finished runs.
func (s *Service) Wait(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.active[id]; a != nil {
		return a.done
	}
	return nil
}

type IdempotencyRequest struct {
	Principal, Operation, Key, RequestHash string
}

func (s *Service) Trigger(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time) (model.Run, error) {
	r, _, err := s.trigger(ctx, d, hash, trigger, scheduled, nil, 1, "")
	return r, err
}

func (s *Service) TriggerIdempotent(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time, idem IdempotencyRequest) (model.Run, bool, error) {
	return s.trigger(ctx, d, hash, trigger, scheduled, &idem, 1, "")
}

func (s *Service) trigger(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time, idem *IdempotencyRequest, attempt int, parent string) (model.Run, bool, error) {
	s.admission.Lock()
	var failedStart *model.Run
	defer func() {
		s.admission.Unlock()
		if failedStart != nil {
			s.notifyFinished(*failedStart, d)
			s.scheduleRetry(*failedStart, d)
		}
	}()
	if s.closing {
		return model.Run{}, false, ErrShutdown
	}
	if err := ctx.Err(); err != nil {
		return model.Run{}, false, err
	}
	if !d.IsEnabled() {
		return model.Run{}, false, fmt.Errorf("definition %s: %w", d.Name, ErrDisabled)
	}
	if d.Kind == model.KindJob && d.OnOverlap == "skip" && s.Active(d.Name) > 0 {
		return s.recordSkipped(ctx, d, hash, trigger, scheduled, idem, attempt, parent, "overlap_skip")
	}
	if d.Kind == model.KindJob {
		select {
		case s.capacity <- struct{}{}:
		default:
			if trigger == "retry" {
				return model.Run{}, false, errCapacity
			}
			return s.recordSkipped(ctx, d, hash, trigger, scheduled, idem, attempt, parent, "queue_full")
		}
	}
	capacityOwned := d.Kind == model.KindJob
	releaseCapacity := func() {
		if capacityOwned {
			<-s.capacity
			capacityOwned = false
		}
	}
	// Admission can be recovered by its caller; release its reservation
	// during unwinding until execution takes ownership.
	defer releaseCapacity()
	id := uuid.NewV7()
	r := model.Run{ID: id.String(), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: hash, Status: "pending", Trigger: trigger, Attempt: attempt, ParentRunID: parent, ScheduledFor: scheduled, BootID: s.bootID, QueuedAt: time.Now().UTC(), LogRef: "file:" + id.String()}
	if idem != nil && idem.Key != "" {
		existingID, err := s.store.AdmitIdempotentRun(ctx, r, idem.Principal, idem.Operation, idem.Key, idem.RequestHash)
		if err != nil {
			releaseCapacity()
			return r, false, err
		}
		if existingID != "" {
			releaseCapacity()
			existing, err := s.store.Run(ctx, existingID)
			return existing, true, err
		}
	} else if err := s.store.CreateRun(ctx, r); err != nil {
		releaseCapacity()
		if trigger == "schedule" && scheduled != nil {
			if existing, lookupErr := s.store.ScheduledRun(ctx, d.ID, *scheduled); lookupErr == nil {
				return existing, true, nil
			}
		}
		return r, false, err
	}
	d = d.Clone()
	maxBytes := resolveLogMax(d.LogMax)
	writer, err := s.logs.Open(r.ID, d.Name, d.Kind, logstore.WriterOptions{MaxBytes: maxBytes, MaxLine: s.maxLine, DropNew: d.LogOnFull == "drop_new"})
	if err != nil {
		ended := time.Now().UTC()
		if finishErr := s.finishRun(r.ID, "failed", "start_error", nil, "", ended, 0, false); finishErr == nil {
			r.Status, r.EndReason, r.EndedAt = "failed", "start_error", &ended
			failedStart = &r
		} else {
			err = errors.Join(err, fmt.Errorf("persist failed start: %w", finishErr))
		}
		releaseCapacity()
		return r, false, fmt.Errorf("open run logs: %w", err)
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	a := &activeRun{cancel: cancel, done: make(chan struct{}), force: make(chan struct{})}
	s.mu.Lock()
	s.active[r.ID] = a
	s.byJob[d.Name]++
	s.mu.Unlock()
	capacityOwned = false // execute releases the slot after full cleanup.
	go s.execute(runCtx, r.Clone(), d, writer, a)
	return r, false, nil
}
func resolveLogMax(value int) int64 {
	if value == 0 {
		value = 100
	}
	return int64(value) << 20
}

// recordSkipped persists a declined trigger. reason distinguishes the job's
// own overlap policy from the daemon-wide concurrency limit.
func (s *Service) recordSkipped(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time, idem *IdempotencyRequest, attempt int, parent, reason string) (model.Run, bool, error) {
	id := uuid.NewV7()
	now := time.Now().UTC()
	r := model.Run{ID: id.String(), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: hash, Status: "skipped", EndReason: reason, Trigger: trigger, Attempt: attempt, ParentRunID: parent, ScheduledFor: scheduled, BootID: s.bootID, QueuedAt: now, EndedAt: &now}
	if idem != nil && idem.Key != "" {
		existingID, err := s.store.AdmitIdempotentRun(ctx, r, idem.Principal, idem.Operation, idem.Key, idem.RequestHash)
		if err != nil {
			return r, false, err
		}
		if existingID != "" {
			existing, err := s.store.Run(ctx, existingID)
			return existing, true, err
		}
		return r, false, nil
	}
	err := s.store.CreateRun(ctx, r)
	if err != nil && trigger == "schedule" && scheduled != nil {
		if existing, lookupErr := s.store.ScheduledRun(ctx, d.ID, *scheduled); lookupErr == nil {
			return existing, true, nil
		}
	}
	return r, false, err
}
func (s *Service) RecordMissed(ctx context.Context, d model.Definition, hash string, count int, scheduled time.Time) (model.Run, error) {
	id := uuid.NewV7()
	now := time.Now().UTC()
	r := model.Run{ID: id.String(), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: hash, Status: "missed", EndReason: "crash_recovery", Trigger: "schedule", Attempt: 1, ScheduledFor: &scheduled, MissedCount: count, BootID: s.bootID, QueuedAt: now, EndedAt: &now}
	err := s.store.CreateRun(ctx, r)
	if err != nil {
		if existing, lookupErr := s.store.ScheduledRun(ctx, d.ID, scheduled); lookupErr == nil {
			return existing, nil
		}
	}
	return r, err
}
func (s *Service) execute(ctx context.Context, r model.Run, d model.Definition, w *logstore.Writer, a *activeRun) {
	defer func() {
		a.cancel(nil)
		s.mu.Lock()
		delete(s.active, r.ID)
		if s.byJob[d.Name] <= 1 {
			delete(s.byJob, d.Name)
		} else {
			s.byJob[d.Name]--
		}
		s.mu.Unlock()
		if d.Kind == model.KindJob {
			<-s.capacity
		}
		close(a.done)
	}()
	if err := fault.Call(func() error {
		s.executeRun(ctx, r, d, w, a)
		return nil
	}); err != nil {
		logFailure("run execution panicked", err, "run", r.ID, "job", d.Name)
		bytes, truncated := w.Stats()
		if closeErr := s.logs.Seal(r.ID); closeErr != nil {
			logFailure("finalizing panicked-run logs failed", closeErr, "run", r.ID)
		}
		s.complete(r, d, terminal{status: "failed", reason: "internal_error", ended: time.Now().UTC(), bytes: bytes, truncated: truncated})
	}
}

func (s *Service) executeRun(ctx context.Context, r model.Run, d model.Definition, w *logstore.Writer, a *activeRun) {
	cmd, identity, err := buildCommand(d, r)
	if err != nil {
		s.finishStartError(r, d, w, err)
		return
	}
	// Caller-owned pipes: cmd.Wait would close StdoutPipe read-ends while
	// the pumps are still draining (lost tail bytes + spurious "file already
	// closed" errors). We own the lifecycle: pumps drain to EOF, then we close.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		s.finishStartError(r, d, w, err)
		return
	}
	defer closePipe(stdoutR)
	defer closePipe(stdoutW)
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		s.finishStartError(r, d, w, err)
		return
	}
	defer closePipe(stderrR)
	defer closePipe(stderrW)
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err = cmd.Start(); err != nil {
		s.finishStartError(r, d, w, err)
		return
	}
	// The child owns its duplicates now; drop the parent's write ends so EOF
	// is reachable once every writer (including descendants) exits.
	closePipe(stdoutW)
	closePipe(stderrW)
	// Capture identity before Wait can reap a fast-exiting child and remove
	// /proc/PID/stat. An exited child retains its procfs entry until reaped.
	startID := processIdentity(cmd.Process.Pid)
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	leaderDone := false
	defer func() {
		// Recovery must clean up descendants even if the leader has exited.
		if !leaderDone || groupAlive(cmd.Process.Pid) {
			killGroup(cmd.Process.Pid, syscall.SIGKILL)
		}
		if !leaderDone {
			<-wait
		}
	}()
	pgid := cmd.Process.Pid
	s.mu.Lock()
	a.pgid = pgid
	s.mu.Unlock()
	started := time.Now().UTC()
	if err = s.store.StartRun(context.Background(), r.ID, cmd.Process.Pid, pgid, startID, started); err != nil {
		killGroup(pgid, syscall.SIGKILL)
		<-wait
		leaderDone = true
		s.finishStartError(r, d, w, fmt.Errorf("persist running state: %w", err))
		return
	}
	r.PID, r.PGID, r.ProcessStartID, r.StartedAt = cmd.Process.Pid, pgid, startID, &started
	writeSystem(w, r.ID, "process started as "+identity)
	pumps := make(chan error, 2)
	pumpsRemaining := 2
	defer func() {
		closePipe(stdoutR)
		closePipe(stderrR)
		for pumpsRemaining > 0 {
			<-pumps
			pumpsRemaining--
		}
	}()
	go func() { pumps <- fault.Call(func() error { return w.Pipe(logstore.Stdout, stdoutR) }) }()
	go func() { pumps <- fault.Call(func() error { return w.Pipe(logstore.Stderr, stderrR) }) }()
	timeout := time.Duration(d.Timeout) * time.Second
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	var waitErr error
	var cause error
	for !leaderDone {
		select {
		case waitErr = <-wait:
			leaderDone = true
		case pumpErr := <-pumps:
			pumpsRemaining--
			if pumpErr != nil && !errors.Is(pumpErr, os.ErrClosed) {
				cause = fmt.Errorf("log pump failed: %w", pumpErr)
				logFailure("run log pump failed", cause, "run", r.ID)
				writeSystem(w, r.ID, "log pump error; stopping process")
				waitErr = stopGroup(pgid, d, wait, a.force)
				leaderDone = true
			}
		case <-timer:
			cause = context.DeadlineExceeded
			writeSystem(w, r.ID, "timeout reached, stopping")
			waitErr = stopGroup(pgid, d, wait, a.force)
			leaderDone = true
		case <-ctx.Done():
			cause = context.Cause(ctx)
			writeSystem(w, r.ID, "stop requested")
			waitErr = stopGroup(pgid, d, wait, a.force)
			leaderDone = true
		}
	}
	// Drain remaining pipe bytes. A descendant that inherited the pipe can
	// keep EOF unreachable; bound the wait, then force-close (the pumps then
	// see a benign os.ErrClosed which we do not report).
	drainDeadline := time.NewTimer(5 * time.Second)
	defer drainDeadline.Stop()
	for pumpsRemaining > 0 {
		select {
		case pumpErr := <-pumps:
			pumpsRemaining--
			if pumpErr != nil && !errors.Is(pumpErr, os.ErrClosed) {
				cause = errors.Join(cause, fmt.Errorf("log pump failed: %w", pumpErr))
				logFailure("run log pump failed", pumpErr, "run", r.ID)
				writeSystem(w, r.ID, "log pump error: "+pumpErr.Error())
			}
		case <-drainDeadline.C:
			writeSystem(w, r.ID, "log pipes still held by descendants; closing")
			killGroup(pgid, syscall.SIGKILL)
			closePipe(stdoutR)
			closePipe(stderrR)
		}
	}
	closePipe(stdoutR)
	closePipe(stderrR)
	if groupAlive(pgid) {
		killGroup(pgid, syscall.SIGKILL)
	}
	status, reason, code, signal := classify(waitErr, cause, d.SuccessCodes)
	if cause != nil && !errors.Is(cause, context.DeadlineExceeded) && !errors.Is(cause, ErrStopped) && !errors.Is(cause, ErrShutdown) {
		status, reason = "failed", "log_error"
	}
	// Close the log sink before the terminal transition so a wait=true
	// reader can never observe a finished run with an unfinalized tail.
	bytes, truncated := w.Stats()
	if err := s.logs.Seal(r.ID); err != nil {
		slog.Error("finalizing run logs failed", "run", r.ID, "error", err)
		status, reason = "failed", "log_error"
	}
	s.complete(r, d, terminal{status: status, reason: reason, code: code, signal: signal, ended: time.Now().UTC(), bytes: bytes, truncated: truncated, retry: true})
}

func (s *Service) finishStartError(r model.Run, d model.Definition, w *logstore.Writer, err error) {
	logFailure("starting run failed", err, "run", r.ID, "job", d.Name)
	writeSystem(w, r.ID, "start error: "+err.Error())
	bytes, truncated := w.Stats()
	if closeErr := s.logs.Seal(r.ID); closeErr != nil {
		slog.Error("finalizing failed-run logs failed", "run", r.ID, "error", closeErr)
	}
	s.complete(r, d, terminal{status: "failed", reason: "start_error", ended: time.Now().UTC(), bytes: bytes, truncated: truncated, retry: true})
}

// terminal is the outcome of a run that has stopped executing.
type terminal struct {
	status, reason string
	code           *int
	signal         string
	ended          time.Time
	bytes          int64
	truncated      bool
	retry          bool // whether a failed job may schedule its next attempt
}

// complete persists a run's terminal state, then notifies and schedules any
// retry. If storage stays unavailable past the inline budget, a background
// finalizer keeps trying, so the run cannot stay "running" with no alert.
func (s *Service) complete(r model.Run, d model.Definition, t terminal) {
	err := s.finishRun(r.ID, t.status, t.reason, t.code, t.signal, t.ended, t.bytes, t.truncated)
	if err == nil {
		s.finished(r, d, t)
		return
	}
	if errors.Is(err, store.ErrInvalidTransition) {
		logFailure("persisting terminal run state rejected", err, "run", r.ID, "status", t.status)
		return
	}
	logFailure("persisting terminal run state failed; retrying in background", err, "run", r.ID, "status", t.status)
	s.admission.Lock()
	if s.closing {
		// Startup recovery marks the run interrupted.
		s.admission.Unlock()
		return
	}
	s.retryWG.Add(1)
	s.admission.Unlock()
	pendingToken := s.persistLag.startPending(t.ended)
	go func() {
		defer s.retryWG.Done()
		defer s.persistLag.endPending(pendingToken)
		if err := fault.Call(func() error {
			s.finalizeLater(r, d, t)
			return nil
		}); err != nil {
			logFailure("run finalizer panicked", err, "run", r.ID)
		}
	}()
}

func (s *Service) finalizeLater(r model.Run, d model.Definition, t terminal) {
	backoff := time.Second
	for {
		timer := time.NewTimer(backoff)
		select {
		case <-s.retryStop:
			timer.Stop()
			slog.Warn("run left unfinalized at shutdown; recovery will mark it interrupted", "run", r.ID)
			return
		case <-timer.C:
		}
		err := s.finishRun(r.ID, t.status, t.reason, t.code, t.signal, t.ended, t.bytes, t.truncated)
		if err == nil {
			slog.Info("persisted terminal run state after retry", "run", r.ID, "status", t.status)
			s.finished(r, d, t)
			return
		}
		if errors.Is(err, store.ErrInvalidTransition) {
			logFailure("persisting terminal run state rejected", err, "run", r.ID, "status", t.status)
			return
		}
		logFailure("persisting terminal run state failed; retrying", err, "run", r.ID, "status", t.status, "retry_in", min(backoff*2, 30*time.Second))
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Service) finished(r model.Run, d model.Definition, t terminal) {
	s.persistLag.observe(t.ended)
	r.Status, r.EndReason, r.ExitCode, r.Signal, r.EndedAt = t.status, t.reason, t.code, t.signal, &t.ended
	s.notifyFinished(r, d)
	if t.retry {
		s.scheduleRetry(r, d)
	}
}

// scheduleRetry creates the next attempt as a separate run. Pending timers
// are canceled at shutdown; only failed job runs are retried.
func (s *Service) scheduleRetry(r model.Run, d model.Definition) {
	if d.Kind != model.KindJob || r.Status != "failed" || r.Attempt > d.Retries {
		return
	}
	s.admission.Lock()
	if s.closing {
		s.admission.Unlock()
		return
	}
	s.retryWG.Add(1)
	s.admission.Unlock()
	go func() {
		defer s.retryWG.Done()
		if err := fault.Call(func() error {
			s.retry(r, d)
			return nil
		}); err != nil {
			logFailure("job retry panicked", err, "run", r.ID, "job", d.Name)
		}
	}()
}

func (s *Service) retry(r model.Run, d model.Definition) {
	delay := d.RetryDelay
	if delay <= 0 {
		delay = 5
	}
	timer := time.NewTimer(time.Duration(delay) * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.retryStop:
			return
		case <-timer.C:
		}
		// Use the current definition: disabled, deleted, or reduced budgets
		// must not launch a queued retry.
		current, hash, err := s.store.Definition(context.Background(), d.Name)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, sql.ErrNoRows) {
				logFailure("reading retry definition failed", err, "job", d.Name, "run", r.ID)
			}
			return
		}
		if current.ID != d.ID || current.Kind != model.KindJob || !current.IsEnabled() || r.Attempt > current.Retries {
			return
		}
		_, _, err = s.trigger(context.Background(), current, hash, "retry", r.ScheduledFor, nil, r.Attempt+1, r.ID)
		if errors.Is(err, errCapacity) {
			// Wait another retry delay for a free slot instead of dropping the attempt.
			slog.Info("job retry deferred: concurrent run capacity is full", "job", d.Name, "run", r.ID, "attempt", r.Attempt+1)
			timer.Reset(time.Duration(delay) * time.Second)
			continue
		}
		if err != nil && !errors.Is(err, ErrShutdown) {
			slog.Error("job retry trigger failed", "job", d.Name, "run", r.ID, "error", err)
		}
		return
	}
}

func (s *Service) finishRun(id, status, reason string, code *int, signal string, ended time.Time, bytes int64, truncated bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for attempt := range 5 {
		err = s.store.FinishRun(ctx, id, status, reason, code, signal, ended, bytes, truncated)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("persist terminal run state: %w", err)
		}
		if attempt < 4 {
			timer := time.NewTimer(time.Duration(1<<attempt) * 50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("persist terminal run state: %w", errors.Join(err, ctx.Err()))
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("persist terminal run state: %w", err)
}

func (s *Service) notifyFinished(r model.Run, d model.Definition) {
	if s.onFinished != nil {
		if err := fault.Call(func() error {
			s.onFinished(r.Clone(), d.Clone())
			return nil
		}); err != nil {
			logFailure("run completion callback panicked", err, "run", r.ID, "job", d.Name)
		}
	}
}
func buildCommand(d model.Definition, r model.Run) (*exec.Cmd, string, error) {
	cred, home, label, err := identity(d.RunAs)
	if err != nil {
		return nil, "", err
	}
	env, err := environment(d, home, r)
	if err != nil {
		return nil, "", err
	}
	program := d.Shell
	args := []string{"-c", d.Command}
	if len(d.Argv) > 0 {
		program, args = d.Argv[0], d.Argv[1:]
	}
	program, err = trustedExecutable(program)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.Command(program, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
	// A numeric UID without a passwd entry has no known home. Use only an
	// explicit job HOME for home-relative paths, never the daemon's ambient HOME.
	if home == "" {
		for _, entry := range env {
			if value, ok := strings.CutPrefix(entry, "HOME="); ok {
				home = value
			}
		}
		if home != "" && !filepath.IsAbs(home) {
			return nil, "", errors.New("env.HOME must be an absolute path when the daemon UID has no passwd home")
		}
	}
	dir := d.WorkingDir
	if dir == "" || dir == "~" {
		dir = home
	}
	if after, ok := strings.CutPrefix(dir, "~/"); ok {
		if home == "" {
			return nil, "", errors.New("working_dir uses ~ but HOME is unavailable; set env.HOME or an absolute working_dir")
		}
		dir = filepath.Join(home, after)
	}
	if d.WorkingDir == "~" && home == "" {
		return nil, "", errors.New("working_dir uses ~ but HOME is unavailable; set env.HOME or an absolute working_dir")
	}
	cmd.Dir = dir
	cmd.Env = env
	return cmd, label, nil
}
func trustedExecutable(program string) (string, error) {
	if filepath.IsAbs(program) {
		return program, nil
	}
	if strings.ContainsRune(program, filepath.Separator) {
		return "", errors.New("executable path must be absolute or a bare trusted-path name")
	}
	for _, dir := range []string{"/usr/bin", "/bin"} {
		candidate := filepath.Join(dir, program)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in trusted PATH", program)
}

func identity(runAs string) (cred *syscall.Credential, home, label string, err error) {
	// Config validation rejects this too. Keep the spawn path guarded so a
	// definition persisted before that validation change cannot bypass it.
	if runAs != "" && os.Geteuid() != 0 {
		return nil, "", "", errors.New("run_as requires a root daemon")
	}
	if runAs == "" {
		current, lookupErr := lookupCurrentUser()
		if lookupErr == nil {
			return nil, current.HomeDir, current.Username, nil
		}
		return nil, "", strconv.Itoa(os.Geteuid()), nil
	}
	var u *user.User
	groupName := ""
	if runAs != "" {
		name, g, _ := strings.Cut(runAs, ":")
		groupName = g
		if _, cerr := strconv.Atoi(name); cerr == nil {
			if u, err = user.LookupId(name); err != nil {
				return nil, "", "", err
			}
		} else if u, err = user.Lookup(name); err != nil {
			return nil, "", "", err
		}
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, "", "", fmt.Errorf("parse user uid: %w", err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, "", "", fmt.Errorf("parse user gid: %w", err)
	}
	if groupName != "" {
		g, gerr := user.LookupGroup(groupName)
		if gerr != nil {
			if g, gerr = user.LookupGroupId(groupName); gerr != nil {
				return nil, "", "", gerr
			}
		}
		gid, err = strconv.ParseUint(g.Gid, 10, 32)
		if err != nil {
			return nil, "", "", fmt.Errorf("parse group gid: %w", err)
		}
	}
	if os.Geteuid() == 0 || uint32(uid) != uint32(os.Geteuid()) {
		cred = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	return cred, u.HomeDir, u.Username, nil
}
func environment(d model.Definition, home string, r model.Run) ([]string, error) {
	env := []string{"PATH=/usr/bin:/bin", "TZ=" + d.Timezone}
	if home != "" {
		env = append(env, "HOME="+home)
	}
	if d.EnvBase == "inherit" {
		env = slices.DeleteFunc(os.Environ(), func(v string) bool {
			return strings.HasPrefix(v, "MINICRON_") || (home == "" && (strings.HasPrefix(v, "HOME=") || strings.HasPrefix(v, "USER=") || strings.HasPrefix(v, "LOGNAME=")))
		})
		env = append(env, "TZ="+d.Timezone)
	}
	fileEnv, err := readEnvFile(d.EnvFile)
	if err != nil {
		return nil, fmt.Errorf("read env_file: %w", err)
	}
	for k, v := range fileEnv {
		env = append(env, k+"="+v)
	}
	for k, v := range d.Env {
		env = append(env, k+"="+v)
	}
	for k, ref := range d.SecretEnv {
		value, err := resolveSecret(ref)
		if err != nil {
			return nil, fmt.Errorf("resolve secret_env %s: %w", k, err)
		}
		env = append(env, k+"="+value)
	}
	env = append(env, "MINICRON_JOB="+d.Name, "MINICRON_RUN_ID="+r.ID, "MINICRON_TRIGGER="+r.Trigger, "MINICRON_ATTEMPT="+strconv.Itoa(r.Attempt))
	return env, nil
}
func readEnvFile(path string) (map[string]string, error) {
	out := make(map[string]string)
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("env_file exceeds 1 MiB")
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}
func resolveSecret(ref string) (string, error) {
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		v, found := os.LookupEnv(name)
		if !found {
			return "", errors.New("required environment variable is unset")
		}
		return v, nil
	}
	if path, ok := strings.CutPrefix(ref, "file:"); ok {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
		if err != nil {
			return "", err
		}
		if len(b) > 1<<20 {
			return "", errors.New("secret file exceeds 1 MiB")
		}
		return strings.TrimSuffix(string(b), "\n"), nil
	}
	return "", errors.New("invalid secret reference")
}
func stopGroup(pgid int, d model.Definition, wait <-chan error, force <-chan struct{}) error {
	grace := time.Duration(d.Grace) * time.Second
	if grace <= 0 {
		killGroup(pgid, syscall.SIGKILL)
		return <-wait
	}
	killGroup(pgid, parseSignal(d.StopSignal))
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var leaderErr error
	leaderDone := false
	for {
		select {
		case err := <-wait:
			leaderErr, leaderDone = err, true
			if !groupAlive(pgid) {
				return leaderErr
			}
		case <-ticker.C:
			if leaderDone && !groupAlive(pgid) {
				return leaderErr
			}
		case <-force:
			killGroup(pgid, syscall.SIGKILL)
			if leaderDone {
				return leaderErr
			}
			return <-wait
		case <-timer.C:
			killGroup(pgid, syscall.SIGKILL)
			if leaderDone {
				return leaderErr
			}
			return <-wait
		}
	}
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
func killGroup(pgid int, sig syscall.Signal) {
	// A zero/negative PGID can signal the daemon's own group or all processes.
	if pgid <= 0 {
		slog.Error("refusing invalid process group signal", "pgid", pgid, "signal", sig)
		return
	}
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Error("signaling process group failed", "pgid", pgid, "signal", sig, "error", err)
	}
}
func parseSignal(v string) syscall.Signal {
	switch strings.TrimPrefix(v, "SIG") {
	case "INT":
		return syscall.SIGINT
	case "HUP":
		return syscall.SIGHUP
	case "QUIT":
		return syscall.SIGQUIT
	case "USR1":
		return syscall.SIGUSR1
	case "USR2":
		return syscall.SIGUSR2
	case "KILL":
		return syscall.SIGKILL
	default:
		return syscall.SIGTERM
	}
}
func classify(err, cause error, success []int) (string, string, *int, string) {
	code := 0
	if err == nil {
		if errors.Is(cause, context.DeadlineExceeded) {
			return "timeout", "timeout", &code, ""
		}
		if cause != nil {
			return "stopped", "stop_signal", &code, ""
		}
		if slices.Contains(success, code) {
			return "succeeded", "exit", &code, ""
		}
		return "failed", "exit_nonzero", &code, ""
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exit.ExitCode()
		signal := ""
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			signal = ws.Signal().String()
		}
		if errors.Is(cause, context.DeadlineExceeded) {
			return "timeout", "timeout", &code, signal
		}
		if cause != nil {
			return "stopped", "stop_signal", &code, signal
		}
		if slices.Contains(success, code) {
			return "succeeded", "exit", &code, signal
		}
		if signal != "" {
			return "failed", "signal", &code, signal
		}
		return "failed", "exit_nonzero", &code, ""
	}
	return "failed", "start_error", nil, ""
}
func kernelBootID() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func processIdentity(pid int) string {
	if runtime.GOOS != "linux" {
		return strconv.Itoa(pid)
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	return processStartID(string(b))
}

// processStartID extracts field 22 of /proc/PID/stat. The command name in
// field 2 can contain spaces and parentheses, so it cannot be split on spaces.
func processStartID(stat string) string {
	_, rest, ok := strings.CutLast(stat, ")")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) > 19 {
		return fields[19]
	}
	return ""
}

func (s *Service) CleanupRecovered(runs []model.Run) {
	if runtime.GOOS != "linux" {
		return
	}
	for _, r := range runs {
		if r.BootID == s.bootID && r.PGID > 0 && r.PID > 0 && r.ProcessStartID != "" && processIdentity(r.PID) == r.ProcessStartID {
			killGroup(r.PGID, syscall.SIGTERM)
			time.Sleep(100 * time.Millisecond)
			killGroup(r.PGID, syscall.SIGKILL)
		}
	}
}
func (s *Service) Stop(id string) error {
	s.mu.Lock()
	a := s.active[id]
	s.mu.Unlock()
	if a == nil {
		return os.ErrNotExist
	}
	a.cancel(ErrStopped)
	return nil
}
func (s *Service) Shutdown(ctx context.Context) error {
	s.admission.Lock()
	if !s.closing {
		s.closing = true
		close(s.retryStop)
		go func() {
			s.retryWG.Wait()
			close(s.retryDone)
		}()
	}
	s.mu.Lock()
	runs := make([]*activeRun, 0, len(s.active))
	for _, a := range s.active {
		runs = append(runs, a)
		a.cancel(ErrShutdown)
	}
	s.mu.Unlock()
	s.admission.Unlock()
	for _, a := range runs {
		select {
		case <-a.done:
		case <-ctx.Done():
			s.mu.Lock()
			for _, remaining := range runs {
				if !remaining.forced {
					remaining.forced = true
					close(remaining.force)
				}
				if remaining.pgid > 0 {
					killGroup(remaining.pgid, syscall.SIGKILL)
				}
			}
			s.mu.Unlock()
			// Killed runs still drain pipes, finalize logs, and persist their
			// terminal state. Give them a short, bounded join so they do not
			// race the daemon closing its databases.
			join := time.NewTimer(forcedRunJoin)
			defer join.Stop()
			for _, remaining := range runs {
				select {
				case <-remaining.done:
				case <-join.C:
					return ctx.Err()
				}
			}
			return ctx.Err()
		}
	}
	select {
	case <-s.retryDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func logFailure(message string, err error, attrs ...any) {
	attrs = append(attrs, "error", err)
	if panicErr, ok := errors.AsType[*fault.PanicError](err); ok {
		attrs = append(attrs, "stack", string(panicErr.Stack))
	}
	slog.Error(message, attrs...)
}

func writeSystem(w *logstore.Writer, runID, message string) {
	if err := w.Write(logstore.System, []byte(message), 0); err != nil {
		logFailure("writing run system log failed", err, "run", runID)
	}
}

func closePipe(pipe *os.File) {
	if err := pipe.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		slog.Error("closing process pipe failed", "error", err)
	}
}
