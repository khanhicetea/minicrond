package store

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func queueFixture(t *testing.T, names ...string) (*Store, string, map[string]model.Definition) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	defs := map[string]model.Definition{}
	for _, name := range names {
		if _, err := st.PutDefinition(t.Context(), model.Definition{Name: name, Kind: model.KindJob, Command: "true", Shell: "/bin/sh"}, 0, "test"); err != nil {
			t.Fatal(err)
		}
		d, _, err := st.Definition(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		defs[name] = d
	}
	return st, dir, defs
}

func queuedRun(d model.Definition, id string) model.Run {
	return model.Run{ID: id, DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: "h", Status: "pending", Trigger: "manual", Attempt: 1, QueuedAt: time.Now().UTC(), LogRef: "file:" + id}
}

var testLimits = QueueLimits{MaxItems: 5, MaxPerJob: 3, MaxBytes: 1 << 20, MaxAge: time.Minute}

func TestEnqueueRunEnforcesCountPerJobAndBytes(t *testing.T) {
	st, _, defs := queueFixture(t, "a", "b")
	ctx := t.Context()
	for i := range 3 {
		if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], "a"+strconv.Itoa(i)), nil, testLimits, false); err != nil {
			t.Fatal(err)
		}
	}
	// Per-job limit.
	if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], "a3"), nil, testLimits, false); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("per-job overflow = %v, want ErrQueueFull", err)
	}
	// Another job still fits, up to the total.
	for i := range 2 {
		if _, err := st.EnqueueRun(ctx, queuedRun(defs["b"], "b"+strconv.Itoa(i)), nil, testLimits, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.EnqueueRun(ctx, queuedRun(defs["b"], "b2"), nil, testLimits, false); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("count overflow = %v, want ErrQueueFull", err)
	}
	stats, err := st.QueueStats(ctx)
	if err != nil || stats.Depth != 5 || stats.Bytes <= 0 || stats.OldestUS == 0 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	// A rejected enqueue leaves no run row behind.
	if _, err := st.Run(ctx, "b2"); err == nil {
		t.Fatal("rejected enqueue created a run row")
	}
	// The byte budget counts accounted payload, not rows.
	st2, _, defs2 := queueFixture(t, "c")
	tight := QueueLimits{MaxItems: 100, MaxPerJob: 100, MaxBytes: 2 * QueuePayloadBytes(queuedRun(defs2["c"], "c0"), nil), MaxAge: time.Minute}
	for i := range 2 {
		if _, err := st2.EnqueueRun(ctx, queuedRun(defs2["c"], "c"+strconv.Itoa(i)), nil, tight, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st2.EnqueueRun(ctx, queuedRun(defs2["c"], "c2"), nil, tight, false); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("byte overflow = %v, want ErrQueueFull", err)
	}
}

func TestEnqueueRunUniqueRefusesSecondItem(t *testing.T) {
	st, _, defs := queueFixture(t, "a")
	if _, err := st.EnqueueRun(t.Context(), queuedRun(defs["a"], "a0"), nil, testLimits, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueRun(t.Context(), queuedRun(defs["a"], "a1"), nil, testLimits, true); !errors.Is(err, ErrQueueDuplicate) {
		t.Fatalf("second unique item = %v, want ErrQueueDuplicate", err)
	}
}

func TestEnqueueRunIdempotentReplayDoesNotConsumeQueue(t *testing.T) {
	st, _, defs := queueFixture(t, "a")
	idem := &Idempotency{Principal: "admin", Operation: "trigger", Key: "k", RequestHash: "x"}
	if replay, err := st.EnqueueRun(t.Context(), queuedRun(defs["a"], "a0"), idem, testLimits, false); err != nil || replay != "" {
		t.Fatalf("first = %q, %v", replay, err)
	}
	if replay, err := st.EnqueueRun(t.Context(), queuedRun(defs["a"], "a1"), idem, testLimits, false); err != nil || replay != "a0" {
		t.Fatalf("replay = %q, %v", replay, err)
	}
	if stats, _ := st.QueueStats(t.Context()); stats.Depth != 1 {
		t.Fatalf("depth = %d", stats.Depth)
	}
	conflict := &Idempotency{Principal: "admin", Operation: "trigger", Key: "k", RequestHash: "other"}
	if _, err := st.EnqueueRun(t.Context(), queuedRun(defs["a"], "a2"), conflict, testLimits, false); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting key = %v", err)
	}
	// A rejected enqueue must not reserve the key.
	full := QueueLimits{MaxItems: 1, MaxPerJob: 1, MaxBytes: 1 << 20, MaxAge: time.Minute}
	fresh := &Idempotency{Principal: "admin", Operation: "trigger", Key: "k2", RequestHash: "x"}
	if _, err := st.EnqueueRun(t.Context(), queuedRun(defs["a"], "a3"), fresh, full, false); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full = %v", err)
	}
	if _, err := st.IdempotentRun(t.Context(), "admin", "trigger", "k2", "x"); err == nil {
		t.Fatal("rejected enqueue reserved its idempotency key")
	}
}

