package daemon

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func diskDaemon(t *testing.T, logsCfg config.Logs) (*Daemon, *store.Store, *logstore.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ldb, err := logdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ldb.Close() })
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	logs.AttachDB(ldb)
	d := &Daemon{DataDir: dir, cfg: &config.Config{Logs: logsCfg, Storage: config.Storage{KeepRunsDefault: 100, KeepForDefault: 30, AuditKeep: 100}}, store: st, ldb: ldb, logs: logs, diskHint: make(chan struct{}, 1)}
	return d, st, logs, dir
}

func archiveRun(t *testing.T, logs *logstore.Store, id string, size int) {
	t.Helper()
	w, err := logs.Open(id, "job", model.KindJob, logstore.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	_, _ = rand.Read(b)
	if err := w.Write(logstore.Stdout, b, 0); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(id); err != nil {
		t.Fatal(err)
	}
}

// 3A at the daemon level: configured limits become the store's policy, a pass
// deletes the oldest completed runs' logs, and metadata (definitions and run
// records) is never touched.
func TestDiskBudgetPassPrunesLogsButNeverMetadata(t *testing.T) {
	budget := 1 // MiB
	d, st, logs, _ := diskDaemon(t, config.Logs{DiskBudget: budget})
	def, err := st.PutDefinition(t.Context(), model.Definition{Name: "noisy", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	for i := range 6 {
		id := fmt.Sprintf("run-%d", i)
		at := base.Add(time.Duration(i) * time.Second)
		if err := st.CreateRun(t.Context(), model.Run{ID: id, DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: at, EndedAt: &at}); err != nil {
			t.Fatal(err)
		}
		archiveRun(t, logs, id, 300<<10) // 1.8 MiB of logs against a 1 MiB budget
	}
	d.applyDiskPolicy(d.cfg.Logs)
	d.enforceDiskBudget(t.Context())

	status := logs.DiskStatus()
	if !status.Enabled || status.PrunedRuns == 0 || status.Insufficient {
		t.Fatalf("status = %+v", status)
	}
	if frames, _ := logs.Read("run-0", 0, 10); len(frames) != 0 {
		t.Fatal("the oldest run's logs survived pressure")
	}
	if frames, _ := logs.Read("run-5", 0, 10); len(frames) != 1 {
		t.Fatal("the newest run's logs were deleted")
	}
	// Retention age is nowhere near: the runs are a minute old. Metadata stays.
	runs, err := st.Runs(t.Context(), def.Name, 100)
	if err != nil || len(runs) != 6 {
		t.Fatalf("run records = %d, %v; pressure must not delete metadata", len(runs), err)
	}
	if _, _, err := st.Definition(t.Context(), def.Name); err != nil {
		t.Fatalf("definition lost: %v", err)
	}
	if got := logs.MaintenanceStats()["disk_budget"]; got.Runs != 1 {
		t.Fatalf("pass duration not recorded: %+v", got)
	}
}

// The loop does nothing until asked: one pass at startup, one per hint, hints
// coalesce, and passes are spaced by the cooldown. No timer drives it.
func TestDiskBudgetLoopRunsOnHintsOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _, logs, _ := diskDaemon(t, config.Logs{DiskBudget: 1 << 20, DiskMinFree: new(int)})
		d.applyDiskPolicy(d.cfg.Logs)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			d.diskBudgetLoop(ctx)
		}()
		defer func() { cancel(); <-done }()
		passes := func() int64 { return logs.DiskStatus().Passes }
		synctest.Wait()
		if passes() != 1 {
			t.Fatalf("startup passes = %d, want 1", passes())
		}
		time.Sleep(48 * time.Hour) // idle: no timer-driven passes
		synctest.Wait()
		if passes() != 1 {
			t.Fatalf("an idle daemon ran %d passes", passes())
		}
		// A hint after idling runs a pass at once ...
		d.requestDiskCheck()
		synctest.Wait()
		if passes() != 2 {
			t.Fatalf("a hint ran %d extra passes, want 1", passes()-1)
		}
		// ... and a burst of hints inside the cooldown yields one pass after it.
		for range 50 {
			d.requestDiskCheck()
		}
		synctest.Wait()
		if passes() != 2 {
			t.Fatalf("hints during the cooldown ran at once: %d passes", passes())
		}
		time.Sleep(diskCheckCooldown)
		synctest.Wait()
		if passes() != 3 {
			t.Fatalf("coalesced hints ran %d passes, want 1", passes()-2)
		}
		time.Sleep(10 * diskCheckCooldown)
		synctest.Wait()
		if passes() != 3 {
			t.Fatalf("passes kept running without hints: %d", passes())
		}
	})
}

