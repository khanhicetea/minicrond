# ADR-9: Bounded durable execution queue and global background budgets

- Status: accepted; implemented with this ADR
- Implements: [ADR-8](0008-background-first-trade-offs.md) choice **4B** and audit finding **A15**
- Related: [audit](../../astra-audit-code.md), [agent guidance](../../AGENTS.md)

## Context

Before this change a job trigger that found `scheduler.max_concurrent_runs`
full was recorded `skipped/queue_full` and lost. Three other kinds of pending
work were bounded by nothing: one goroutine plus timer per failed run waiting to
retry, one goroutine per run whose terminal state could not be persisted, and
the archiver's in-memory queue of sealed buffers. Under a sustained imbalance
(e.g. 100 failures/s with a one-hour `retry_delay`) these grow without limit
while nobody is watching. Target profile: ~1 CPU / 512 MiB, 10 workers, four
concurrent jobs (ADR-8 5A); defaults below are conservative for it and are
configuration, not capacity guarantees.

## Decision

### 1. What is queued

Only **job** triggers that would otherwise be refused for lack of execution
capacity: `schedule` (including `catch_up = latest`), `manual` (API/CLI/UI), and
`retry`. Workers are never queued (supervised, not capacity-gated). `startup`
(`[[init]]`) triggers are never queued: they run sequentially and the daemon waits
for them. Overlap policy is unchanged and evaluated first: `on_overlap = skip`
with an active run still records `skipped/overlap_skip`; it additionally refuses
a second queued item for the same definition (`overlap_skip`), because a queued
run is a pending run of that job. `parallel` jobs may queue several items.

A queued item **is** a `runs` row with the new non-terminal status `queued`,
plus a small `exec_queue` row (`run_id`, `seq`, `definition_id`, `enqueued_us`,
`expires_us`, `payload_bytes`) written in the **same SQLite transaction**
(schema 9). The caller therefore gets a real run id: `GET /runs/{id}`, run
history, idempotency keys, the unique `(definition, scheduled_for)` schedule
occurrence index and retention all work unchanged. Queue wait is
`started_at - queued_at`.

### 2. Limits (all configurable under `[queue]`, restart required)

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | `false` restores the old behavior: over-capacity triggers are skipped `queue_full` |
| `max_items` | 100 | total queued items |
| `max_per_job` | 25 | queued items per definition, so one noisy job cannot occupy the queue |
| `max_bytes` | 256 (KiB) | persisted payload bytes (`payload_bytes` = identity strings of the item: job, hash, idempotency key, parent, plus a fixed row overhead) |
| `max_age` | 900 (s) | time an item may wait for capacity before it expires |
| `drain_rate` | 5 | maximum queued items started per second |
| `max_pending_retries` | 500 | global cap of retries waiting for their delay (section 7) |

All limits are checked inside the enqueue transaction (count, per-job count, byte
sum), so concurrent triggers cannot overshoot them.

### 3. Persist before acknowledging; rejection

`Trigger` returns success for a queued item only after the transaction commits.
- Queue full (any limit): API/manual callers get the typed error
  `executor.ErrQueueFull` (HTTP **429** `queue_full`, `Retry-After`), no run row.
  `schedule`/`retry` callers get a terminal `skipped/queue_full` run row (the
  existing visible record), one per occurrence: the scheduler cannot storm
  because it fires at schedule speed and every occurrence is a single row.
- Metadata persistence unavailable: `executor.ErrQueueUnavailable` (HTTP **503**
  `queue_unavailable`). The scheduler treats it like any trigger storage error:
  its pass fails and restarts with capped exponential backoff, and `catch_up`
  handles the occurrence. Nothing is claimed durable that is not.

### 4. Ordering and fairness

One drain goroutine, FIFO per definition (`seq`). Across definitions it takes the
head item of the definition served least recently (round robin; ties by `seq`),
so a burst of one job cannot starve the others. A new queueable trigger joins the
queue whenever the queue is non-empty even if a slot is momentarily free, so
newer work never overtakes older items.

### 5. Drain, rate and expiry

The drain goroutine exists once per daemon and is **armed only while the queue is
non-empty**: it wakes on enqueue, on a capacity slot being released, and on a timer
for the next expiry or the next rate-limit slot. It never polls. It starts at most
`drain_rate` items per second and only when a capacity slot is free, so a backlog
after downtime cannot create an execution storm.

