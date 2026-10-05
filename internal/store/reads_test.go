package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// Metadata reads that serve the API must not queue behind the writer
// connection, or API traffic and state transitions delay each other.
func TestReadMethodsDoNotUseWriterConnection(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def, err := s.PutDefinition(t.Context(), model.Definition{Name: "example", Kind: model.KindJob, Command: "true", Shell: "/bin/sh"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := model.Run{ID: "r1", DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Status: "succeeded", Trigger: "manual", QueuedAt: now, EndedAt: &now}
	if err := s.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	// Occupy the single writer connection, as a long write or a slow listing
	// on that connection would.
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if got, err := s.ReadRun(ctx, "r1"); err != nil || got.ID != "r1" {
		t.Fatalf("ReadRun with the writer busy: %+v, %v", got, err)
	}
	if defs, err := s.ReadDefinitions(ctx); err != nil || len(defs) != 1 {
		t.Fatalf("ReadDefinitions with the writer busy: %v, %v", defs, err)
	}
	if got, hash, err := s.ReadDefinition(ctx, "example"); err != nil || got.Name != "example" || hash == "" {
		t.Fatalf("ReadDefinition with the writer busy: %+v, %q, %v", got, hash, err)
	}
	if defs, err := s.RetentionDefinitions(ctx); err != nil || len(defs) != 1 {
		t.Fatalf("RetentionDefinitions with the writer busy: %v, %v", defs, err)
	}
	if _, err := s.RetentionCandidates(ctx, def.ID, 1, time.Time{}, nil, 10); err != nil {
		t.Fatalf("RetentionCandidates with the writer busy: %v", err)
	}

	// Sensitivity check: the writer-connection variants really do queue.
	short, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	if _, err := s.Run(short, "r1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run on the busy writer = %v, want it to wait out its deadline", err)
	}
}

// A commit is visible to the very next pool read, so moving an API read off
// the writer connection does not show stale state after the write returns.
func TestReadMethodsSeeCommittedWrites(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 50 {
		name := fmt.Sprintf("job%d", i)
		def, err := s.PutDefinition(t.Context(), model.Definition{Name: name, Kind: model.KindJob, Command: "true", Shell: "/bin/sh"}, 0, "test")
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := s.ReadDefinition(t.Context(), name)
		if err != nil || got.Revision != def.Revision {
			t.Fatalf("%s not visible after commit: %+v, %v", name, got, err)
		}
		id := fmt.Sprintf("run%d", i)
		if err := s.CreateRun(t.Context(), model.Run{ID: id, DefinitionID: def.ID, Job: name, Kind: def.Kind, Status: "pending", Trigger: "manual", QueuedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		code := 0
		if err := s.FinishRun(t.Context(), id, "succeeded", "", &code, "", time.Now().UTC(), 0, false); err != nil {
			t.Fatal(err)
		}
		if run, err := s.ReadRun(t.Context(), id); err != nil || run.Status != "succeeded" {
			t.Fatalf("%s state after commit: %+v, %v", id, run, err)
		}
	}
	defs, err := s.ReadDefinitions(t.Context())
	if err != nil || len(defs) != 50 {
		t.Fatalf("ReadDefinitions = %d, %v", len(defs), err)
	}
}

func TestSortContextSortsAndStopsOnCancel(t *testing.T) {
	values := make([]int, 50000)
	for i := range values {
		values[i] = (i * 7919) % 50021
	}
	sorted := append([]int(nil), values...)
	guard := sortGuard{ctx: t.Context()}
	if err := guard.run(func() { slices.SortFunc(sorted, func(a, b int) int { guard.check(); return a - b }) }); err != nil || !sort.IntsAreSorted(sorted) {
		t.Fatalf("sortContext sorted=%v err=%v", sort.IntsAreSorted(sorted), err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	canceled := sortGuard{ctx: ctx}
	err := canceled.run(func() {
		slices.SortFunc(append([]int(nil), values...), func(a, b int) int { calls++; canceled.check(); return a - b })
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sort returned %v", err)
	}
	// Sorting 50,000 values takes hundreds of thousands of comparisons; a
	// canceled sort must give up within a couple of check intervals.
	if calls > 4*(metricsCheckMask+1) {
		t.Fatalf("canceled sort still made %d comparisons", calls)
	}
}

// cancelAfterCtx reports cancellation from Err once n calls have been made,
// standing in for a request that is canceled while the daemon is aggregating.
type cancelAfterCtx struct {
	context.Context
	n     int32
	calls atomic.Int32 // the driver may call Err from its own goroutine
}

func (c *cancelAfterCtx) Err() error {
	if c.calls.Add(1) > c.n {
		return context.Canceled
	}
	return nil
}

func TestRunMetricsStopsWhenCanceledDuringAggregation(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def, err := s.PutDefinition(t.Context(), model.Definition{Name: "metric", Kind: model.KindJob, Command: "true", Shell: "/bin/sh"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	const rows = 6000
	for i := range rows {
		us := now.Add(-time.Duration(i) * time.Second).UnixMicro()
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO runs(run_id,definition_id,job,kind,revision,definition_hash,status,trigger,queued_us,started_us,ended_us)
			VALUES(?,?,?,'job',1,'hash','succeeded','manual',?,?,?)`, fmt.Sprintf("run-%d", i), def.ID, def.Name, us, us+1000, us+int64(i%97+1)*1000); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	full, err := s.RunMetrics(t.Context(), now.Add(-24*time.Hour), now, 48)
	if err != nil || full.Total != rows {
		t.Fatalf("baseline metrics total=%d err=%v", full.Total, err)
	}
	// Cancel early in the row loop, then once only the sort is left (the row
	// loop checks about every 1024 rows, the sort about every 1024 compares).
	for _, n := range []int32{2, 30} {
		got, err := s.RunMetrics(&cancelAfterCtx{Context: t.Context(), n: n}, now.Add(-24*time.Hour), now, 48)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel after %d checks: err=%v total=%d", n, err, got.Total)
		}
	}
}
