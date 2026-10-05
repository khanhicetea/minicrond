package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
	"github.com/khanhicetea/minicrond/internal/store"
)

func runStatus(t *testing.T, st *store.Store, id string) model.Run {
	t.Helper()
	r, err := st.Run(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitTerminal(t *testing.T, st *store.Store, id string, timeout time.Duration) model.Run {
	t.Helper()
	var r model.Run
	eventually(t, timeout, "run "+id+" never became terminal", func() bool {
		r = runStatus(t, st, id)
		return model.Terminal(r.Status)
	})
	return r
}

func parallelJob(name, command string) model.Definition {
	return model.Definition{Name: name, Kind: model.KindJob, Command: command, Shell: "/bin/sh", OnOverlap: "parallel", SuccessCodes: []int{0}}
}

// With the gate full, a trigger is persisted as a queued run (and acknowledged
// with a real run id) instead of being skipped, then starts once a slot frees.
func TestOverCapacityTriggerIsQueuedAndDrains(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{DrainRate: 100}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 1"))
	job, jh := putJob(t, st, parallelJob("later", "echo done"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	queued, err := s.Trigger(t.Context(), job, jh, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != "queued" {
		t.Fatalf("over-capacity trigger = %s/%s, want queued", queued.Status, queued.EndReason)
	}
	if got := runStatus(t, st, queued.ID); got.Status != "queued" || got.StartedAt != nil {
		t.Fatalf("persisted queued run = %+v", got)
	}
	if d := s.Diagnostics(t.Context()); d.Queue.Depth != 1 || d.Queue.Bytes <= 0 {
		t.Fatalf("diagnostics = %+v", d.Queue)
	}
	done := waitTerminal(t, st, queued.ID, 10*time.Second)
	if done.Status != "succeeded" || done.StartedAt == nil || !done.StartedAt.After(done.QueuedAt) {
		t.Fatalf("drained run = %+v", done)
	}
	if d := s.Diagnostics(t.Context()); d.Queue.Depth != 0 {
		t.Fatalf("queue depth after drain = %d", d.Queue.Depth)
	}
}

// Queue count and per-job limits reject explicitly: manual callers get
// ErrQueueFull and no run; scheduled triggers leave a visible skipped record.
func TestQueueLimitsRejectExplicitly(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{MaxItems: 2, MaxPerJob: 1}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	b, bhash := putJob(t, st, parallelJob("b", "true"))
	c, ch := putJob(t, st, parallelJob("c", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		d model.Definition
		h string
	}{{a, ah}, {b, bhash}} {
		if r, err := s.Trigger(t.Context(), q.d, q.h, "manual", nil); err != nil || r.Status != "queued" {
			t.Fatalf("queue %s: %v, %v", q.d.Name, r.Status, err)
		}
	}
	// Per-job limit.
	if _, err := s.Trigger(t.Context(), a, ah, "manual", nil); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("per-job overflow = %v, want ErrQueueFull", err)
	}
	// Total limit.
	if _, err := s.Trigger(t.Context(), c, ch, "manual", nil); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("total overflow = %v, want ErrQueueFull", err)
	}
	// A scheduled trigger records the refusal instead of failing the scheduler.
	at := time.Now().UTC().Truncate(time.Second)
	rejected, err := s.Trigger(t.Context(), c, ch, "schedule", &at)
	if err != nil || rejected.Status != "skipped" || rejected.EndReason != "queue_full" {
		t.Fatalf("scheduled overflow = %s/%s, %v", rejected.Status, rejected.EndReason, err)
	}
	if got := runStatus(t, st, rejected.ID); got.Status != "skipped" {
		t.Fatalf("rejection not persisted: %+v", got)
	}
	if d := s.Diagnostics(t.Context()); d.Queue.Rejected != 3 || d.Queue.Depth != 2 {
		t.Fatalf("diagnostics = %+v", d.Queue)
	}
	runs, err := st.Runs(t.Context(), "c", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("manual rejection must not create a run row: %+v, %v", runs, err)
	}
}

func TestQueueByteLimitRejects(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{MaxBytes: 300}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Trigger(t.Context(), a, ah, "manual", nil); err != nil || r.Status != "queued" {
		t.Fatalf("first = %s, %v", r.Status, err)
	}
	if _, err := s.Trigger(t.Context(), a, ah, "manual", nil); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("byte overflow = %v, want ErrQueueFull", err)
	}
}

// If the queue cannot be persisted nothing is claimed: the trigger fails with
// ErrQueueUnavailable and leaves no run row (the enqueue is one transaction).
func TestQueuePersistenceUnavailableRejects(t *testing.T) {
	dir, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	if _, err := saboteur.Exec("ALTER TABLE exec_queue RENAME TO exec_queue_offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Trigger(t.Context(), a, ah, "manual", nil); !errors.Is(err, ErrQueueUnavailable) {
		t.Fatalf("manual trigger = %v, want ErrQueueUnavailable", err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if _, err := s.Trigger(t.Context(), a, ah, "schedule", &at); !errors.Is(err, ErrQueueUnavailable) {
		t.Fatalf("scheduled trigger = %v, want ErrQueueUnavailable", err)
	}
	if runs, err := st.Runs(t.Context(), "a", 10); err != nil || len(runs) != 0 {
		t.Fatalf("failed enqueue left run rows: %+v, %v", runs, err)
	}
	if d := s.Diagnostics(t.Context()); d.Queue.Unavailable != 2 || d.Queue.Depth != 0 {
		t.Fatalf("diagnostics = %+v", d.Queue)
	}
}

// An item that waits longer than max_age is ended as skipped/queue_expired by
// the drain loop's expiry timer even though capacity never frees up.
func TestQueuedItemExpires(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{MaxAge: 300 * time.Millisecond}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 20"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	queued, err := s.Trigger(t.Context(), a, ah, "manual", nil)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("queue = %s, %v", queued.Status, err)
	}
	got := waitTerminal(t, st, queued.ID, 5*time.Second)
	if got.Status != "skipped" || got.EndReason != "queue_expired" || got.StartedAt != nil {
		t.Fatalf("expired run = %+v", got)
	}
	if d := s.Diagnostics(t.Context()); d.Queue.Expired != 1 || d.Queue.Depth != 0 {
		t.Fatalf("diagnostics = %+v", d.Queue)
	}
}

// The drain loop revalidates against the current definition when it takes an
// item: disabled/deleted ones are dropped with a reason, an edited one runs in
// its current form (revision and hash are updated on the run).
func TestQueuedItemRevalidatesDefinition(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{DrainRate: 100}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 1"))
	disabled, dh := putJob(t, st, parallelJob("to-disable", "echo should-not-run"))
	deleted, delh := putJob(t, st, parallelJob("to-delete", "echo should-not-run"))
	edited, eh := putJob(t, st, parallelJob("to-edit", "echo old"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, q := range []struct {
		d model.Definition
		h string
	}{{disabled, dh}, {deleted, delh}, {edited, eh}} {
		r, err := s.Trigger(t.Context(), q.d, q.h, "manual", nil)
		if err != nil || r.Status != "queued" {
			t.Fatalf("queue %s: %s, %v", q.d.Name, r.Status, err)
		}
		ids[q.d.Name] = r.ID
	}
	ctx := t.Context()
	// Disabling through a definition update is only noticed when the drain takes
	// the item; DeleteDefinition drops its queued items immediately (store).
	off := disabled
	disabledFlag := false
	off.Enabled = &disabledFlag
	if _, err := st.PutDefinition(ctx, off, disabled.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDefinition(ctx, "to-delete", "test"); err != nil {
		t.Fatal(err)
	}
	newDef := edited
	newDef.Command = "echo new"
	saved, err := st.PutDefinition(ctx, newDef, edited.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, newHash, err := st.Definition(ctx, "to-edit")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitTerminal(t, st, ids["to-disable"], 10*time.Second); got.Status != "skipped" || got.EndReason != "definition_disabled" {
		t.Fatalf("disabled item = %s/%s", got.Status, got.EndReason)
	}
	if got := waitTerminal(t, st, ids["to-delete"], 10*time.Second); got.Status != "skipped" || got.EndReason != "definition_removed" {
		t.Fatalf("deleted item = %s/%s", got.Status, got.EndReason)
	}
	got := waitTerminal(t, st, ids["to-edit"], 10*time.Second)
	if got.Status != "succeeded" || got.Revision != saved.Revision || got.DefinitionHash != newHash {
		t.Fatalf("edited item = %+v (want revision %d hash %s)", got, saved.Revision, newHash)
	}
	if d := s.Diagnostics(ctx); d.Queue.Dropped != 1 {
		t.Fatalf("dropped = %d", d.Queue.Dropped)
	}
}

// Restart: queued items survive, running ones are interrupted and never
// re-queued, and the new service drains the survivors.
func TestQueueSurvivesRestartWithoutReplayingStartedRuns(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 30"))
	a, ah := putJob(t, st, parallelJob("a", "echo survivor"))
	started, err := s.Trigger(t.Context(), blocker, bh, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "blocker never started", func() bool { return runStatus(t, st, started.ID).Status == "running" })
	var queuedIDs []string
	for range 3 {
		r, err := s.Trigger(t.Context(), a, ah, "manual", nil)
		if err != nil || r.Status != "queued" {
			t.Fatalf("queue: %s, %v", r.Status, err)
		}
		queuedIDs = append(queuedIDs, r.ID)
	}
	// "Crash": stop the first service and recover like a daemon start.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	logs := s.logs
	s2 := New(st, logs, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{DrainRate: 100}})
	t.Cleanup(func() { s2.Shutdown(context.Background()) })
	if err := s2.ResumeQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := runStatus(t, st, started.ID); got.Status == "queued" || got.Status == "pending" || got.Status == "running" {
		t.Fatalf("started run was left replayable: %+v", got)
	}
	for _, id := range queuedIDs {
		if got := waitTerminal(t, st, id, 10*time.Second); got.Status != "succeeded" {
			t.Fatalf("survivor %s = %s/%s", id, got.Status, got.EndReason)
		}
	}
	runs, err := st.Runs(t.Context(), "blocker", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("blocker was replayed: %+v, %v", runs, err)
	}
}

// After a long outage the backlog does not start as a storm: items past their
// age expire, the rest start no faster than drain_rate.
func TestResumedBacklogIsRateBoundedAndExpiresStaleItems(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{DrainRate: 4, MaxAge: 5 * time.Second}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 0.5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 5 {
		r, err := s.Trigger(t.Context(), a, ah, "manual", nil)
		if err != nil || r.Status != "queued" {
			t.Fatalf("queue: %s, %v", r.Status, err)
		}
		ids = append(ids, r.ID)
	}
	var starts []time.Time
	for _, id := range ids {
		got := waitTerminal(t, st, id, 15*time.Second)
		if got.Status != "succeeded" || got.StartedAt == nil {
			t.Fatalf("run %s = %+v", id, got)
		}
		starts = append(starts, *got.StartedAt)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	// Four gaps of 250ms at 4 starts/second.
	if spread := starts[len(starts)-1].Sub(starts[0]); spread < 800*time.Millisecond {
		t.Fatalf("5 queued runs started within %v; drain_rate=4 allows no more than one per 250ms", spread)
	}
}

// Fairness: heads are served round-robin across definitions, FIFO within one.
func TestQueueServesDefinitionsRoundRobin(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{DrainRate: 200}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 0.5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	b, bhash := putJob(t, st, parallelJob("b", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, q := range []struct {
		d model.Definition
		h string
	}{{a, ah}, {a, ah}, {a, ah}, {b, bhash}} {
		r, err := s.Trigger(t.Context(), q.d, q.h, "manual", nil)
		if err != nil || r.Status != "queued" {
			t.Fatalf("queue: %s, %v", r.Status, err)
		}
		ids = append(ids, r.ID)
	}
	type started struct {
		job string
		at  time.Time
		id  string
	}
	var order []started
	for _, id := range ids {
		got := waitTerminal(t, st, id, 10*time.Second)
		order = append(order, started{got.Job, *got.StartedAt, id})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].at.Before(order[j].at) })
	var jobs []string
	for _, o := range order {
		jobs = append(jobs, o.job)
	}
	// The newest job's single item must not wait behind all of a's backlog.
	if jobs[0] != "a" || jobs[1] != "b" {
		t.Fatalf("start order = %v, want a then b first", jobs)
	}
	// FIFO within a definition.
	var aIDs []string
	for _, o := range order {
		if o.job == "a" {
			aIDs = append(aIDs, o.id)
		}
	}
	if !sort.StringsAreSorted(aIDs) {
		t.Fatalf("definition a ran out of order: %v", aIDs)
	}
}

// Overlap policy is unchanged: skip refuses while a run is active, and also
// while one is queued (a queued run is a pending run of that job); parallel
// jobs may queue several.
func TestQueueRespectsOverlapPolicy(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 5"))
	skipDef := parallelJob("skipper", "true")
	skipDef.OnOverlap = "skip"
	skipper, sh := putJob(t, st, skipDef)
	par, ph := putJob(t, st, parallelJob("par", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	first, err := s.Trigger(t.Context(), skipper, sh, "manual", nil)
	if err != nil || first.Status != "queued" {
		t.Fatalf("first skip-job trigger = %s, %v", first.Status, err)
	}
	second, err := s.Trigger(t.Context(), skipper, sh, "manual", nil)
	if err != nil || second.Status != "skipped" || second.EndReason != "overlap_skip" {
		t.Fatalf("second skip-job trigger = %s/%s, %v", second.Status, second.EndReason, err)
	}
	for range 2 {
		if r, err := s.Trigger(t.Context(), par, ph, "manual", nil); err != nil || r.Status != "queued" {
			t.Fatalf("parallel job = %s, %v", r.Status, err)
		}
	}
}

// Catch-up/schedule triggers queue too, and the unique (definition, occurrence)
// index still makes a repeated occurrence a replay, not a second queued run.
func TestScheduledOccurrenceQueuesOnce(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	first, err := s.Trigger(t.Context(), a, ah, "schedule", &at)
	if err != nil || first.Status != "queued" || first.ScheduledFor == nil {
		t.Fatalf("scheduled = %+v, %v", first, err)
	}
	again, err := s.Trigger(t.Context(), a, ah, "schedule", &at)
	if err != nil || again.ID != first.ID {
		t.Fatalf("repeated occurrence = %+v, %v; want %s", again, err, first.ID)
	}
	if d := s.Diagnostics(t.Context()); d.Queue.Depth != 1 {
		t.Fatalf("depth = %d", d.Queue.Depth)
	}
}

// Idempotent manual triggers: a replayed key returns the queued run.
func TestQueuedTriggerIsIdempotent(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	idem := IdempotencyRequest{Principal: "admin", Operation: "trigger", Key: "k1", RequestHash: "h"}
	first, replayed, err := s.TriggerIdempotent(t.Context(), a, ah, "manual", nil, idem)
	if err != nil || replayed || first.Status != "queued" {
		t.Fatalf("first = %+v replayed=%v %v", first, replayed, err)
	}
	again, replayed, err := s.TriggerIdempotent(t.Context(), a, ah, "manual", nil, idem)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay = %+v replayed=%v %v", again, replayed, err)
	}
}

// A retry that meets a full gate is queued (durably) and runs later; it is not
// dropped and does not hold a retry timer.
func TestRetryMeetingFullGateIsQueued(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{DrainRate: 100}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 2"))
	flakyDef := parallelJob("flaky-queue", "exit 1")
	flakyDef.Retries, flakyDef.RetryDelay = 1, 1
	flaky, fh := putJob(t, st, flakyDef)
	first, err := s.Trigger(t.Context(), flaky, fh, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if done := s.Wait(first.ID); done != nil {
		<-done
	}
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	var retryID string
	eventually(t, 10*time.Second, "retry was never queued", func() bool {
		runs, err := st.Runs(t.Context(), "flaky-queue", 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.Attempt == 2 {
				if r.Status == "skipped" {
					t.Fatalf("retry dropped as %s", r.EndReason)
				}
				retryID = r.ID
				return r.Status == "queued"
			}
		}
		return false
	})
	if got := waitTerminal(t, st, retryID, 15*time.Second); got.Status != "failed" || got.ParentRunID != first.ID || got.Trigger != "retry" {
		t.Fatalf("queued retry = %+v", got)
	}
}

// startup triggers are never queued: [[init]] waits for them.
func TestStartupTriggerIsNotQueued(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.Trigger(t.Context(), a, ah, "startup", nil)
	if err != nil || r.Status != "skipped" || r.EndReason != "queue_full" {
		t.Fatalf("startup over capacity = %s/%s, %v", r.Status, r.EndReason, err)
	}
}

func TestQueueDisabledKeepsSkipBehavior(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{Disabled: true}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 5"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.Trigger(t.Context(), a, ah, "manual", nil)
	if err != nil || r.Status != "skipped" || r.EndReason != "queue_full" {
		t.Fatalf("disabled queue = %s/%s, %v", r.Status, r.EndReason, err)
	}
	if d := s.Diagnostics(t.Context()); d.Queue.Enabled || d.Queue.Depth != 0 {
		t.Fatalf("diagnostics = %+v", d.Queue)
	}
}

// Shutdown with queued items leaves them durable and does not start them.
func TestShutdownLeavesQueuedItemsDurable(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 30"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	queued, err := s.Trigger(t.Context(), a, ah, "manual", nil)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("queue = %s, %v", queued.Status, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := runStatus(t, st, queued.ID); got.Status != "queued" {
		t.Fatalf("queued run after shutdown = %s/%s", got.Status, got.EndReason)
	}
	if stats, err := st.QueueStats(t.Context()); err != nil || stats.Depth != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

// Disabling the queue stops new items only; items persisted earlier still drain.
func TestDisabledQueueStillDrainsLeftoverItems(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 30"))
	a, ah := putJob(t, st, parallelJob("a", "echo leftover"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	queued, err := s.Trigger(t.Context(), a, ah, "manual", nil)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("queue = %s, %v", queued.Status, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	s2 := New(st, s.logs, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{Disabled: true, DrainRate: 100}})
	t.Cleanup(func() { s2.Shutdown(context.Background()) })
	if err := s2.ResumeQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := waitTerminal(t, st, queued.ID, 10*time.Second); got.Status != "succeeded" {
		t.Fatalf("leftover item = %s/%s", got.Status, got.EndReason)
	}
}

// S1: a retry that meets a full (or unavailable) queue is re-deferred, not
// recorded skipped, so its retry chain survives.
func TestRetryMeetingFullQueueIsRedeferred(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1, Queue: QueueOptions{MaxItems: 1, DrainRate: 100}})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 30"))
	filler, fh := putJob(t, st, parallelJob("filler", "true"))
	flakyDef := parallelJob("flaky-full", "exit 1")
	flakyDef.Retries, flakyDef.RetryDelay = 1, 1
	flaky, flh := putJob(t, st, flakyDef)
	first, err := s.Trigger(t.Context(), flaky, flh, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, first.ID)
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Trigger(t.Context(), filler, fh, "manual", nil); err != nil || r.Status != "queued" {
		t.Fatalf("filler = %s, %v", r.Status, err)
	}
	// Let several retry delays pass against the full queue.
	eventually(t, 10*time.Second, "retry was never attempted against the full queue", func() bool {
		return s.Diagnostics(t.Context()).Queue.Rejected >= 2
	})
	runs, err := st.Runs(t.Context(), "flaky-full", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Attempt == 2 {
			t.Fatalf("retry was recorded as %s/%s instead of being re-deferred", r.Status, r.EndReason)
		}
	}
	eventually(t, 5*time.Second, "deferred retry not pending", func() bool { return s.PendingRetries() == 1 })
}

func TestRetryMeetingUnavailableQueueIsRedeferred(t *testing.T) {
	dir, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 30"))
	flakyDef := parallelJob("flaky-unavail", "exit 1")
	flakyDef.Retries, flakyDef.RetryDelay = 1, 1
	flaky, flh := putJob(t, st, flakyDef)
	first, err := s.Trigger(t.Context(), flaky, flh, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, first.ID)
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	saboteur, err := sqlite.Open(filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saboteur.Close()
	if _, err := saboteur.Exec("ALTER TABLE exec_queue RENAME TO exec_queue_offline"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "retry never met the unavailable queue", func() bool {
		return s.Diagnostics(t.Context()).Queue.Unavailable >= 2
	})
	eventually(t, 5*time.Second, "deferred retry not pending", func() bool { return s.PendingRetries() == 1 })
}

// S2: Stop on a queued run cancels it in the queue with a visible end reason.
func TestStopCancelsQueuedRun(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 30"))
	a, ah := putJob(t, st, parallelJob("a", "echo must-not-run"))
	started, err := s.Trigger(t.Context(), blocker, bh, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := s.Trigger(t.Context(), a, ah, "manual", nil)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("queue = %s, %v", queued.Status, err)
	}
	if err := s.Stop(queued.ID); err != nil {
		t.Fatalf("stop queued run: %v", err)
	}
	got := runStatus(t, st, queued.ID)
	if got.Status != "stopped" || got.EndReason != "queue_cancelled" || got.StartedAt != nil || got.EndedAt == nil {
		t.Fatalf("cancelled run = %+v", got)
	}
	if d := s.Diagnostics(t.Context()); d.Queue.Depth != 0 {
		t.Fatalf("depth = %d", d.Queue.Depth)
	}
	if err := s.Stop(queued.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second stop = %v, want not found", err)
	}
	// An active run is still stopped normally.
	if err := s.Stop(started.ID); err != nil {
		t.Fatal(err)
	}
}

// NIT 1: an item the drain reaches after shutdown began stays queued (durable
// for the next start) instead of being dequeued and spent as failed/start_error.
func TestShutdownBeforeDequeueKeepsItemQueued(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 1})
	blocker, bh := putJob(t, st, parallelJob("blocker", "sleep 30"))
	a, ah := putJob(t, st, parallelJob("a", "true"))
	if _, err := s.Trigger(t.Context(), blocker, bh, "manual", nil); err != nil {
		t.Fatal(err)
	}
	queued, err := s.Trigger(t.Context(), a, ah, "manual", nil)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("queue = %s, %v", queued.Status, err)
	}
	heads, err := st.QueueHeads(t.Context(), 10)
	if err != nil || len(heads) != 1 {
		t.Fatalf("heads = %+v, %v", heads, err)
	}
	s.BeginShutdown()
	if _, _, _, err := s.startQueued(t.Context(), &heads[0]); !errors.Is(err, ErrShutdown) {
		t.Fatalf("startQueued during shutdown = %v, want ErrShutdown", err)
	}
	if got := runStatus(t, st, queued.ID); got.Status != "queued" {
		t.Fatalf("item = %s/%s, want still queued", got.Status, got.EndReason)
	}
	if stats, _ := st.QueueStats(t.Context()); stats.Depth != 1 {
		t.Fatalf("queue depth = %d", stats.Depth)
	}
}
