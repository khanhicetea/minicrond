package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/minicron/minicron/internal/api"
	"github.com/minicron/minicron/internal/config"
	"github.com/minicron/minicron/internal/executor"
	"github.com/minicron/minicron/internal/logdb"
	"github.com/minicron/minicron/internal/logstore"
	"github.com/minicron/minicron/internal/scheduler"
	"github.com/minicron/minicron/internal/store"
	"github.com/minicron/minicron/internal/supervisor"
)

type Daemon struct {
	ConfigPath, DataDir, Version string
	mu                           sync.Mutex
	cfg                          *config.Config
	stopping                     bool
	lock                         *os.File
	store                        *store.Store
	ldb                          *logdb.LogDB
	logs                         *logstore.Store
	exec                         *executor.Service
	sched                        *scheduler.Scheduler
	super                        *supervisor.Supervisor
	api                          *api.Server
}

func (d *Daemon) Run(ctx context.Context) (runErr error) {
	if err := d.acquireLock(); err != nil {
		return err
	}
	defer d.releaseLock()
	cfg, err := config.Load(d.ConfigPath)
	if err != nil {
		return err
	}
	d.cfg = cfg
	st, err := store.Open(ctx, d.DataDir)
	if err != nil {
		return err
	}
	d.store = st
	defer st.Close()
	if err = st.SyncFiles(ctx, cfg.Definitions, cfg.Include.PruneMissing); err != nil {
		return err
	}
	logs, err := logstore.New(filepath.Join(d.DataDir, "logs"))
	if err != nil {
		return err
	}
	// Long-term logs live in their own SQLite file, separate from minicron.db:
	// the file buffer stays the crash-safe hot path, the archive is the
	// durable, prunable history.
	ldb, err := logdb.Open(d.DataDir)
	if err != nil {
		return err
	}
	defer ldb.Close()
	d.ldb = ldb
	logs.AttachDB(ldb)
	d.logs = logs
	maxLine, err := logstore.ParseBytes(cfg.Logs.MaxLine)
	if err != nil || maxLine <= 0 {
		// Validated at config load; keep a safe fallback for direct callers.
		maxLine = 256 << 10
	}
	d.exec = executor.New(st, logs, executor.Options{MaxConcurrentRuns: cfg.Scheduler.MaxConcurrentRuns, MaxLineBytes: maxLine})
	recoverable, err := st.Recoverable(ctx)
	if err != nil {
		return err
	}
	d.exec.CleanupRecovered(recoverable)
	if err = st.Recover(ctx); err != nil {
		return err
	}
	// Buffers orphaned by a crash (runs that never reached Close) are swept
	// into the archive so their pre-crash output is not lost.
	if err = logs.ArchiveOrphans(); err != nil {
		slog.Error("orphaned log sweep failed", "error", err)
	}
	d.sched = scheduler.New(st, d.exec)
	d.super = supervisor.New(st, d.exec)
	d.api = api.New(st, logs, d.exec, d.super, d.Reload, d.Version)
	token, err := d.api.InitializeToken(ctx)
	if err != nil {
		return err
	}
	if token != "" {
		slog.Warn("initial bearer token; save it now because it cannot be recovered", "token", token)
	}
	defs, err := st.Definitions(ctx)
	if err != nil {
		return err
	}
	// Use the same shutdown path for startup failures and normal cancellation.
	// Producers and HTTP handlers must stop before the databases close.
	defer func() {
		d.api.SetReady(false)
		d.mu.Lock()
		d.stopping = true
		d.sched.Stop()
		d.super.Shutdown()
		d.mu.Unlock()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d.exec.Shutdown(shutdownCtx)
		// HTTP cleanup gets its own budget even when a job used the run budget.
		apiCtx, cancelAPI := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelAPI()
		runErr = errors.Join(runErr, d.api.Shutdown(apiCtx))
	}()
	if err := d.sched.Reload(ctx, defs); err != nil {
		return err
	}
	d.super.Reload(defs)
	socket := ""
	if cfg.Server.UnixSocket == nil || *cfg.Server.UnixSocket {
		socket = filepath.Join(d.DataDir, "minicron.sock")
	}
	if err = d.api.Start(cfg.Server.Bind, socket); err != nil {
		return err
	}
	d.api.SetReady(true)
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	var maintenance sync.WaitGroup
	for _, loop := range []func(context.Context){d.retentionLoop, d.workerFlushLoop, d.logPruneLoop} {
		maintenance.Add(1)
		go func() {
			defer maintenance.Done()
			loop(maintenanceCtx)
		}()
	}
	defer func() {
		stopMaintenance()
		maintenance.Wait()
	}()
	slog.Info("minicron ready", "bind", cfg.Server.Bind, "socket", socket)
	<-ctx.Done()
	return nil
}
func (d *Daemon) Reload(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping || d.cfg == nil || d.store == nil {
		return errors.New("daemon is not ready")
	}
	cfg, err := config.Load(d.ConfigPath)
	if err != nil {
		return err
	}
	if cfg.Server.Bind != d.cfg.Server.Bind {
		return errors.New("server.bind requires daemon restart")
	}
	if err = d.store.SyncFiles(ctx, cfg.Definitions, cfg.Include.PruneMissing); err != nil {
		return err
	}
	defs, err := d.store.Definitions(ctx)
	if err != nil {
		return err
	}
	if err := d.sched.Reload(ctx, defs); err != nil {
		return err
	}
	d.cfg = cfg
	d.super.Reload(defs)
	return nil
}
func (d *Daemon) retentionLoop(ctx context.Context) {
	d.sweepRetention(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.sweepRetention(ctx)
		}
	}
}
func (d *Daemon) sweepRetention(ctx context.Context) {
	d.mu.Lock()
	storageCfg := d.cfg.Storage
	d.mu.Unlock()
	defs, err := d.store.Definitions(ctx)
	if err != nil {
		slog.Error("retention list failed", "error", err)
		return
	}
	for _, def := range defs {
		keep := def.KeepRuns
		if keep == 0 {
			keep = storageCfg.KeepRunsDefault
		}
		keepFor := def.KeepFor
		if keepFor == "" {
			keepFor = storageCfg.KeepForDefault
		}
		duration, err := time.ParseDuration(keepFor)
		if err != nil {
			continue
		}
		ids, err := d.store.RetentionCandidates(ctx, def.ID, keep, time.Now().Add(-duration))
		if err != nil {
			continue
		}
		for _, id := range ids {
			if err := d.logs.Delete(id); err == nil {
				_ = d.store.DeleteRun(ctx, id)
			}
		}
	}
}

