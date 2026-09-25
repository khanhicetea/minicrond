package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	retryWG   sync.WaitGroup
}
type activeRun struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
	pgid   int
}

var ErrStopped = errors.New("operator stop")
var ErrShutdown = errors.New("daemon shutdown")
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
		var b [16]byte
		_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail on supported platforms
		bootID = hex.EncodeToString(b[:])
	}
	return &Service{store: st, logs: logs, bootID: bootID, capacity: make(chan struct{}, opt.MaxConcurrentRuns), maxLine: int(opt.MaxLineBytes), onFinished: opt.OnFinished, active: make(map[string]*activeRun), byJob: make(map[string]int), retryStop: make(chan struct{})}
}
func (s *Service) Active(job string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.byJob[job] }

// Wait returns a channel that closes once the run reaches its terminal
// state and its log sink is finalized. It returns nil when the run is
// unknown or already finished; callers should then read the store directly.
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
	defer s.admission.Unlock()
	if s.closing {
		return model.Run{}, false, ErrShutdown
	}
	if err := ctx.Err(); err != nil {
		return model.Run{}, false, err
	}
	if !d.IsEnabled() {
		return model.Run{}, false, fmt.Errorf("definition %s is disabled", d.Name)
	}
	if d.Kind == model.KindJob && d.OnOverlap == "skip" && s.Active(d.Name) > 0 {
		return s.recordSkipped(ctx, d, hash, trigger, scheduled, idem, attempt, parent)
	}
	if d.Kind == model.KindJob {
		select {
		case s.capacity <- struct{}{}:
		default:
			return s.recordSkipped(ctx, d, hash, trigger, scheduled, idem, attempt, parent)
		}
	}
	releaseCapacity := func() {
		if d.Kind == model.KindJob {
			<-s.capacity
		}
	}
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
	maxBytes := resolveLogMax(d.LogMax)
	writer, err := s.logs.Open(r.ID, d.Name, d.Kind, logstore.WriterOptions{MaxBytes: maxBytes, MaxLine: s.maxLine, DropNew: d.LogOnFull == "drop_new"})
	if err != nil {
		ended := time.Now().UTC()
		if finishErr := s.store.FinishRun(ctx, r.ID, "failed", "start_error", nil, "", ended, 0, false); finishErr == nil {
			r.Status, r.EndReason, r.EndedAt = "failed", "start_error", &ended
			s.notifyFinished(r, d)
			// trigger holds admission until it returns; schedule outside it.
			go s.scheduleRetry(r, d)
		}
		releaseCapacity()
		return r, false, err
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	a := &activeRun{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.active[r.ID] = a
	s.byJob[d.Name]++
	s.mu.Unlock()
	go s.execute(runCtx, r, d, writer, a)
	return r, false, nil
}
func resolveLogMax(value int) int64 {
	if value == 0 {
		value = 100
	}
	return int64(value) << 20
}

func (s *Service) recordSkipped(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time, idem *IdempotencyRequest, attempt int, parent string) (model.Run, bool, error) {
	id := uuid.NewV7()
	now := time.Now().UTC()
	r := model.Run{ID: id.String(), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: hash, Status: "skipped", EndReason: "overlap_skip", Trigger: trigger, Attempt: attempt, ParentRunID: parent, ScheduledFor: scheduled, BootID: s.bootID, QueuedAt: now, EndedAt: &now}
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
		s.logs.Close(r.ID)
		s.mu.Lock()
		delete(s.active, r.ID)
		s.byJob[d.Name]--
		s.mu.Unlock()
		if d.Kind == model.KindJob {
			<-s.capacity
		}
		close(a.done)
	}()
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
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		s.finishStartError(r, d, w, err)
		return
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err = cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		s.finishStartError(r, d, w, err)
		return
	}
	// The child owns its duplicates now; drop the parent's write ends so EOF
	// is reachable once every writer (including descendants) exits.
	stdoutW.Close()
	stderrW.Close()
	pgid := cmd.Process.Pid
	started := time.Now().UTC()
	startID := processIdentity(cmd.Process.Pid)
	if err = s.store.StartRun(context.Background(), r.ID, cmd.Process.Pid, pgid, startID, started); err != nil {
		killGroup(pgid, syscall.SIGKILL)
		cmd.Wait()
		stdoutR.Close()
		stderrR.Close()
		s.finishStartError(r, d, w, fmt.Errorf("persist running state: %w", err))
		return
	}
	r.PID, r.PGID, r.ProcessStartID, r.StartedAt = cmd.Process.Pid, pgid, startID, &started
	s.mu.Lock()
	a.pgid = pgid
	s.mu.Unlock()
	_ = w.Write(logstore.System, []byte("process started as "+identity), 0)
	pumps := make(chan error, 2)
	go func() { pumps <- w.Pipe(logstore.Stdout, stdoutR) }()
	go func() { pumps <- w.Pipe(logstore.Stderr, stderrR) }()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	timeout := time.Duration(d.Timeout) * time.Second
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	var waitErr error
	var cause error
	pumpsRemaining := 2
	select {
	case waitErr = <-wait:
	case pumpErr := <-pumps:
		pumpsRemaining--
		if pumpErr != nil && !errors.Is(pumpErr, os.ErrClosed) {
			cause = fmt.Errorf("log pump failed: %w", pumpErr)
			_ = w.Write(logstore.System, []byte("log pump error; stopping process"), 0)
			waitErr = stopGroup(pgid, d, wait)
		} else {
			waitErr = <-wait
		}
	case <-timer:
		cause = context.DeadlineExceeded
		_ = w.Write(logstore.System, []byte("timeout reached, stopping"), 0)
		waitErr = stopGroup(pgid, d, wait)
	case <-ctx.Done():
		cause = context.Cause(ctx)
		_ = w.Write(logstore.System, []byte("stop requested"), 0)
		waitErr = stopGroup(pgid, d, wait)
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
				w.Write(logstore.System, []byte("log pump error: "+pumpErr.Error()), 0)
			}
		case <-drainDeadline.C:
			w.Write(logstore.System, []byte("log pipes still held by descendants; closing"), 0)
			stdoutR.Close()
			stderrR.Close()
		}
	}
	stdoutR.Close()
	stderrR.Close()
	status, reason, code, signal := classify(waitErr, cause, d.SuccessCodes)
	if cause != nil && !errors.Is(cause, context.DeadlineExceeded) && !errors.Is(cause, ErrStopped) && !errors.Is(cause, ErrShutdown) {
		status, reason = "failed", "log_error"
	}
	// Close the log sink before the terminal transition so a wait=true
	// reader can never observe a finished run with an unfinalized tail.
	bytes, truncated := w.Stats()
	if err := s.logs.Close(r.ID); err != nil {
		slog.Error("finalizing run logs failed", "run", r.ID, "error", err)
		status, reason = "failed", "log_error"
	}
	ended := time.Now().UTC()
	if err := s.finishRun(r.ID, status, reason, code, signal, ended, bytes, truncated); err != nil {
		slog.Error("persisting terminal run state failed after retries", "run", r.ID, "status", status, "error", err)
		return
	}
	r.Status, r.EndReason, r.ExitCode, r.Signal, r.EndedAt = status, reason, code, signal, &ended
	s.notifyFinished(r, d)
	s.scheduleRetry(r, d)
}

