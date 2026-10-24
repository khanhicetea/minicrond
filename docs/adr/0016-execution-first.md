# ADR-16: Execution first — run history is written behind execution

- Status: accepted (owner decision, 2026-10-22); implemented on `claude/focused-knuth-pc6y1l`
- Decision source: the owner: "minicrond should keep running schedules and
  workers durably even when the disk or logs are not ready; run histories and
  run logs are important but not the highest priority. It should run as a
  stable, durable process manager in a Docker container."
- Supersedes: the "required execution-state persistence remains enforced"
  clause of [ADR-8](0008-background-first-trade-offs.md) 2A, the finalizer
  inline hold of [ADR-9](0009-durable-execution-queue.md) §8, and A02's
  "supervision waits until the run is terminal in the database".
- Related: [ADR-14](0014-worker-exhaustion.md), [operations](../operations.md#pending-work-budgets-and-overload)

## Context

Before this change every execution step depended on a successful metadata
(`minicron.db`) write:

| Step | Old behavior when the write failed |
|---|---|
| insert the run row | the run never started; the scheduler pass failed and retried with 1 s → 60 s backoff |
| persist the next fire (`schedule_state`) before waiting | the scheduler never reached the fire time: **no scheduled job ran** while writes failed |
| `running` transition (10 s budget) | the **child was SIGKILLed** and the run marked `start_error` |
| open run logs | the run failed with `start_error` (workers burned restart budget) |
| terminal transition | supervisors **did not restart workers** until the row became terminal; jobs held their capacity slot up to 2 min each past 256 pending finalizers, throttling all execution; alerts and retries waited for the write |
| startup recovery, alert recovery, config sync, token, queue resume | **the daemon exited** (in a container: crash loop, nothing runs) |
| a maintenance loop panic | the daemon shut down (all workers stopped) |

A full disk, a read-only volume, an external process holding the SQLite write
lock, or a stalled device therefore stopped the very work the daemon exists to
do. The owner ranks execution above history.

## Decision

Execution never waits for, or is stopped by, metadata or log storage.
History is written synchronously while storage is healthy and **behind
execution** while it is not.

1. **Run rows.** Launch paths (`schedule`, `startup`, `retry`, `manual` without
   an idempotency key, skipped/missed records) insert the row with a 2 s
   deadline. An *unavailable* error (anything except a constraint violation, a
   refused transition, an idempotency conflict or the caller's own
   cancellation) launches the run anyway and hands the row to the deferred
   history writer. A constraint violation still refuses the launch, so a
   scheduled occurrence that already has a row is never run twice. Idempotent
   API triggers and queue enqueue/dequeue still require the database (their
   contracts are about durable admission; [ADR-9] rejection rules are
   unchanged).
2. **Circuit breaker.** After an unavailable error the executor marks storage
   *degraded*. While degraded and history is pending, launches do not touch the
   database at all, so a dead or locked disk adds no launch latency beyond the
   first attempt (bounded by SQLite's 5 s busy timeout, which does not observe
   contexts, or 2 s for a stuck connection). The first successful write clears it.
3. **Running/terminal transitions.** A failed `running` write no longer kills
   the child. A failed terminal write no longer delays anything: the outcome is
   kept in memory, the completion callback (alerts) and the retry policy run at
   completion, and the write joins the deferred history. Only a *refused*
   `running` transition (the row is no longer `pending`, e.g. it was deleted)
   still stops the process.
4. **Deferred history writer.** The existing single finalizer goroutine now
   writes up to three ordered stages per run: the pending row, the running
   transition, the terminal state. Per-item 1 s → 30 s backoff, three
   consecutive failures pause a pass, a poison item cannot starve the rest.
   Bounded by `MaxFinalizers` (1024 items, about 2–4 KiB each). **When full, the
   next run's history is not kept**: the run executes normally, the loss is
   logged and counted (`run_history.dropped`). No capacity slot is held for
   history any more (the inline hold and its 4× hard cap are removed).
5. **Supervision.** The executor keeps the in-memory outcome of each startup run
   (workers, `[[init]]`) until its waiter takes it (`TakeOutcome`, bounded at
   1024). Restart policy, healthy-lifetime accounting and `[[init]]` success use
   it; the stored row is only a fallback. A reload whose exhaustion clear fails
   still starts workers.
6. **Scheduler.** Each job loop keeps its schedule position (anchor, last fire)
   in memory across passes. Only the first pass reads `schedule_state`; if that
   read fails the job schedules from now (no catch-up for the unknown gap).
   Schedule-state writes have a 2 s deadline and failures are logged once per
   episode; firing continues.
7. **Logs.** A run whose log storage cannot be opened starts with a degraded
   writer (ADR-8 2A): output is discarded and counted, the directory is created
   and capture resumes lazily on a later line.
8. **Startup and maintenance.** History-only startup writes (interrupt alerts,
   recover runs, config-definition sync, token initialization, queue resume)
   are logged instead of aborting startup; a failed config sync runs the
   definitions already in the registry. Unreadable definitions remain fatal
   (the daemon cannot know what to run). A panicking maintenance loop is
   restarted with 1 s → 5 min backoff instead of shutting the daemon down.
9. **Containers (PID 1).** When minicrond is PID 1 in its PID namespace and
   runs the daemon, it re-executes itself as its only child and acts as a
   minimal init: it forwards TERM/INT/HUP/QUIT/USR1/USR2, reaps every orphan,
   and exits with the daemon's status. Reaping inside the daemon was rejected:
   `wait4(-1)` races `os/exec`'s own wait of each run. `MINICROND_INIT=0`
   disables it (for `docker run --init`/tini).

## Alternatives rejected

- **A local append-only journal file** for unwritten history: it would fail on
  the same full or read-only disk, and adds a second storage format and replay
  logic. Rejected in favor of a bounded in-memory spool.
- **Unbounded spool**: violates bounded-resource rules; a multi-day outage with a
  chatty schedule would grow memory without limit.
- **Keep blocking execution but alert loudly**: the owner explicitly ranks
  execution first.
- **Reaping in-process with `PR_SET_CHILD_SUBREAPER` or `wait4(-1)`**: races
  `os/exec`. Documenting `docker run --init` alone leaves the default image
  broken.

## Accepted costs

- History written late can be lost: at daemon shutdown or crash, spooled
  history that never reached the database is gone (a stored nonterminal row is
  still marked `interrupted` at the next start); beyond 1024 pending items new
  history is dropped. A run whose terminal state was dropped keeps a stored
  `running` row until the next start.
- While degraded, scheduled-occurrence de-duplication across a daemon restart
  depends on rows that may not have been written: after a crash during an
  outage, `catch_up = "latest"` may run an occurrence again. There was never an
  exactly-once guarantee.
- Alerts are sent before the run is stored when storage is degraded. Their
  delivery records (`alert_deliveries`, FK to `runs`) are parked in memory
  (256 observations, oldest kept) and written right after the history writer
  stores the run row (2026-10-24); an older observation never overwrites a
  newer one. They are lost when the run's history is dropped, at shutdown, or
  beyond that bound.
- An unrecorded run's process group is not in the database, so after a daemon
  crash (not in a container, where the namespace dies with PID 1) startup
  recovery cannot kill its leftovers.
- The PID 1 init costs one extra small idle process.

## Bounds and observability

`GET /api/v1/daemon` → `diagnostics.run_history`: `storage_degraded`,
`deferred` (runs admitted without a row), `dropped` (history records lost),
`pending` (items waiting). `diagnostics.finalizers.inline` is kept and is always 0.

## Validation

New tests: a scheduled job keeps firing (observed through a file outside the
database) while `schedule_state` writes abort and the `runs` table is missing,
and its history appears after recovery; a run executes while the DB write lock
is held by another connection and later launches skip the database; a failing
`running` write does not kill the child; a worker restarts while its previous
terminal write fails; a worker runs with an unusable log root; history overflow
never holds capacity; the PID 1 reaper reports exit codes and reaps others.
Manual: as PID 1 in a fresh PID namespace, a job that backgrounds children left
35 zombies after 12 s without the init and 0 with it.

## Revisit when

Operators need history to survive a daemon restart during an outage (consider
a journal on a separate volume), the 1024 cap proves too small or large on the
5A profile, or a storage error class is found that should refuse launches.