// The log store raises the hint from its capture paths once the daemon wires
// it, and a full hint slot never blocks a writer.
func TestCaptureRaisesDiskHintWithoutBlocking(t *testing.T) {
	d, _, logs, _ := diskDaemon(t, config.Logs{})
	logs.SetPressureHook(d.requestDiskCheck)
	w, err := logs.Open("run", "job", model.KindJob, logstore.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.diskHint:
	default:
		t.Fatal("opening a run raised no hint")
	}
	// Slot full, no consumer: neither seal nor further hints may block.
	d.requestDiskCheck()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Write(logstore.Stdout, []byte("x"), 0)
		_ = logs.Close("run")
		d.requestDiskCheck()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a writer blocked on the disk hint")
	}
}

func TestMaintenanceTasksRecordDurations(t *testing.T) {
	d, _, logs, _ := diskDaemon(t, config.Logs{DBKeepFor: 30})
	d.sweepRetention(t.Context())
	d.pruneLogs(t.Context())
	got := logs.MaintenanceStats()
	if got["retention"].Runs != 1 || got["log_prune"].Runs != 1 {
		t.Fatalf("maintenance stats = %+v", got)
	}
}

// The quarantine is kept by default and purged only when configured.
func TestQuarantinePolicyFromConfig(t *testing.T) {
	d, _, _, dir := diskDaemon(t, config.Logs{})
	entry := filepath.Join(dir, "logs", logstore.QuarantineDir, "old.corrupt")
	if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(entry, old, old); err != nil {
		t.Fatal(err)
	}
	d.purgeQuarantine(t.Context())
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("default policy purged quarantined data: %v", err)
	}
	d.cfg.Logs.QuarantineKeepFor = 30
	d.purgeQuarantine(t.Context())
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("configured quarantine_keep_for did not purge: %v", err)
	}
}

// A slow disk-budget pass (pruning, compaction and a directory walk on slow
// storage) must not stall the other maintenance loops: the worker flush exists
// to keep live buffers small exactly when disk is short. The loops only ask for
// a pass; diskBudgetLoop runs it.
func TestSlowDiskPassDoesNotDelayMaintenanceLoops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _, logs, dir := diskDaemon(t, config.Logs{WorkerFlushInterval: 1, DBKeepFor: 30, DBPruneAt: "00:30"})
		d.cfg.Scheduler.Timezone = "UTC"
		release := make(chan struct{})
		passes := 0
		d.diskPassHook = func(ctx context.Context) {
			passes++
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		defer func() { cancel(); wg.Wait() }()
		for _, loop := range []func(context.Context){d.diskBudgetLoop, d.workerFlushLoop, d.retentionLoop, d.logPruneLoop} {
			wg.Go(func() { loop(ctx) })
		}
		synctest.Wait()
		if passes != 1 {
			t.Fatalf("startup passes = %d", passes)
		}
		// A finished, unarchived buffer is picked up by the worker-flush tick.
		w, err := logs.Open("orphan", "job", model.KindJob, logstore.WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(logstore.Stdout, []byte("kept"), 0); err != nil {
			t.Fatal(err)
		}
		logs.AttachDB(nil)
		if err := logs.Close("orphan"); err != nil {
			t.Fatal(err)
		}
		logs.AttachDB(d.ldb)
		time.Sleep(time.Hour) // flush tick, hourly retention, 00:30 prune: all while the pass hangs
		synctest.Wait()
		if _, err := os.Stat(filepath.Join(dir, "logs", "orphan")); !os.IsNotExist(err) {
			t.Fatalf("worker flush was delayed by a slow disk pass: %v", err)
		}
		stats := logs.MaintenanceStats()
		if stats["retention"].Runs < 2 || stats["log_prune"].Runs < 1 || stats["worker_flush"].Runs < 1 {
			t.Fatalf("maintenance loops stalled behind the disk pass: %+v", stats)
		}
		if passes != 1 {
			t.Fatalf("loops ran %d passes inline", passes-1)
		}
		if len(d.diskHint) != 1 {
			t.Fatal("the loops did not request a coalesced disk check")
		}
		close(release)
	})
}