// workerFlushLoop periodically seals the file buffers of still-running runs
// (workers) and copies the sealed chunks into the SQLite log archive, keeping
// the live buffer small without losing long-running output.
func (d *Daemon) workerFlushLoop(ctx context.Context) {
	interval := d.logFlushInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.logs.FlushActive()
			if next := d.logFlushInterval(); next != interval {
				interval = next
				ticker.Reset(next)
			}
		}
	}
}
func (d *Daemon) logFlushInterval() time.Duration {
	d.mu.Lock()
	value := d.cfg.Logs.WorkerFlushInterval
	d.mu.Unlock()
	if parsed, err := time.ParseDuration(value); err == nil && parsed >= time.Second {
		return parsed
	}
	return 15 * time.Minute
}

// logPruneLoop runs the daily log-archive prune sweep at the configured local
// time and deletes archived logs older than logs.db_keep_for. The wait is
// capped at one hour so config reloads take effect without a restart.
func (d *Daemon) logPruneLoop(ctx context.Context) {
	for {
		d.mu.Lock()
		clock := d.cfg.Logs.DBPruneAt
		tzName := d.cfg.Scheduler.Timezone
		d.mu.Unlock()
		next := nextDaily(time.Now(), clock, tzName)
		wait := min(time.Until(next), time.Hour)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if time.Now().Before(next) {
				continue // hourly re-check, prune time not reached yet
			}
			d.pruneLogs(ctx)
		}
	}
}
func (d *Daemon) pruneLogs(ctx context.Context) {
	d.mu.Lock()
	keepFor := d.cfg.Logs.DBKeepFor
	d.mu.Unlock()
	duration, err := time.ParseDuration(keepFor)
	if err != nil || duration <= 0 {
		slog.Error("log prune skipped: invalid logs.db_keep_for", "value", keepFor)
		return
	}
	n, err := d.ldb.Prune(ctx, time.Now().Add(-duration))
	if err != nil {
		slog.Error("log prune failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("pruned archived logs", "runs", n, "keep_for", keepFor)
	}
}

// nextDaily returns the next occurrence of the "HH:MM" clock time in the
// given timezone after now.
func nextDaily(now time.Time, clock, tzName string) time.Time {
	tz, err := time.LoadLocation(tzName)
	if err != nil {
		tz = time.UTC
	}
	hour, minute := 3, 30
	if hh, mm, ok := strings.Cut(clock, ":"); ok {
		if h, err := strconv.Atoi(hh); err == nil && h >= 0 && h <= 23 {
			hour = h
		}
		if m, err := strconv.Atoi(mm); err == nil && m >= 0 && m <= 59 {
			minute = m
		}
	}
	local := now.In(tz)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, tz)
	if !candidate.After(local) {
		candidate = time.Date(local.Year(), local.Month(), local.Day()+1, hour, minute, 0, 0, tz)
	}
	return candidate
}

func (d *Daemon) acquireLock() error {
	if err := os.MkdirAll(d.DataDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(d.DataDir, "minicron.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("data directory is locked by another daemon: %w", err)
	}
	f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Sync()
	d.lock = f
	return nil
}
func (d *Daemon) releaseLock() {
	if d.lock != nil {
		syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN)
		d.lock.Close()
	}
}
