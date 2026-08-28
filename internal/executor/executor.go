package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

	"github.com/google/uuid"

	"github.com/minicron/minicron/internal/logstore"
	"github.com/minicron/minicron/internal/model"
	"github.com/minicron/minicron/internal/store"
)

type Service struct {
	store    *store.Store
	logs     *logstore.Store
	bootID   string
	capacity chan struct{}
	mu       sync.Mutex
	active   map[string]*activeRun
	byJob    map[string]int
}
type activeRun struct {
	cancel context.CancelCauseFunc
	pgid   int
	done   chan struct{}
}

var ErrStopped = errors.New("operator stop")
var ErrShutdown = errors.New("daemon shutdown")

func New(st *store.Store, logs *logstore.Store, maxJobs int) *Service {
	var b [16]byte
	rand.Read(b[:])
	return &Service{store: st, logs: logs, bootID: hex.EncodeToString(b[:]), capacity: make(chan struct{}, maxJobs), active: make(map[string]*activeRun), byJob: make(map[string]int)}
}
func (s *Service) Active(job string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.byJob[job] }
func (s *Service) Trigger(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time) (model.Run, error) {
	if !d.IsEnabled() {
		return model.Run{}, fmt.Errorf("definition %s is disabled", d.Name)
	}
	if d.Kind == model.KindJob && d.OnOverlap == "skip" && s.Active(d.Name) > 0 {
		return s.recordSkipped(ctx, d, hash, trigger, scheduled)
	}
	if d.Kind == model.KindJob {
		select {
		case s.capacity <- struct{}{}:
		default:
			return s.recordSkipped(ctx, d, hash, trigger, scheduled)
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		if d.Kind == model.KindJob {
			<-s.capacity
		}
		return model.Run{}, err
	}
	r := model.Run{ID: id.String(), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: hash, Status: "pending", Trigger: trigger, Attempt: 1, ScheduledFor: scheduled, BootID: s.bootID, QueuedAt: time.Now().UTC(), LogRef: "file:" + id.String()}
	if err := s.store.CreateRun(ctx, r); err != nil {
		if d.Kind == model.KindJob {
			<-s.capacity
		}
		return r, err
	}
	maxBytes, _ := logstore.ParseBytes(d.LogMax)
	if maxBytes == 0 {
		maxBytes = 100 << 20
	}
	writer, err := s.logs.Open(r.ID, maxBytes, 256<<10)
	if err != nil {
		s.store.FinishRun(ctx, r.ID, "failed", "start_error", nil, "", time.Now(), 0, false)
		if d.Kind == model.KindJob {
			<-s.capacity
		}
		return r, err
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	a := &activeRun{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.active[r.ID] = a
	s.byJob[d.Name]++
	s.mu.Unlock()
	go s.execute(runCtx, r, d, writer, a)
	return r, nil
}
func (s *Service) recordSkipped(ctx context.Context, d model.Definition, hash, trigger string, scheduled *time.Time) (model.Run, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return model.Run{}, err
	}
	now := time.Now().UTC()
	r := model.Run{ID: id.String(), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: hash, Status: "skipped", EndReason: "overlap_skip", Trigger: trigger, Attempt: 1, ScheduledFor: scheduled, BootID: s.bootID, QueuedAt: now, EndedAt: &now}
	err = s.store.CreateRun(ctx, r)
	return r, err
}
func (s *Service) RecordMissed(ctx context.Context, d model.Definition, hash string, count int, scheduled time.Time) (model.Run, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return model.Run{}, err
	}
	now := time.Now().UTC()
	r := model.Run{ID: id.String(), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: hash, Status: "missed", EndReason: "crash_recovery", Trigger: "schedule", Attempt: 1, ScheduledFor: &scheduled, MissedCount: count, BootID: s.bootID, QueuedAt: now, EndedAt: &now}
	err = s.store.CreateRun(ctx, r)
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
		s.finishStartError(r, w, err)
		return
	}
	// Caller-owned pipes: cmd.Wait would close StdoutPipe read-ends while
	// the pumps are still draining (lost tail bytes + spurious "file already
	// closed" errors). We own the lifecycle: pumps drain to EOF, then we close.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		s.finishStartError(r, w, err)
		return
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		s.finishStartError(r, w, err)
		return
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err = cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		s.finishStartError(r, w, err)
		return
	}
	// The child owns its duplicates now; drop the parent's write ends so EOF
	// is reachable once every writer (including descendants) exits.
	stdoutW.Close()
	stderrW.Close()
	pgid := cmd.Process.Pid
	a.pgid = pgid
	started := time.Now().UTC()
	startID := processIdentity(cmd.Process.Pid)
	if err = s.store.StartRun(context.Background(), r.ID, cmd.Process.Pid, pgid, startID, started); err != nil {
		killGroup(pgid, syscall.SIGKILL)
		cmd.Wait()
		stdoutR.Close()
		stderrR.Close()
		s.finishStartError(r, w, fmt.Errorf("persist running state: %w", err))
		return
	}
	w.Write(logstore.System, []byte("process started as "+identity), 0)
	pumps := make(chan error, 2)
	go func() { pumps <- w.Pipe(logstore.Stdout, stdoutR) }()
	go func() { pumps <- w.Pipe(logstore.Stderr, stderrR) }()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	timeout, _ := time.ParseDuration(d.Timeout)
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	var waitErr error
	var cause error
	select {
	case waitErr = <-wait:
	case <-timer:
		cause = context.DeadlineExceeded
		w.Write(logstore.System, []byte("timeout reached, stopping"), 0)
		waitErr = stopGroup(pgid, d, wait)
	case <-ctx.Done():
		cause = context.Cause(ctx)
		w.Write(logstore.System, []byte("stop requested"), 0)
		waitErr = stopGroup(pgid, d, wait)
	}
	// Drain remaining pipe bytes. A descendant that inherited the pipe can
	// keep EOF unreachable; bound the wait, then force-close (the pumps then
	// see a benign os.ErrClosed which we do not report).
	drainDeadline := time.NewTimer(5 * time.Second)
	defer drainDeadline.Stop()
	for range 2 {
		select {
		case pumpErr := <-pumps:
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
	// Close the log sink before the terminal transition so a wait=true
	// reader can never observe a finished run with an unfinalized tail.
	bytes, truncated := w.Stats()
	_ = w.Close()
	s.store.FinishRun(context.Background(), r.ID, status, reason, code, signal, time.Now().UTC(), bytes, truncated)
}
func (s *Service) finishStartError(r model.Run, w *logstore.Writer, err error) {
	w.Write(logstore.System, []byte("start error: "+err.Error()), 0)
	bytes, truncated := w.Stats()
	_ = w.Close()
	s.store.FinishRun(context.Background(), r.ID, "failed", "start_error", nil, "", time.Now().UTC(), bytes, truncated)
}
func buildCommand(d model.Definition, r model.Run) (*exec.Cmd, string, error) {
	var cmd *exec.Cmd
	if len(d.Argv) > 0 {
		cmd = exec.Command(d.Argv[0], d.Argv[1:]...)
	} else {
		cmd = exec.Command(d.Shell, "-c", d.Command)
	}
	u, cred, home, label, err := identity(d.RunAs)
	if err != nil {
		return nil, "", err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
	dir := d.WorkingDir
	if dir == "" || dir == "~" {
		dir = home
	}
	if strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~/"))
	}
	cmd.Dir = dir
	cmd.Env = environment(d, home, r)
	_ = u
	return cmd, label, nil
}
func identity(runAs string) (*user.User, *syscall.Credential, string, string, error) {
	current, err := user.Current()
	if err != nil {
		return nil, nil, "", "", err
	}
	u := current
	groupName := ""
	if runAs != "" {
		name, g, _ := strings.Cut(runAs, ":")
		groupName = g
		if n, err := strconv.Atoi(name); err == nil {
			u, err = user.LookupId(strconv.Itoa(n))
			if err != nil {
				return nil, nil, "", "", err
			}
		} else {
			u, err = user.Lookup(name)
			if err != nil {
				return nil, nil, "", "", err
			}
		}
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	if groupName != "" {
		g, err := user.LookupGroup(groupName)
		if err != nil {
			g, err = user.LookupGroupId(groupName)
			if err != nil {
				return nil, nil, "", "", err
			}
		}
		gid, _ = strconv.ParseUint(g.Gid, 10, 32)
	}
	var cred *syscall.Credential
	if os.Geteuid() == 0 || uint32(uid) != uint32(os.Geteuid()) {
		cred = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	return u, cred, u.HomeDir, u.Username, nil
}
func environment(d model.Definition, home string, r model.Run) []string {
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "TZ=" + d.Timezone}
	if d.EnvBase == "inherit" {
		env = slices.DeleteFunc(os.Environ(), func(v string) bool { return strings.HasPrefix(v, "MINICRON_") })
	}
	for k, v := range readEnvFile(d.EnvFile) {
		env = append(env, k+"="+v)
	}
	for k, v := range d.Env {
		env = append(env, k+"="+v)
	}
	for k, ref := range d.SecretEnv {
		if value, ok := resolveSecret(ref); ok {
			env = append(env, k+"="+value)
		}
	}
	env = append(env, "MINICRON_JOB="+d.Name, "MINICRON_RUN_ID="+r.ID, "MINICRON_TRIGGER="+r.Trigger, "MINICRON_ATTEMPT=1")
	return env
}
func readEnvFile(path string) map[string]string {
	out := make(map[string]string)
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
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
	return out
}
func resolveSecret(ref string) (string, bool) {
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		v, found := os.LookupEnv(name)
		return v, found
	}
	if path, ok := strings.CutPrefix(ref, "file:"); ok {
		b, err := os.ReadFile(path)
		return strings.TrimSuffix(string(b), "\n"), err == nil
	}
	return "", false
}
func stopGroup(pgid int, d model.Definition, wait <-chan error) error {
	grace, _ := time.ParseDuration(d.Grace)
	if grace <= 0 {
		killGroup(pgid, syscall.SIGKILL)
		return <-wait
	}
	killGroup(pgid, parseSignal(d.StopSignal))
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-wait:
		return err
	case <-timer.C:
		killGroup(pgid, syscall.SIGKILL)
		return <-wait
	}
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
	case "KILL":
		return syscall.SIGKILL
	default:
		return syscall.SIGTERM
	}
}
func classify(err, cause error, success []int) (string, string, *int, string) {
	var exit *exec.ExitError
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
	if errors.As(err, &exit) {
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
func processIdentity(pid int) string {
	if runtime.GOOS != "linux" {
		return strconv.Itoa(pid)
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) > 21 {
		return fields[21]
	}
	return ""
}
func (s *Service) CleanupRecovered(runs []model.Run) {
	if runtime.GOOS != "linux" {
		return
	}
	for _, r := range runs {
		if r.PGID > 0 && r.PID > 0 && r.ProcessStartID != "" && processIdentity(r.PID) == r.ProcessStartID {
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
func (s *Service) Shutdown(ctx context.Context) {
	s.mu.Lock()
	runs := make([]*activeRun, 0, len(s.active))
	for _, a := range s.active {
		runs = append(runs, a)
		a.cancel(ErrShutdown)
	}
	s.mu.Unlock()
	for _, a := range runs {
		select {
		case <-a.done:
		case <-ctx.Done():
			return
		}
	}
}

var _ io.Reader