An item whose `expires_us` has passed is not started: in one transaction its run
becomes terminal `skipped` with end reason `queue_expired` and its queue row is
deleted. Expiry, rejection and drop counts are exported (section 9). Expired items
are retained like any run (visible history).

### 6. Crash/restart and replay

The enqueue and the dequeue are each one transaction. Dequeue
(`status queued -> pending`, `DELETE exec_queue`) happens before the process is
spawned; on restart `Recover` marks every `pending`/`running` row `interrupted`
exactly as before. **Therefore an item whose run may have started is never
re-queued**; an item still `queued` was provably never started and survives the
restart. At startup the daemon only counts the queue and arms the drain; the same
rate, capacity and expiry rules apply, so items older than `max_age` after a long
outage expire instead of running late in a burst. There is no exactly-once
promise: a crash after the dequeue commit and before the spawn loses that run
(recorded `interrupted`), a crash after the spawn is the usual interrupted run.

### 7. Revalidation when an item is taken

Under the admission lock the current definition is loaded by name:
- deleted, recreated (different id), now a worker, or disabled: the item becomes
  terminal `skipped` (`definition_removed` / `definition_disabled`);
- `retry` items whose attempt exceeds the current `retries`: `skipped/retry_budget`;
- `on_overlap = skip` and the job is now active: `skipped/overlap_skip`;
- otherwise the item runs under the **current** definition (an edit made while it
  waited applies; the run row's `revision`/`definition_hash` are updated in the
  dequeue transaction to what actually runs).

### 8. A15: retries, finalizers and archive discovery

- **Retries**: one scheduler goroutine over a min-heap replaces the goroutine and
  timer per pending retry; the timer is armed for the earliest due item only. Items
  are small (identity, attempt, due time); the current definition is re-read when
  due. The heap is capped at `max_pending_retries`; beyond it the retry is **dropped
  with explicit evidence**: an error log, the `retries_dropped` counter, and a
  terminal `skipped/retry_dropped` run row for that attempt (written by the scheduler
  goroutine from a small newest-wins ring, never on the completion path). A due retry
  that finds no capacity is queued like any trigger (section 1); with the queue
  disabled it waits another `retry_delay`. Pending retries are in memory and are
  still lost on restart, exactly as before.
- **Finalizers**: one finalizer goroutine owns a map of runs whose terminal write
  failed (at most one entry per run, capped at 256), retrying with global backoff
  (1 s .. 30 s); one storage failure pauses the whole pass. At the cap, the
  completing run does **not** drop its state: it keeps its own goroutine and its
  capacity slot and retries inline, so overload turns into backpressure on admission
  (bounded by `max_concurrent_runs`). The database still shows `running` until the
  write lands; `Recover` marks it `interrupted` after a restart.
- **Archiver**: the in-memory queue of sealed run ids is capped at 4096. Beyond the
  cap the id is not queued; the archiver sets an overflow flag and, when its queue
  drains, runs an orphan sweep (which rediscovers every sealed buffer on disk). The
  periodic worker-flush sweep remains. Sealed-buffer count and bytes are computed on
  demand for diagnostics.

### 9. Visibility

`GET /api/v1/daemon` `diagnostics` gains `execution_queue` (enabled, depth, bytes,
oldest age, limits, and expired/rejected/unavailable/dropped counters since start),
`pending_retries` (`count`, `max`, `dropped`), `finalizers` (`background`, `inline`,
`max`) and `log_archive` (queued ids, overflow flag and total, sealed runs and bytes,
computed on request from a bounded directory walk). Run metrics count `queued` runs
as queued. Counters are process-lifetime; durable evidence is the terminal `skipped`
run rows above.

## Alternatives rejected

- *Separate payload-only queue table without a run row*: the caller would get no
  run id to poll, and idempotency/schedule uniqueness would need a second
  implementation.
- *New terminal status `expired`*: would touch retention indexes, the web status
  types and API consumers; `skipped` with a precise `end_reason` is already
  retained, indexed and displayed.
- *Durable retry timers*: would add a database write to every failed completion
  while storage may be what failed; retries keep their previous in-memory
  semantics, now bounded.
- *Poll the queue table on a ticker*: costs wakeups with an empty queue.

## Accepted downside

Late execution (up to `max_age`), one extra small table and one extra write per
queued trigger, and a hard refusal when the queue is full. Revisit `max_items` /
`drain_rate` after measuring the small-server profile, and if operators need
retries to survive restarts.