func (s *Service) finishStartError(r model.Run, d model.Definition, w *logstore.Writer, err error) {
	w.Write(logstore.System, []byte("start error: "+err.Error()), 0)
	bytes, truncated := w.Stats()
	if closeErr := s.logs.Close(r.ID); closeErr != nil {
		slog.Error("finalizing failed-run logs failed", "run", r.ID, "error", closeErr)
	}
	ended := time.Now().UTC()
	if ferr := s.finishRun(r.ID, "failed", "start_error", nil, "", ended, bytes, truncated); ferr != nil {
		slog.Error("persisting failed run state failed after retries", "run", r.ID, "error", ferr)
		return
	}
	r.Status, r.EndReason, r.EndedAt = "failed", "start_error", &ended
	s.notifyFinished(r, d)
	s.scheduleRetry(r, d)
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
		delay := d.RetryDelay
		if delay <= 0 {
			delay = 5
		}
		timer := time.NewTimer(time.Duration(delay) * time.Second)
		defer timer.Stop()
		select {
		case <-s.retryStop:
			return
		case <-timer.C:
		}
		// Use the current definition: disabled, deleted, or reduced budgets
		// must not launch a queued retry.
		current, hash, err := s.store.Definition(context.Background(), d.Name)
		if err != nil || current.ID != d.ID || current.Kind != model.KindJob || !current.IsEnabled() || r.Attempt > current.Retries {
			return
		}
		if _, _, err := s.trigger(context.Background(), current, hash, "retry", r.ScheduledFor, nil, r.Attempt+1, r.ID); err != nil && !errors.Is(err, ErrShutdown) {
			slog.Error("job retry trigger failed", "job", d.Name, "run", r.ID, "error", err)
		}
	}()
}

func (s *Service) finishRun(id, status, reason string, code *int, signal string, ended time.Time, bytes int64, truncated bool) error {
	var err error
	for attempt := range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = s.store.FinishRun(ctx, id, status, reason, code, signal, ended, bytes, truncated)
		cancel()
		if err == nil {
			return nil
		}
		if attempt < 4 {
			time.Sleep(time.Duration(1<<attempt) * 50 * time.Millisecond)
		}
	}
	return err
}

func (s *Service) notifyFinished(r model.Run, d model.Definition) {
	if s.onFinished != nil {
		s.onFinished(r, d)
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
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	if groupName != "" {
		g, gerr := user.LookupGroup(groupName)
		if gerr != nil {
			if g, gerr = user.LookupGroupId(groupName); gerr != nil {
				return nil, "", "", gerr
			}
		}
		gid, _ = strconv.ParseUint(g.Gid, 10, 32)
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
	env = append(env, "MINICRON_JOB="+d.Name, "MINICRON_RUN_ID="+r.ID, "MINICRON_TRIGGER="+r.Trigger, "MINICRON_ATTEMPT=1")
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
func stopGroup(pgid int, d model.Definition, wait <-chan error) error {
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
func killGroup(pgid int, sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }
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
			for _, remaining := range runs {
				if remaining.pgid > 0 {
					killGroup(remaining.pgid, syscall.SIGKILL)
				}
			}
			return ctx.Err()
		}
	}
	s.retryWG.Wait()
	return nil
}
