package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
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

func TestDeleteAndDisableDropQueuedItems(t *testing.T) {
	st, _, defs := queueFixture(t, "gone", "off", "kept")
	ctx := t.Context()
	for _, q := range []struct{ def, run string }{{"gone", "g1"}, {"gone", "g2"}, {"off", "o1"}, {"kept", "k1"}} {
		if _, err := st.EnqueueRun(ctx, queuedRun(defs[q.def], q.run), nil, testLimits, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeleteDefinition(ctx, "gone", "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabled(ctx, "off", false); err != nil {
		t.Fatal(err)
	}
	for id, reason := range map[string]string{"g1": "definition_removed", "g2": "definition_removed", "o1": "definition_disabled"} {
		if r, _ := st.Run(ctx, id); r.Status != "skipped" || r.EndReason != reason {
			t.Fatalf("%s = %s/%s, want skipped/%s", id, r.Status, r.EndReason, reason)
		}
	}
	if r, _ := st.Run(ctx, "k1"); r.Status != "queued" {
		t.Fatalf("unrelated item = %s", r.Status)
	}
	if stats, _ := st.QueueStats(ctx); stats.Depth != 1 {
		t.Fatalf("depth = %d", stats.Depth)
	}
	// Enabling again must not touch anything.
	if err := st.SetEnabled(ctx, "off", true); err != nil {
		t.Fatal(err)
	}
}

// S3: the active-run index must cover 'queued', or RunMetrics and the active
// filter fall back to full table scans. Checked on fresh and migrated databases.
func TestActiveIndexCoversQueuedOnFreshAndMigratedDatabases(t *testing.T) {
	check := func(t *testing.T, st *Store) {
		t.Helper()
		queries := map[string]string{
			"metrics": `SELECT job,status,queued_us,started_us,ended_us FROM runs WHERE queued_us>=1 OR ended_us>=1 OR status IN ('queued','pending','running')`,
			"active":  `SELECT run_id FROM runs WHERE status IN ('queued','pending','running') ORDER BY queued_us DESC, run_id DESC LIMIT 10`,
		}
		for name, q := range queries {
			rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+q)
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			rows.Close()
			joined := strings.Join(plan, " | ")
			if !strings.Contains(joined, "idx_runs_active") {
				t.Fatalf("%s plan does not use idx_runs_active: %s", name, joined)
			}
		}
	}
	t.Run("fresh", func(t *testing.T) {
		st, _, _ := queueFixture(t)
		check(t, st)
	})
	t.Run("migrated", func(t *testing.T) {
		dir := t.TempDir()
		db, err := sql.Open("sqlite", filepath.Join(dir, "minicron.db"))
		if err != nil {
			t.Fatal(err)
		}
		old := strings.Replace(schema, resourceUsageDDL, "", 1)
		old = strings.Replace(old, alertChannelsDDL, "", 1)
		old = strings.Replace(old, "PRAGMA user_version=11;", "PRAGMA user_version=8;", 1)
		old = strings.Replace(old, execQueueDDL, "", 1)
		old = strings.Replace(old, "idx_runs_active ON runs(status) WHERE status IN ('queued','pending','running')", "idx_runs_active ON runs(status) WHERE status IN ('pending','running')", 1)
		if !strings.Contains(old, "WHERE status IN ('pending','running');") {
			t.Fatal("fixture did not recreate the old index")
		}
		if _, err := db.Exec(old); err != nil {
			t.Fatal(err)
		}
		db.Close()
		st, err := Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		check(t, st)
	})
}

// NIT 4: Recover repairs a queued run that has no queue row, and leaves
// properly queued ones alone.
func TestRecoverRepairsQueuedRunWithoutQueueRow(t *testing.T) {
	st, _, defs := queueFixture(t, "a")
	ctx := t.Context()
	if _, err := st.EnqueueRun(ctx, queuedRun(defs["a"], "ok"), nil, testLimits, false); err != nil {
		t.Fatal(err)
	}
	orphan := queuedRun(defs["a"], "orphan")
	orphan.Status = "queued"
	if err := st.CreateRun(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Run(ctx, "orphan"); r.Status != "interrupted" || r.EndReason != "crash_recovery" {
		t.Fatalf("orphan = %s/%s", r.Status, r.EndReason)
	}
	if r, _ := st.Run(ctx, "ok"); r.Status != "queued" {
		t.Fatalf("queued run = %s", r.Status)
	}
}
