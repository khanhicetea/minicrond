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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/khanhicetea/minicrond/internal/alerts"
	"github.com/khanhicetea/minicrond/internal/api"
	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/scheduler"
	"github.com/khanhicetea/minicrond/internal/sqlite"
	"github.com/khanhicetea/minicrond/internal/store"
	"github.com/khanhicetea/minicrond/internal/supervisor"
)

// shutdownBudget caps the shutdown path up to alert drain (reload wait,
// executor, supervisor, maintenance, alerts). Each stage also has its own
// smaller budget. HTTP cleanup and the archiver stop keep small reserved
// slices (5s and at least 2s) so they still run when the budget is spent.
// The budget is a target, not a hard guarantee: see runShutdown for what can
// still overrun. The one guarantee kept strictly is that the databases are
// never closed under a maintenance loop that failed to stop.
const shutdownBudget = 45 * time.Second

// maintenanceJoinBudget bounds the wait for maintenance loops after they have
// been canceled. A variable so tests can shorten it.
var maintenanceJoinBudget = 10 * time.Second

// extraMaintenanceLoop, when set (tests only), runs as one more maintenance loop.
var extraMaintenanceLoop func(context.Context)

// alertDrainFloor is the minimum time alert delivery gets at shutdown.
const alertDrainFloor = 5 * time.Second

// executorShutdownBudget is how long runs get to stop gracefully at shutdown
// before they are force-killed. A variable so tests can shorten it.
var executorShutdownBudget = 15 * time.Second

// lockContext acquires mu or gives up when ctx ends. sync.Mutex has no
// context-aware Lock, so it polls TryLock at a few-millisecond cadence; this
// is only used on rare reload/shutdown paths. It is not fair: it does not
// queue behind blocked Lock callers, so a steady stream of API reloads could
// starve one waiter until its context ends.
func lockContext(ctx context.Context, mu *sync.Mutex) bool {
	if mu.TryLock() {
		return true
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if mu.TryLock() {
				return true
			}
		}
	}
}

type Daemon struct {
	ConfigPath, DataDir, Version string
	// reloadMu serializes Reload and Reconcile; shutdown waits for it only
	// within a budget (lockContext). It is held while waiting for scheduler and
	// worker loops to exit, so nothing a run completion needs may require it.
	// mu only guards short field access.
	reloadMu sync.Mutex
	mu       sync.Mutex
	cfg      *config.Config
	running  bool
	stopping bool
	lock     *os.File
	store    *store.Store
	ldb      *logdb.LogDB
	logs     *logstore.Store
	exec     *executor.Service
	sched    *scheduler.Scheduler
	super    *supervisor.Supervisor
	api      *api.Server
	// alerts is read lock-free by run completion callbacks. Taking a daemon
	// lock there would deadlock with a reload waiting for a worker to exit.
	alerts atomic.Pointer[alerts.Dispatcher]
	// diskHint carries coalesced requests for a log disk-budget pass (diskbudget.go).
	diskHint chan struct{}
	// diskPassHook replaces the disk-budget pass; tests use it to make passes slow.
	diskPassHook func(context.Context)
}

func (d *Daemon) Run(ctx context.Context) error {
	return fault.Call(func() error { return d.run(ctx) })
}

