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

	"github.com/khanhicetea/minicrond/internal/alerts"
	"github.com/khanhicetea/minicrond/internal/api"
	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/scheduler"
	"github.com/khanhicetea/minicrond/internal/store"
	"github.com/khanhicetea/minicrond/internal/supervisor"
)

type Daemon struct {
	ConfigPath, DataDir, Version string
	mu                           sync.Mutex
	cfg                          *config.Config
	running                      bool
	stopping                     bool
	lock                         *os.File
	store                        *store.Store
	ldb                          *logdb.LogDB
	logs                         *logstore.Store
	exec                         *executor.Service
	sched                        *scheduler.Scheduler
	super                        *supervisor.Supervisor
	api                          *api.Server
	alerts                       *alerts.Dispatcher
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
	d.mu.Lock()
	d.cfg = cfg
	d.mu.Unlock()
	st, err := store.Open(ctx, d.DataDir)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.store = st
	d.mu.Unlock()
	defer st.Close()
	if err := st.InterruptAlerts(ctx); err != nil {
		return fmt.Errorf("mark interrupted alerts: %w", err)
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
	d.mu.Lock()
	d.ldb = ldb
	d.logs = logs
	d.mu.Unlock()
	logs.AttachDB(ldb)
	maxLine := int64(cfg.Logs.MaxLine) << 10
	execService := executor.New(st, logs, executor.Options{
		MaxConcurrentRuns: cfg.Scheduler.MaxConcurrentRuns,
		MaxLineBytes:      maxLine,
		OnFinished: func(run model.Run, definition model.Definition) {
			d.mu.Lock()
			dispatcher := d.alerts
			d.mu.Unlock()
			if dispatcher != nil {
				dispatcher.Notify(run, definition)
			}
		},
	})
	d.mu.Lock()
	d.exec = execService
	d.mu.Unlock()
	recoverable, err := st.Recoverable(ctx)
	if err != nil {
		return err
	}
	execService.CleanupRecovered(recoverable)
	if err = st.Recover(ctx); err != nil {
		return err
	}
	// Buffers orphaned by a crash (runs that never reached Close) are swept
	// into the archive so their pre-crash output is not lost.
	if err = logs.ArchiveOrphans(); err != nil {
		slog.Error("orphaned log sweep failed", "error", err)
	}
	sched := scheduler.New(st, execService)
	super := supervisor.New(st, execService)
	apiServer := api.New(st, logs, execService, super, d.Reload, d.Reconcile, d.Version)
	apiServer.SetJobDefaults(func() model.Definition {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.cfg.Defaults
	})
	apiServer.SetAlertChannels(func() []config.AlertChannel {
		d.mu.Lock()
		defer d.mu.Unlock()
		return append([]config.AlertChannel(nil), d.cfg.AlertChannels...)
	})
	apiServer.SetAlertTest(func(ctx context.Context, name string) error {
		d.mu.Lock()
		dispatcher := d.alerts
		d.mu.Unlock()
		if dispatcher == nil {
			return errors.New("alert dispatcher is not ready")
		}
		return dispatcher.Test(ctx, name)
	})
	apiServer.SetAlertQueueDepth(func() int {
		d.mu.Lock()
		dispatcher := d.alerts
		d.mu.Unlock()
		if dispatcher == nil {
			return 0
		}
		return dispatcher.QueueDepth()
	})
	d.mu.Lock()
	d.sched, d.super, d.api = sched, super, apiServer
	d.mu.Unlock()
	token, err := apiServer.InitializeToken(ctx)
	if err != nil {
		return err
	}
	if token != "" {
		path := filepath.Join(d.DataDir, "initial-token")
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			return fmt.Errorf("write initial token: %w", err)
		}
		slog.Warn("initial bearer token written to restricted one-time file", "path", path)
	}
	defs, err := st.Definitions(ctx)
	if err != nil {
		return err
	}
	if err := validateAlertReferences(defs, cfg.AlertChannels); err != nil {
		return err
	}
	dispatcher, err := alerts.New(cfg.AlertChannels, st.RecordAlert)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.alerts = dispatcher
	d.mu.Unlock()
	// Use the same shutdown path for startup failures and normal cancellation.
	// Producers and HTTP handlers must stop before the databases close.
	defer func() {
		apiServer.SetReady(false)
		d.mu.Lock()
		d.stopping = true
		d.running = false
		d.mu.Unlock()
		sched.Stop()
		super.Shutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		runErr = errors.Join(runErr, execService.Shutdown(shutdownCtx))
		alertCtx, cancelAlerts := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelAlerts()
		runErr = errors.Join(runErr, dispatcher.Close(alertCtx))
		// HTTP cleanup gets its own budget even when a job used the run budget.
		apiCtx, cancelAPI := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelAPI()
		runErr = errors.Join(runErr, apiServer.Shutdown(apiCtx))
	}()
	if err := sched.Reload(ctx, defs); err != nil {
		return err
	}
	super.Reload(defs)
	socket := ""
	if cfg.Server.UnixSocket == nil || *cfg.Server.UnixSocket {
		socket = filepath.Join(d.DataDir, "minicron.sock")
	}
	if err = apiServer.Start(cfg.Server.Bind, socket); err != nil {
		return err
	}
	d.mu.Lock()
	d.running = true
	d.mu.Unlock()
	apiServer.SetReady(true)
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	var maintenance sync.WaitGroup
	for _, loop := range []func(context.Context){d.retentionLoop, d.workerFlushLoop, d.logPruneLoop} {
		maintenance.Go(func() {
			loop(maintenanceCtx)
		})
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
	if err := d.ready(); err != nil {
		return err
	}
	cfg, err := config.Load(d.ConfigPath)
	if err != nil {
		return err
	}
	if cfg.Server.Bind != d.cfg.Server.Bind || !boolPtrEqual(cfg.Server.UnixSocket, d.cfg.Server.UnixSocket) {
		return errors.New("server.bind and server.unix_socket require daemon restart")
	}
	if cfg.Scheduler.MaxConcurrentRuns != d.cfg.Scheduler.MaxConcurrentRuns || cfg.Logs.MaxLine != d.cfg.Logs.MaxLine {
		return errors.New("scheduler.max_concurrent_runs and logs.max_line require daemon restart")
	}
	defs, err := d.store.Definitions(ctx)
	if err != nil {
		return err
	}
	if err := validateAlertReferences(defs, cfg.AlertChannels); err != nil {
		return err
	}
	if err := d.alerts.Reload(cfg.AlertChannels); err != nil {
		return err
	}
	d.cfg = cfg
	return d.reconcileLocked(ctx)
}

func validateAlertReferences(defs []model.Definition, channels []config.AlertChannel) error {
	available := make(map[string]bool, len(channels))
	for _, channel := range channels {
		available[channel.Name] = true
	}
	for _, def := range defs {
		for _, name := range def.Alerts {
			if !available[name] {
				return fmt.Errorf("%s.alerts: unknown channel %q", def.Name, name)
			}
		}
	}
	return nil
}

// Reconcile refreshes the scheduler and worker supervisor from the authoritative
// definition registry without reloading daemon settings.
func (d *Daemon) Reconcile(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	return d.reconcileLocked(ctx)
}

func (d *Daemon) ready() error {
	if d.stopping || !d.running || d.cfg == nil || d.store == nil || d.alerts == nil || d.sched == nil || d.super == nil {
		return errors.New("daemon is not ready")
	}
	return nil
}

func (d *Daemon) reconcileLocked(ctx context.Context) error {
	defs, err := d.store.Definitions(ctx)
	if err != nil {
		return err
	}
	if err := d.sched.Reload(ctx, defs); err != nil {
		return err
	}
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
	defs, err := d.store.RetentionDefinitions(ctx)
	if err != nil {
		slog.Error("retention list failed", "error", err)
		return
	}
	if err := d.store.PruneMetadata(ctx, storageCfg.AuditKeep); err != nil {
		slog.Error("metadata retention failed", "error", err)
	}
	for _, def := range defs {
		keep := def.KeepRuns
		if keep == 0 {
			keep = storageCfg.KeepRunsDefault
		}
		keepFor := def.KeepFor
		if keepFor == 0 {
			keepFor = storageCfg.KeepForDefault
		}
		ids, err := d.store.RetentionCandidates(ctx, def.ID, keep, time.Now().Add(-time.Duration(keepFor)*24*time.Hour))
		if err != nil {
			slog.Error("run retention selection failed", "definition", def.Name, "error", err)
			continue
		}
		for _, id := range ids {
			if err := d.logs.Delete(id); err != nil {
				slog.Error("retained log deletion failed", "run", id, "error", err)
				continue
			}
			if err := d.store.DeleteRun(ctx, id); err != nil {
				slog.Error("retained run deletion failed", "run", id, "error", err)
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
			// Retry failed final archival without requiring a daemon restart.
			if err := d.logs.ArchiveOrphans(); err != nil {
				slog.Error("orphaned log retry failed", "error", err)
			}
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
	if value > 0 {
		return time.Duration(value) * time.Minute
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
	duration := time.Duration(keepFor) * 24 * time.Hour
	if duration <= 0 {
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

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (d *Daemon) acquireLock() error {
	if info, err := os.Lstat(d.DataDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("data directory must not be a symlink")
	}
	if err := os.MkdirAll(d.DataDir, 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(d.DataDir); err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("data directory must be a private directory")
	}
	path := filepath.Join(d.DataDir, "minicron.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
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
