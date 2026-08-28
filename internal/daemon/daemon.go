package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/minicron/minicron/internal/api"
	"github.com/minicron/minicron/internal/config"
	"github.com/minicron/minicron/internal/executor"
	"github.com/minicron/minicron/internal/logstore"
	"github.com/minicron/minicron/internal/scheduler"
	"github.com/minicron/minicron/internal/store"
	"github.com/minicron/minicron/internal/supervisor"
)

type Daemon struct {
	ConfigPath, DataDir, Version string
	Schema                       []byte
	mu                           sync.Mutex
	cfg                          *config.Config
	lock                         *os.File
	store                        *store.Store
	logs                         *logstore.Store
	exec                         *executor.Service
	sched                        *scheduler.Scheduler
	super                        *supervisor.Supervisor
	api                          *api.Server
}

func (d *Daemon) Run(ctx context.Context) error {
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
	d.logs = logs
	d.exec = executor.New(st, logs, cfg.Scheduler.MaxConcurrentRuns)
	recoverable, err := st.Recoverable(ctx)
	if err != nil {
		return err
	}
	d.exec.CleanupRecovered(recoverable)
	if err = st.Recover(ctx); err != nil {
		return err
	}
	d.sched = scheduler.New(st, d.exec)
	d.super = supervisor.New(st, d.exec)
	d.api = api.New(st, logs, d.exec, d.super, d.Reload, d.Version, d.Schema)
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
	d.sched.Reload(ctx, defs)
	d.super.Reload(defs)
	socket := ""
	if cfg.Server.UnixSocket == nil || *cfg.Server.UnixSocket {
		socket = filepath.Join(d.DataDir, "minicron.sock")
	}
	if err = d.api.Start(cfg.Server.Bind, socket); err != nil {
		return err
	}
	d.api.SetReady(true)
	go d.retentionLoop(ctx)
	slog.Info("minicron ready", "bind", cfg.Server.Bind, "socket", socket)
	<-ctx.Done()
	d.api.SetReady(false)
	d.sched.Stop()
	d.super.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d.exec.Shutdown(shutdownCtx)
	return d.api.Shutdown(shutdownCtx)
}
func (d *Daemon) Reload(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cfg == nil || d.store == nil {
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
	d.cfg = cfg
	d.sched.Reload(ctx, defs)
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