func (d *Daemon) run(ctx context.Context) (runErr error) {
	if err := d.acquireLock(); err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, d.releaseLock()) }()
	cfg, err := config.Load(d.ConfigPath)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg = cfg
	d.mu.Unlock()
	// Set by the shutdown path when a maintenance loop outlives its join
	// budget; the database closers below then leave the databases open.
	var maintenanceStuck atomic.Bool
	dbOptions := sqlite.Options{Synchronous: cfg.Storage.Synchronous}
	st, err := store.Open(ctx, d.DataDir, dbOptions)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.store = st
	d.mu.Unlock()
	defer func() {
		if maintenanceStuck.Load() {
			slog.Error("not closing the metadata database: a maintenance loop is still running; SQLite recovers at the next start")
			return
		}
		runErr = errors.Join(runErr, st.Close())
	}()
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
	ldb, err := logdb.Open(d.DataDir, dbOptions)
	if err != nil {
		return err
	}
	defer func() {
		if maintenanceStuck.Load() {
			slog.Error("not closing the log database: a maintenance loop is still running; SQLite recovers at the next start")
			return
		}
		runErr = errors.Join(runErr, ldb.Close())
	}()
	d.mu.Lock()
	d.ldb = ldb
	d.logs = logs
	d.mu.Unlock()
	logs.AttachDB(ldb)
	logs.SetFrameSync(cfg.Logs.Durability == "frame")
	logs.SetGroupSync(time.Duration(cfg.Logs.SyncInterval)*time.Millisecond, int64(cfg.Logs.SyncMaxDirty)<<10)
	d.diskHint = make(chan struct{}, 1)
	d.applyDiskPolicy(cfg.Logs)
	logs.SetPressureHook(d.requestDiskCheck)
	// Finished runs are archived in the background. Stop the archiver after
	// the executor (deferred calls run in reverse) and before the archive
	// database closes; anything still queued is swept at the next start.
	logs.StartArchiver()
	// shutdownBy is the overall shutdown deadline, set when the shutdown
	// defer below starts. The archiver stop runs after it (deferred calls run
	// in reverse) and must fit in what remains, with a small floor.
	var shutdownBy time.Time
	defer func() {
		archiveBy := time.Now().Add(10 * time.Second)
		if !shutdownBy.IsZero() {
			archiveBy = minTime(archiveBy, maxTime(shutdownBy, time.Now().Add(2*time.Second)))
		}
		archiveCtx, cancel := context.WithDeadline(context.Background(), archiveBy)
		defer cancel()
		if err := logs.StopArchiver(archiveCtx); err != nil {
			slog.Warn("log archival interrupted at shutdown; the next start will finish it", "error", err)
		}
	}()
	maxLine := int64(cfg.Logs.MaxLine) << 10
	execService := executor.New(st, logs, executor.Options{
		MaxConcurrentRuns: cfg.Scheduler.MaxConcurrentRuns,
		MaxLineBytes:      maxLine,
		OnFinished: func(run model.Run, definition model.Definition) {
			if dispatcher := d.alerts.Load(); dispatcher != nil {
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
	if err := validateAlertReferences(cfg.Definitions(), cfg.AlertChannels); err != nil {
		return err
	}
	if err := st.SyncConfigDefinitions(ctx, cfg.Definitions()); err != nil {
		return fmt.Errorf("sync config definitions: %w", err)
	}
	// Buffers orphaned by a crash (runs that never reached Close) are swept
	// into the archive so their pre-crash output is not lost.
	if err = logs.ArchiveOrphansContext(ctx); err != nil {
		slog.Error("orphaned log sweep failed", "error", err)
	}
	sched := scheduler.New(st, execService)
	super := supervisor.New(st, execService)
	apiServer := api.New(st, logs, execService, super, d.Reload, d.Reconcile, d.Version)
	if err := apiServer.SetBasePath(os.Getenv("BASE_PATH")); err != nil {
		return err
	}
	apiServer.SetTCPEnabled(cfg.Server.TCPOn())
	apiServer.SetReadLimits(api.ReadLimits{
		Slots:       cfg.Reads.Slots,
		BudgetBytes: int64(cfg.Reads.Budget) << 20,
		WorkTimeout: time.Duration(cfg.Reads.WorkTimeout) * time.Second,
	})
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
		dispatcher := d.alerts.Load()
		if dispatcher == nil {
			return errors.New("alert dispatcher is not ready")
		}
		return dispatcher.Test(ctx, name)
	})
	apiServer.SetAlertQueueDepth(func() int {
		dispatcher := d.alerts.Load()
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
	dispatcher, err := alerts.New(cfg.AlertChannels, func(ctx context.Context, records []alerts.Record) error {
		updates := make([]store.AlertUpdate, len(records))
		for i, r := range records {
			updates[i] = store.AlertUpdate{RunID: r.RunID, Channel: r.Channel, Status: r.Status, Attempts: r.Attempts, LastError: r.Reason}
		}
		return st.RecordAlerts(ctx, updates)
	})
	if err != nil {
		return err
	}
	d.alerts.Store(dispatcher)
	// Maintenance loops are created here so the shutdown path can cancel them
	// first and join them last; they only start once the daemon is ready.
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	var maintenance sync.WaitGroup
	// Use the same shutdown path for startup failures and normal cancellation.
	// Producers and HTTP handlers must stop before the databases close.
	defer func() {
		overall, cancelOverall := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancelOverall()
		deadline, _ := overall.Deadline()
		shutdownBy = deadline
		runErr = errors.Join(runErr, d.runShutdown(overall, shutdownParts{
			stopMaintenance:  stopMaintenance,
			maintenance:      &maintenance,
			maintenanceStuck: &maintenanceStuck,
			api:              apiServer,
			sched:            sched,
			super:            super,
			exec:             execService,
			dispatcher:       dispatcher,
		}))
	}()
	for _, init := range cfg.Init {
		if !init.IsEnabled() {
			continue
		}
		def, hash, err := st.Definition(ctx, init.Name)
		if err != nil {
			return err
		}
		run, err := execService.Trigger(ctx, def, hash, "startup", nil)
		if err != nil {
			return fmt.Errorf("init %s: %w", init.Name, err)
		}
		if done := execService.Wait(run.ID); done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		finished, err := st.Run(ctx, run.ID)
		if err != nil {
			return err
		}
		if finished.Status != "succeeded" {
			return fmt.Errorf("init %s: run %s ended %s", init.Name, run.ID, finished.Status)
		}
	}
	if err := sched.Reload(ctx, defs); err != nil {
		return err
	}
	super.Reload(defs)
	socket := ""
	if cfg.Server.UnixOn() {
		socket = filepath.Join(d.DataDir, "minicron.sock")
	}
	if err = apiServer.Start(cfg.Server.Bind, socket); err != nil {
		return err
	}
	d.mu.Lock()
	d.running = true
	d.mu.Unlock()
	apiServer.SetReady(true)
	maintenanceErrors := make(chan error, 5)
	loops := map[string]func(context.Context){"retention": d.retentionLoop, "worker log flush": d.workerFlushLoop, "log prune": d.logPruneLoop, "log disk budget": d.diskBudgetLoop}
	if extraMaintenanceLoop != nil {
		loops["test"] = extraMaintenanceLoop
	}
	for name, loop := range loops {
		maintenance.Go(func() {
			if err := fault.Call(func() error {
				loop(maintenanceCtx)
				return nil
			}); err != nil {
				maintenanceErrors <- fmt.Errorf("%s loop: %w", name, err)
			}
		})
	}
	slog.Info("minicron ready", "tcp_enabled", cfg.Server.TCPOn(), "bind", cfg.Server.Bind, "socket", socket)
	select {
	case <-ctx.Done():
		return nil
	case err := <-apiServer.Errors():
		return fmt.Errorf("HTTP service failed: %w", err)
	case err := <-maintenanceErrors:
		return err
	}
}

// shutdownParts are the components runShutdown stops, in dependency order.
type shutdownParts struct {
	stopMaintenance  context.CancelFunc
	maintenance      *sync.WaitGroup
	maintenanceStuck *atomic.Bool
	api              *api.Server
	sched            *scheduler.Scheduler
	super            *supervisor.Supervisor
	exec             *executor.Service
	dispatcher       *alerts.Dispatcher
}

// runShutdown stops the daemon within ctx, in this order:
//
//  1. Signal everything that can run long: cancel maintenance (joined later),
//     refuse new API work, mark the daemon stopping, and signal worker
//     supervision so a reload waiting on an old worker's grace period unblocks.
//  2. Wait for an in-flight reload (bounded) so it cannot restart loops after
//     the scheduler stops, then refuse executor work and stop the scheduler
//     (bounded, 5s).
//  3. Executor shutdown (15s), supervisor join (5s), maintenance join (10s),
//     alert drain (20s), HTTP (5s), each capped by ctx.
//
// Maintenance and archive calls take the shutdown-canceled context
// (FlushActiveContext, ArchiveOrphansContext, DeleteRunsContext, retention and
// prune queries) and StopArchiver takes its own deadline. What can still
// overrun: executor runs that ignore a forced kill beyond forcedRunJoin, and
// uncancelable file/fsync work. A stage that times out is reported and the next
// stage still runs, except that a maintenance loop that fails to stop makes the
// daemon leave the databases open instead of closing them underneath it.
func (d *Daemon) runShutdown(ctx context.Context, p shutdownParts) error {
	var err error
	p.stopMaintenance()
	p.api.BeginShutdown()
	d.mu.Lock()
	d.stopping = true
	d.running = false
	d.mu.Unlock()
	p.super.BeginShutdown()
	lockCtx, cancelLock := context.WithTimeout(ctx, 5*time.Second)
	locked := lockContext(lockCtx, &d.reloadMu)
	cancelLock()
	if !locked {
		slog.Warn("reload still in progress at shutdown; stopping without waiting for it")
	}
	// Refuse new executor work first: a scheduler loop blocked in Trigger
	// (queued behind a slow admission) is released with ErrShutdown, so
	// Scheduler.Stop can join it. Stop itself is also bounded; its loops are
	// already canceled and only wait for calls that return on their own.
	p.exec.BeginShutdown()
	schedStopped := make(chan struct{})
	go func() {
		p.sched.Stop()
		close(schedStopped)
	}()
	schedCtx, cancelSched := context.WithTimeout(ctx, 5*time.Second)
	select {
	case <-schedStopped:
	case <-schedCtx.Done():
		slog.Warn("scheduler loops did not stop within the shutdown budget")
		err = errors.Join(err, fmt.Errorf("scheduler shutdown: %w", schedCtx.Err()))
	}
	cancelSched()
	if locked {
		d.reloadMu.Unlock()
	}
	execCtx, cancelExec := context.WithTimeout(ctx, executorShutdownBudget)
	err = errors.Join(err, p.exec.Shutdown(execCtx))
	cancelExec()
	superCtx, cancelSuper := context.WithTimeout(ctx, 5*time.Second)
	if superErr := p.super.ShutdownContext(superCtx); superErr != nil {
		err = errors.Join(err, fmt.Errorf("worker supervision shutdown: %w", superErr))
	}
	cancelSuper()
	joined := make(chan struct{})
	go func() {
		p.maintenance.Wait()
		close(joined)
	}()
	maintCtx, cancelMaint := context.WithTimeout(ctx, maintenanceJoinBudget)
	select {
	case <-joined:
	case <-maintCtx.Done():
		// Strict: the loops were canceled and the log store honors that, so
		// a loop that is still running is stuck in uncancelable work. Do not
		// close the databases under it.
		p.maintenanceStuck.Store(true)
		slog.Error("maintenance loops did not stop within the shutdown budget; databases will be left open")
		err = errors.Join(err, fmt.Errorf("maintenance shutdown: %w", maintCtx.Err()))
	}
	cancelMaint()
	// Failure alerts for runs killed at shutdown are the ones most worth
	// delivering: keep a floor even when earlier stages used the budget.
	alertBudget := alertDrainFloor
	if deadline, ok := ctx.Deadline(); ok {
		alertBudget = min(max(time.Until(deadline), alertDrainFloor), 20*time.Second)
	}
	alertCtx, cancelAlerts := context.WithTimeout(context.WithoutCancel(ctx), alertBudget)
	err = errors.Join(err, p.dispatcher.Close(alertCtx))
	cancelAlerts()
	// HTTP cleanup keeps its own slice even when a job used the run budget.
	apiCtx, cancelAPI := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	err = errors.Join(err, p.api.Shutdown(apiCtx))
	cancelAPI()
	return err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (d *Daemon) Reload(ctx context.Context) error {
	if !lockContext(ctx, &d.reloadMu) {
		return ctx.Err()
	}
	defer d.reloadMu.Unlock()
	current, err := d.readyConfig()
	if err != nil {
		return err
	}
	cfg, err := config.Load(d.ConfigPath)
	if err != nil {
		return err
	}
	if cfg.Server.Bind != current.Server.Bind || cfg.Server.UnixOn() != current.Server.UnixOn() || cfg.Server.TCPOn() != current.Server.TCPOn() {
		return errors.New("server.bind, server.tcp_enabled, and server.unix_socket require daemon restart")
	}
	if cfg.Scheduler.MaxConcurrentRuns != current.Scheduler.MaxConcurrentRuns || cfg.Logs.MaxLine != current.Logs.MaxLine {
		return errors.New("scheduler.max_concurrent_runs and logs.max_line require daemon restart")
	}
	if cfg.Storage.Synchronous != current.Storage.Synchronous {
		return errors.New("storage.synchronous requires daemon restart")
	}
	if cfg.Reads != current.Reads {
		return errors.New("the [reads] limits require daemon restart")
	}
	defs, err := d.store.Definitions(ctx)
	if err != nil {
		return err
	}
	active := cfg.Definitions()
	for _, def := range defs {
		if def.Source != "config" {
			active = append(active, def)
		}
	}
	if err := validateAlertReferences(active, cfg.AlertChannels); err != nil {
		return err
	}
	// Resolve channel credentials before touching the registry, then swap
	// in-memory state only after the definition sync has committed.
	channels, err := alerts.Prepare(cfg.AlertChannels)
	if err != nil {
		return err
	}
	if err := d.store.SyncConfigDefinitions(ctx, cfg.Definitions()); err != nil {
		return fmt.Errorf("sync config definitions: %w", err)
	}
	if err := d.alerts.Load().Apply(channels); err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg = cfg
	d.mu.Unlock()
	d.logs.SetFrameSync(cfg.Logs.Durability == "frame")
	d.logs.SetGroupSync(time.Duration(cfg.Logs.SyncInterval)*time.Millisecond, int64(cfg.Logs.SyncMaxDirty)<<10)
	d.applyDiskPolicy(cfg.Logs)
	d.requestDiskCheck()
	return d.reconcile(ctx)
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
	if !lockContext(ctx, &d.reloadMu) {
		return ctx.Err()
	}
	defer d.reloadMu.Unlock()
	if _, err := d.readyConfig(); err != nil {
		return err
	}
	return d.reconcile(ctx)
}

// readyConfig reports the current settings of a running daemon. Components
// wired during startup are immutable once the daemon is running.
func (d *Daemon) readyConfig() (*config.Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping || !d.running || d.cfg == nil || d.store == nil || d.alerts.Load() == nil || d.sched == nil || d.super == nil {
		return nil, errors.New("daemon is not ready")
	}
	return d.cfg, nil
}

// reconcile is called with reloadMu held and d.mu released: stopping loops
// waits for runs whose completion callbacks must not block on daemon locks.
func (d *Daemon) reconcile(ctx context.Context) error {
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
			d.requestDiskCheck() // coalesced; diskBudgetLoop does the work off this loop
		}
	}
}
func (d *Daemon) sweepRetention(ctx context.Context) {
	const pageSize = 128
	defer d.timed("retention")()
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
		if ctx.Err() != nil {
			return
		}
		keep := def.KeepRuns
		if keep == 0 {
			keep = storageCfg.KeepRunsDefault
		}
		keepFor := def.KeepFor
		if keepFor == 0 {
			keepFor = storageCfg.KeepForDefault
		}
		olderThan := time.Now().Add(-time.Duration(keepFor) * 24 * time.Hour)
		var cursor *store.RetentionCandidate
		for {
			if ctx.Err() != nil {
				return
			}
			page, err := d.store.RetentionCandidates(ctx, def.ID, keep, olderThan, cursor, pageSize)
			if err != nil {
				slog.Error("run retention selection failed", "definition", def.Name, "error", err)
				break
			}
			if len(page) == 0 {
				break
			}
			ids := make([]string, 0, len(page))
			for _, candidate := range page {
				if ctx.Err() != nil {
					return
				}
				ids = append(ids, candidate.ID)
			}
			deletable, err := d.logs.DeleteRunsContext(ctx, ids)
			if err != nil {
				slog.Error("retained log deletion failed", "count", len(ids), "error", err)
			}
			if err := d.store.DeleteRuns(ctx, deletable); err != nil {
				slog.Error("retained run batch deletion failed", "count", len(deletable), "error", err)
				// Preserve per-run progress and diagnostics if one row prevents
				// the atomic batch from completing.
				for _, id := range deletable {
					if ctx.Err() != nil {
						return
					}
					if err := d.store.DeleteRun(ctx, id); err != nil {
						slog.Error("retained run deletion failed", "run", id, "error", err)
					}
				}
			}
			cursor = &page[len(page)-1]
			if len(page) < pageSize {
				break
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
			flushed := d.timed("worker_flush")
			if err := d.logs.FlushActiveContext(ctx); err != nil {
				return
			}
			// Retry failed final archival without requiring a daemon restart.
			if err := d.logs.ArchiveOrphansContext(ctx); err != nil && ctx.Err() == nil {
				slog.Error("orphaned log retry failed", "error", err)
			}
			flushed()
			d.requestDiskCheck() // coalesced; diskBudgetLoop does the work off this loop
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
			d.purgeQuarantine(ctx)
			d.requestDiskCheck() // coalesced; diskBudgetLoop does the work off this loop
		}
	}
}
func (d *Daemon) pruneLogs(ctx context.Context) {
	defer d.timed("log_prune")()
	d.mu.Lock()
	keepFor := d.cfg.Logs.DBKeepFor
	maxSize := d.cfg.Logs.MaxSizeBytes()
	d.mu.Unlock()
	defer func() {
		if err := d.ldb.Compact(ctx); err != nil && ctx.Err() == nil {
			slog.Error("log archive compaction failed", "error", err)
		}
	}()
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
	if maxSize <= 0 {
		return
	}
	n, err = d.ldb.PruneToSize(ctx, maxSize)
	if err != nil {
		slog.Error("log size prune failed", "error", err)
		return
	}
	if n > 0 {
		slog.Warn("pruned oldest archived logs to fit logs.db_max_size", "runs", n, "max_size_mib", maxSize>>20)
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
		return errors.Join(fmt.Errorf("data directory is locked by another daemon: %w", err), f.Close())
	}
	if err := writeLockPID(f); err != nil {
		return errors.Join(err, f.Close()) // Closing releases the advisory lock.
	}
	d.lock = f
	return nil
}
func writeLockPID(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate daemon lock: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		return fmt.Errorf("write daemon lock pid: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync daemon lock: %w", err)
	}
	return nil
}

func (d *Daemon) releaseLock() error {
	if d.lock == nil {
		return nil
	}
	f := d.lock
	d.lock = nil
	var unlockErr error
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		unlockErr = fmt.Errorf("unlock daemon data directory: %w", err)
	}
	return errors.Join(unlockErr, f.Close())
}
