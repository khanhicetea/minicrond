package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
)

// X1 / ADR-23: deferred history sync.

func openHistoryStore(t *testing.T, dir string, opt ...sqlite.Options) (*Store, model.Definition) {
	t.Helper()
	s, err := Open(t.Context(), dir, opt...)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.PutDefinition(t.Context(), scheduledDefinition("job"), 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s, d
}

func historyRun(d model.Definition, id string) model.Run {
	return model.Run{ID: id, DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Status: "pending", Trigger: "manual", Attempt: 1, QueuedAt: time.Now()}
}

// writeHistory stores a run row and its terminal state.
func writeHistory(t *testing.T, s *Store, d model.Definition, id string) {
	t.Helper()
	if err := s.CreateRun(t.Context(), historyRun(d, id)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRunWithUsage(t.Context(), id, "succeeded", "exit", new(int), "", time.Now(), 0, false, nil); err != nil {
		t.Fatal(err)
	}
}

func syncedAt(t *testing.T, s *Store) string {
	t.Helper()
	var v string
	err := s.db.QueryRowContext(t.Context(), "SELECT value FROM meta WHERE key='history_synced_us'").Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

func waitStats(t *testing.T, s *Store, what string, cond func(HistorySyncStats) bool) HistorySyncStats {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := s.HistorySyncStats()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %+v", what, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The timer is armed only while relaxed commits are unsynced, fires once for
// a burst of writes, makes one FULL commit and is not re-armed while idle.
func TestHistorySyncTimerFiresOnceAndDisarms(t *testing.T) {
	t.Setenv(historySyncEnv, "")
	s, d := openHistoryStore(t, t.TempDir())
	defer s.Close()
	s.history.interval = 500 * time.Millisecond
	if st := s.HistorySyncStats(); !st.Deferred || st.Dirty || st.Armed || st.Syncs != 0 {
		t.Fatalf("idle store: %+v", st)
	}
	writeHistory(t, s, d, "a")
	writeHistory(t, s, d, "b")
	if st := s.HistorySyncStats(); !st.Dirty || !st.Armed || st.RelaxedWrites != 4 {
		t.Fatalf("after relaxed writes: %+v, want dirty, armed, 4 relaxed writes", st)
	}
	if syncedAt(t, s) != "" {
		t.Fatal("sync ran before the interval")
	}
	waitStats(t, s, "sync never ran", func(st HistorySyncStats) bool { return st.Syncs >= 1 })
	time.Sleep(3 * s.history.interval)
	st := s.HistorySyncStats()
	if st.Syncs != 1 || st.Dirty || st.Armed || st.Failures != 0 {
		t.Fatalf("after sync and idle: %+v, want one sync, clean, disarmed", st)
	}
	if syncedAt(t, s) == "" {
		t.Fatal("sync did not write its meta key")
	}
	// The sync commit ran on the writer at FULL, and the writer stays FULL.
	if got := synchronousOf(t, t.Context(), s.db); got != 2 {
		t.Fatalf("writer synchronous = %d after sync, want 2 (FULL)", got)
	}
	// A later write arms the next sync.
	writeHistory(t, s, d, "c")
	if st := s.HistorySyncStats(); !st.Armed {
		t.Fatalf("write after a sync did not arm the timer: %+v", st)
	}
	waitStats(t, s, "second sync never ran", func(st HistorySyncStats) bool { return st.Syncs == 2 && !st.Armed && !st.Dirty })
}

// A failed sync keeps the store dirty and retries on the next interval.
func TestHistorySyncRetriesAfterFailure(t *testing.T) {
	t.Setenv(historySyncEnv, "")
	s, d := openHistoryStore(t, t.TempDir())
	defer s.Close()
	s.history.interval = 50 * time.Millisecond
	if _, err := s.db.ExecContext(t.Context(), "ALTER TABLE meta RENAME TO meta_away"); err != nil {
		t.Fatal(err)
	}
	writeHistory(t, s, d, "a")
	waitStats(t, s, "sync failures not retried", func(st HistorySyncStats) bool { return st.Failures >= 2 })
	if st := s.HistorySyncStats(); !st.Dirty || st.Syncs != 0 {
		t.Fatalf("after failed syncs: %+v, want dirty and no sync", st)
	}
	if _, err := s.db.ExecContext(t.Context(), "ALTER TABLE meta_away RENAME TO meta"); err != nil {
		t.Fatal(err)
	}
	waitStats(t, s, "sync did not recover", func(st HistorySyncStats) bool { return st.Syncs == 1 && !st.Dirty && !st.Armed })
}

// Close performs a final sync when dirty and leaves no timer behind.
func TestCloseSyncsDeferredHistory(t *testing.T) {
	t.Setenv(historySyncEnv, "")
	dir := t.TempDir()
	s, d := openHistoryStore(t, dir)
	s.history.interval = time.Hour
	writeHistory(t, s, d, "a")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if st := s.HistorySyncStats(); st.Syncs != 1 || st.Dirty || st.Armed {
		t.Fatalf("after Close: %+v, want one sync, clean, disarmed", st)
	}
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if syncedAt(t, s) == "" {
		t.Fatal("Close did not write the sync record")
	}
	// A clean store does not sync on Close.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if st := s.HistorySyncStats(); st.Syncs != 0 {
		t.Fatalf("clean Close synced: %+v", st)
	}
}

// CreateRunSynced / FinishRunSynced (on_history_unavailable = "skip") keep
// per-write FULL commits and do not arm the timer.
func TestSyncedHistoryWritesAreFull(t *testing.T) {
	t.Setenv(historySyncEnv, "")
	s, d := openHistoryStore(t, t.TempDir())
	defer s.Close()
	if err := s.CreateRunSynced(t.Context(), historyRun(d, "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRunSynced(t.Context(), "a", "succeeded", "exit", new(int), "", time.Now(), 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if st := s.HistorySyncStats(); !st.Deferred || st.RelaxedWrites != 0 || st.Dirty || st.Armed {
		t.Fatalf("synced writes: %+v, want no relaxed write and no timer", st)
	}
	if r, err := s.Run(t.Context(), "a"); err != nil || r.Status != "succeeded" {
		t.Fatalf("run = %+v, %v", r, err)
	}
}

// With synchronous = "normal" configured, nothing changes: no deferral, no
// timer, the writer stays NORMAL.
func TestHistorySyncOffWithNormal(t *testing.T) {
	t.Setenv(historySyncEnv, "")
	s, d := openHistoryStore(t, t.TempDir(), sqlite.Options{Synchronous: "normal"})
	defer s.Close()
	writeHistory(t, s, d, "a")
	if err := s.StartRun(t.Context(), "a", 1, 1, "x", time.Now()); err == nil {
		t.Fatal("StartRun of a finished run succeeded")
	}
	if st := s.HistorySyncStats(); st.Deferred || st.RelaxedWrites != 0 || st.Dirty || st.Armed {
		t.Fatalf("normal store: %+v", st)
	}
	if got := synchronousOf(t, t.Context(), s.db); got != 1 {
		t.Fatalf("writer synchronous = %d, want 1 (NORMAL)", got)
	}
}

// MINICROND_HISTORY_SYNC=commit restores per-write FULL history commits; the
// ADR-18 relaxed writes stay relaxed but arm no timer.
func TestHistorySyncEnvRestoresPerWriteFull(t *testing.T) {
	t.Setenv(historySyncEnv, "commit")
	s, d := openHistoryStore(t, t.TempDir())
	defer s.Close()
	if err := s.CreateRun(t.Context(), historyRun(d, "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.StartRun(t.Context(), "a", 1, 1, "x", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRunWithUsage(t.Context(), "a", "succeeded", "exit", new(int), "", time.Now(), 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if st := s.HistorySyncStats(); st.Deferred || st.RelaxedWrites != 0 || st.Dirty || st.Armed {
		t.Fatalf("commit mode: %+v", st)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if st := s.HistorySyncStats(); st.Syncs != 0 {
		t.Fatalf("commit mode synced on Close: %+v", st)
	}
}

func TestHistorySyncOffWhenReadOnly(t *testing.T) {
	t.Setenv(historySyncEnv, "")
	if h := newHistorySync(true, true); h.deferred {
		t.Fatal("read-only store defers history")
	}
	if h := newHistorySync(true, false); !h.deferred {
		t.Fatal("FULL store does not defer history")
	}
}

const historyCrashEnv = "MINICROND_HISTORY_CRASH_DIR"

// Relaxed run rows and terminal states survive a daemon crash: the helper
// process commits them, never syncs, and dies by SIGKILL without Close.
func TestDeferredHistorySurvivesDaemonCrash(t *testing.T) {
	if dir := os.Getenv(historyCrashEnv); dir != "" {
		crashAfterDeferredHistory(dir)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeferredHistorySurvivesDaemonCrash$")
	cmd.Env = append(os.Environ(), historyCrashEnv+"="+dir, historySyncEnv+"=")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("helper did not die by SIGKILL: %v\n%s", err, out)
	}
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done, err := s.Run(t.Context(), "crash-done")
	if err != nil || done.Status != "succeeded" || done.EndedAt == nil {
		t.Fatalf("relaxed terminal state lost by a process crash: %+v, %v", done, err)
	}
	pending, err := s.Run(t.Context(), "crash-pending")
	if err != nil || pending.Status != "pending" {
		t.Fatalf("relaxed run row lost by a process crash: %+v, %v", pending, err)
	}
	if syncedAt(t, s) != "" {
		t.Fatal("helper synced; the test did not exercise unsynced commits")
	}
}

func crashAfterDeferredHistory(dir string) {
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		os.Exit(3)
	}
	s.history.interval = time.Hour
	d, err := s.PutDefinition(ctx, scheduledDefinition("crash-job"), 0, "test")
	if err != nil {
		os.Exit(4)
	}
	if err := s.CreateRun(ctx, historyRun(d, "crash-done")); err != nil {
		os.Exit(5)
	}
	if err := s.FinishRunWithUsage(ctx, "crash-done", "succeeded", "exit", new(int), "", time.Now(), 0, false, nil); err != nil {
		os.Exit(6)
	}
	if err := s.CreateRun(ctx, historyRun(d, "crash-pending")); err != nil {
		os.Exit(7)
	}
	if st := s.HistorySyncStats(); st.RelaxedWrites != 3 || !st.Dirty || st.Syncs != 0 {
		os.Exit(8)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}