func TestQueuedRunSurvivesReopenAndRecoveryKeepsPendingOut(t *testing.T) {
	st, dir, defs := queueFixture(t, "a")
	ctx := t.Context()
	if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], "queued-1"), nil, testLimits, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], "started-1"), nil, testLimits, false); err != nil {
		t.Fatal(err)
	}
	// started-1 is dequeued (pending): a crash after this point may have
	// started its process.
	if err := st.DequeueRun(ctx, "started-1", defs["a"].Revision, "h2", "boot"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	queued, err := st.Run(ctx, "queued-1")
	if err != nil || queued.Status != "queued" {
		t.Fatalf("queued run after restart = %+v, %v", queued, err)
	}
	started, err := st.Run(ctx, "started-1")
	if err != nil || started.Status != "interrupted" || started.DefinitionHash != "h2" {
		t.Fatalf("dequeued run after restart = %+v, %v", started, err)
	}
	heads, err := st.QueueHeads(ctx, 10)
	if err != nil || len(heads) != 1 || heads[0].RunID != "queued-1" {
		t.Fatalf("heads after restart = %+v, %v", heads, err)
	}
}

func TestDequeueAndDropAreAtomicAndSingleShot(t *testing.T) {
	st, _, defs := queueFixture(t, "a")
	ctx := t.Context()
	for _, id := range []string{"r1", "r2"} {
		if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], id), nil, testLimits, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DequeueRun(ctx, "r1", 7, "newhash", "boot"); err != nil {
		t.Fatal(err)
	}
	r1, _ := st.Run(ctx, "r1")
	if r1.Status != "pending" || r1.Revision != 7 || r1.DefinitionHash != "newhash" || r1.BootID != "boot" {
		t.Fatalf("dequeued run = %+v", r1)
	}
	if err := st.DequeueRun(ctx, "r1", 7, "newhash", "boot"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second dequeue = %v", err)
	}
	if err := st.DropQueued(ctx, "r1", "definition_removed", time.Now()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("drop of a dequeued run = %v", err)
	}
	if err := st.DropQueued(ctx, "r2", "definition_removed", time.Now()); err != nil {
		t.Fatal(err)
	}
	r2, _ := st.Run(ctx, "r2")
	if r2.Status != "skipped" || r2.EndReason != "definition_removed" || r2.EndedAt == nil {
		t.Fatalf("dropped run = %+v", r2)
	}
	if stats, _ := st.QueueStats(ctx); stats.Depth != 0 {
		t.Fatalf("depth = %d", stats.Depth)
	}
}

func TestExpireQueuedEndsOnlyExpiredItems(t *testing.T) {
	st, _, defs := queueFixture(t, "a")
	ctx := t.Context()
	short := QueueLimits{MaxItems: 10, MaxPerJob: 10, MaxBytes: 1 << 20, MaxAge: time.Millisecond}
	if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], "old"), nil, short, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], "fresh"), nil, testLimits, false); err != nil {
		t.Fatal(err)
	}
	next, ok, err := st.NextQueueExpiry(ctx)
	if err != nil || !ok || time.Until(next) > time.Second {
		t.Fatalf("next expiry = %v %v %v", next, ok, err)
	}
	ids, err := st.ExpireQueued(ctx, time.Now().Add(time.Second), 10)
	if err != nil || len(ids) != 1 || ids[0] != "old" {
		t.Fatalf("expired = %v, %v", ids, err)
	}
	old, _ := st.Run(ctx, "old")
	if old.Status != "skipped" || old.EndReason != "queue_expired" || !model.Terminal(old.Status) {
		t.Fatalf("expired run = %+v", old)
	}
	if fresh, _ := st.Run(ctx, "fresh"); fresh.Status != "queued" {
		t.Fatalf("fresh run = %+v", fresh)
	}
}

func TestQueueHeadsReturnsOldestPerDefinition(t *testing.T) {
	st, _, defs := queueFixture(t, "a", "b")
	ctx := t.Context()
	for _, id := range []struct{ def, run string }{{"a", "a1"}, {"a", "a2"}, {"b", "b1"}, {"a", "a3"}, {"b", "b2"}} {
		if _, err := st.EnqueueRun(ctx, queuedRun(defs[id.def], id.run), nil, testLimits, false); err != nil {
			t.Fatal(err)
		}
	}
	heads, err := st.QueueHeads(ctx, 10)
	if err != nil || len(heads) != 2 || heads[0].RunID != "a1" || heads[1].RunID != "b1" {
		t.Fatalf("heads = %+v, %v", heads, err)
	}
}

func TestQueuedRunsAreNotRetentionDeletable(t *testing.T) {
	st, _, defs := queueFixture(t, "a")
	ctx := t.Context()
	if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], "q"), nil, testLimits, false); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRuns(ctx, []string{"q"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRun(ctx, "q"); err != nil {
		t.Fatal(err)
	}
	if r, err := st.Run(ctx, "q"); err != nil || r.Status != "queued" {
		t.Fatalf("queued run was deleted: %+v, %v", r, err)
	}
	runs, err := st.RunsPage(ctx, "", 10, "", "active")
	if err != nil || len(runs) != 1 {
		t.Fatalf("active filter = %+v, %v", runs, err)
	}
	metrics, err := st.RunMetrics(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Minute), 4)
	if err != nil || metrics.Queued != 1 {
		t.Fatalf("metrics = %+v, %v", metrics, err)
	}
}
