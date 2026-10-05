package logstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// withinTime fails the test if f does not return in d, without hanging it.
func withinTime(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

func seedOrphans(t *testing.T, dir string, n int) {
	t.Helper()
	for i := range n {
		blob, err := encodeFrames([]Frame{{Sequence: 1, Stream: Stdout, Payload: []byte("x")}})
		if err != nil {
			t.Fatal(err)
		}
		writeOrphan(t, dir, fmt.Sprintf("orphan-%03d", i), "000001.zst", blob)
	}
}

// cancelAfterContext reports cancellation once its Err has been consulted n
// times, deterministically ending a sweep part-way.
type cancelAfterContext struct {
	context.Context
	left atomic.Int64
}

func (c *cancelAfterContext) Err() error {
	if c.left.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

// A04: a canceled sweep stops between runs, reports the cancellation, and the
// remaining buffers are archived by the next sweep.
func TestArchiveOrphansAbortsMidSweepOnCancel(t *testing.T) {
	s, db, dir := newArchiveStore(t)
	const orphans = 30
	seedOrphans(t, dir, orphans)
	ctx := &cancelAfterContext{Context: t.Context()}
	ctx.left.Store(12)
	err := s.ArchiveOrphansContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep error = %v, want cancellation", err)
	}
	runs, _, _, _ := db.Stats(t.Context())
	if runs == 0 || runs >= orphans {
		t.Fatalf("archived %d of %d runs; the sweep should stop part-way", runs, orphans)
	}
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	if runs, _, _, _ := db.Stats(t.Context()); runs != orphans {
		t.Fatalf("after the next sweep %d of %d runs archived", runs, orphans)
	}
}

func TestFlushActiveContextCanceledTouchesNothing(t *testing.T) {
	s, db, _ := newArchiveStore(t)
	for i := range 3 {
		id := fmt.Sprintf("worker-%d", i)
		w, err := s.Open(id, "w", model.KindWorker, WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(Stdout, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close(id) })
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.FlushActiveContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush error = %v", err)
	}
	if runs, _, _, _ := db.Stats(t.Context()); runs != 0 {
		t.Fatalf("canceled flush archived %d runs", runs)
	}
	if err := s.FlushActiveContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if runs, _, _, _ := db.Stats(t.Context()); runs != 3 {
		t.Fatalf("flush archived %d runs, want 3", runs)
	}
}

func TestFlushContextStopsWaitingForRunOwner(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run", "w", model.KindWorker, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close("run") })
	if !s.claim("run") {
		t.Fatal("claim failed")
	}
	defer s.release("run")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var err2 error
	withinTime(t, 5*time.Second, "FlushContext behind a busy owner", func() { err2 = w.FlushContext(ctx, s) })
	if !errors.Is(err2, context.DeadlineExceeded) {
		t.Fatalf("flush error = %v", err2)
	}
	if s.owners["run"] != true {
		t.Fatal("a canceled waiter must not steal or clear the owner's claim")
	}
}

func TestDeleteRunsContextCanceledDeletesNothing(t *testing.T) {
	s, db, _ := newArchiveStore(t)
	for _, id := range []string{"a", "b"} {
		w, err := s.Open(id, "job", model.KindJob, WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(Stdout, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	deleted, err := s.DeleteRunsContext(ctx, []string{"a", "b"})
	if !errors.Is(err, context.Canceled) || len(deleted) != 0 {
		t.Fatalf("deleted %v, error %v", deleted, err)
	}
	if runs, _, _, _ := db.Stats(t.Context()); runs != 2 {
		t.Fatalf("canceled deletion removed archive rows: %d left", runs)
	}
	if deleted, err := s.DeleteRuns([]string{"a", "b"}); err != nil || len(deleted) != 2 {
		t.Fatalf("deleted %v, error %v", deleted, err)
	}
}

// A04 item 5: StopArchiver used to set a flag and then wait unconditionally
// for workers stuck on a slot, lock or transaction. Now the deadline cancels
// the operation itself, so nothing touches the database after it returns.
func TestStopArchiverCancelsWorkerBlockedOnArchiveSlot(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	s.StartArchiver()
	for range cap(s.archiveSlots) {
		s.archiveSlots <- struct{}{}
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Seal("run"); err != nil {
		t.Fatal(err)
	}
	// The worker has taken the run off the queue and waits for a slot.
	deadline := time.Now().Add(5 * time.Second)
	for s.ArchiveBacklog() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker never picked up the sealed run")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var stopErr error
	start := time.Now()
	withinTime(t, 5*time.Second, "StopArchiver", func() { stopErr = s.StopArchiver(ctx) })
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("StopArchiver = %v", stopErr)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("StopArchiver took %v", time.Since(start))
	}
	// Workers are gone: no archive operation can run after this point.
	s.archiver.workers.Wait()
	// The buffer is intact for the next orphan sweep.
	for range cap(s.archiveSlots) {
		<-s.archiveSlots
	}
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	if frames, err := s.Read("run", 0, 10); err != nil || len(frames) != 1 {
		t.Fatalf("log after interrupted shutdown: %v, %v", frames, err)
	}
}

// Hold the archive database's write lock the way a stuck transaction would;
// the shutdown deadline must still end the in-flight archive transaction.
func TestStopArchiverCancelsArchiveTransactionBlockedOnDatabase(t *testing.T) {
	s, _, dir := newArchiveStore(t)
	control, err := sql.Open("sqlite", filepath.Join(dir, "minicron-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	conn, err := control.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	s.StartArchiver()
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Seal("run"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the worker enter its transaction
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var stopErr error
	start := time.Now()
	// SQLite's own busy timeout is 5 s; the context must beat it.
	withinTime(t, 10*time.Second, "StopArchiver with a blocked archive transaction", func() { stopErr = s.StopArchiver(ctx) })
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("StopArchiver = %v", stopErr)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("StopArchiver waited %v for the database", took)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "run")); err != nil {
		t.Fatalf("buffer must remain for the next sweep: %v", err)
	}
}

// Review S1: the final group sync ran before StopArchiver looked at its
// context, so a stalled disk could hold shutdown past its deadline.
func TestStopArchiverFinalSyncHonorsDeadline(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	s.SetGroupSync(time.Hour, 0)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	s.syncFile = func(f *os.File) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	defer close(release)
	s.StartArchiver()
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	var stopErr error
	start := time.Now()
	withinTime(t, 3*time.Second, "StopArchiver with a stalled fsync", func() { stopErr = s.StopArchiver(ctx) })
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("StopArchiver = %v, want the deadline error", stopErr)
	}
	select {
	case <-entered:
	default:
		t.Fatal("the stalled fsync was never reached")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("StopArchiver took %v", took)
	}
	s.archiver.workers.Wait() // archive workers are gone even though the sync is stuck
}
