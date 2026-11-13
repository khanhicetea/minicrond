# ADR-23: Deferred history sync (experiment X1)

- Status: proposed (experiment, owner decision pending)
- Date: 2026-11-13
- Amends, if adopted: [ADR-18](0018-relaxed-commits.md) (which kept the run
  row and terminal state fully synced)
- Related: [ADR-16](0016-execution-first.md), [ADR-19](0019-history-unavailable-policy.md),
  [ADR-8](0008-background-first-trade-offs.md) 1B,
  [happy-path benchmark](../benchmarks/2026-11-13-happy-path/README.md) ("Not built: X1")

## Context

After ADR-18, every run still makes two metadata commits with their own WAL
fsync under `storage.synchronous = "full"` (the default): the run row
(`CreateRun`, before launch, under the admission lock) and the terminal state
(`FinishRunWithUsage`, before the run's capacity slot is released). All
metadata writes share one writer connection, so each fsync also delays every
other write queued behind it. The happy-path benchmark names this the largest
remaining lever: two fsyncs per run on the launch and completion path.

Zero-viewer workload addressed: many short scheduled jobs on shared
boundaries, nobody reading logs. The cost is per run, independent of viewers.

## Decision

Experiment, on only when `synchronous = "full"` is configured and the store is
writable:

1. **The run row and the terminal state are committed relaxed** with the
   ADR-18 machinery (`Store.relaxed`, prepared statements via
   `execRelaxedPrepared`): `synchronous=NORMAL` on the pinned writer, then
   back to `FULL`, discarding the connection if the restore fails. In WAL mode
   such a commit reaches the OS before it returns, so it survives a daemon
   crash.
2. **They become durable on a cadence.** While any relaxed commit is not yet
   covered by a synced one (`dirty`), one `time.AfterFunc` timer is armed.
   When it fires (2 s, constant `historySyncInterval`) it makes one small
   `synchronous=FULL` commit on the writer: an upsert of the `meta` key
   `history_synced_us`. A transaction that changes no page does not fsync, so
   it is a real write. Its WAL fsync covers every frame written before it, so
   all earlier relaxed commits become durable at once. Once X1 is on, the
   ADR-18 relaxed writes (`StartRun`, schedule state) mark the store dirty
   too, so they are now also durable within the interval.
3. **Per-write FULL stays** for:
   - jobs with `on_history_unavailable = "skip"`: the executor writes their
     run rows and terminal states with `CreateRunSynced` / `FinishRunSynced`,
     inline and in the background history writer (ADR-19 promises them a
     durable pending row before they run);
   - idempotent API triggers (`AdmitIdempotentRun`), the durable queue
     (enqueue, dequeue; ADR-9), definitions, alert channels, alerts, worker
     state and every other metadata write: unchanged;
   - everything when `synchronous = "normal"` is configured (no change, no
     timer) or the store is read-only.
4. **Orderly shutdown:** `Store.Close` stops the timer and makes a final sync
   commit if dirty. The daemon closes the store after the executor has
   stopped.
5. **No configuration surface.** One environment variable, read once at
   `store.Open`: `MINICROND_HISTORY_SYNC=commit` restores per-write FULL for
   the run row and terminal state (ADR-18 behavior, no timer). Any other
   non-empty value is logged and ignored. It exists so a benchmark can A/B
   inside one binary.

Code: `internal/store/relaxed.go` (`historySync`, `markDirty`,
`syncHistory`, `closeHistory`, `execHistory`, `HistorySyncStats`),
`CreateRun`/`CreateRunSynced`/`FinishRunWithUsage`/`FinishRunSynced` in
`store.go`, and `historySynced(d)` in the executor (`admitRow`, `finishRun`,
the background history writer).

## Alternatives

- **Group commit** of concurrent history writes into one transaction: keeps
  every write fully synced and still cuts fsyncs under load, but needs a
  committer goroutine, per-caller cancellation rules (a caller that timed out
  after its statement ran) and handling of a transaction SQLite rolled back on
  its own. It also does not help a lone run: one run still pays two fsyncs.
  Not chosen for the experiment; it remains the option that keeps the
  AGENTS.md durability rule without an exception.
- **Write-behind by default** (queue history in memory and write it from the
  background history writer, like ADR-16's degraded mode): removes the commit
  from the launch path entirely, but buffers accepted state only in RAM, so a
  daemon crash loses it, and recovery cannot mark the run `interrupted`
  because it has no row. Rejected: X1 keeps daemon-crash durability.
- **`synchronous=normal` for everything:** measured in ADR-17, not proposed;
  it would also weaken queue and definition durability.
- **A ticker:** rejected; the timer exists only while dirty, so an idle
  daemon has no wakeups.

## Bounds and lifecycle

- At most one timer, armed only while dirty; no goroutine while idle. A burst
  of N runs within the interval costs one extra FULL commit, not N.
- The sync is one statement with a 5 s timeout (`historySyncTimeout`). It
  takes the writer connection like any other write; it never runs on the
  launch or completion path. Execution waits for history no longer than
  today (the relaxed commit is the same statement without the fsync).
- `dirty` is cleared before the sync write and set by every relaxed commit
  after it lands, so a commit racing the sync either is covered by it or arms
  the next one. It is also set when a relaxed statement failed (a timed-out
  commit may have reached the WAL).
- A failed sync leaves the store dirty and re-arms the timer: one attempt per
  interval while storage fails, logged once per failure episode
  (`syncing deferred run history failed`) and once on recovery.
- A relaxed setting cannot leak into a later FULL write: unchanged ADR-18
  restore-or-discard logic. The sync commit runs on the pool's writer, which
  is always at the configured `FULL` outside `relaxed`.
- `Close` serializes with a running timer callback (`syncMu`), so the callback
  never writes to a closed database. If the daemon skips `Close` because a
  maintenance loop is stuck, no final sync happens; the data still survives
  that daemon exit (it is in the OS), only not a power loss in the next
  moments.

## Durability and accepted downside

Daemon crash (process killed, OOM, panic): nothing changes. Every relaxed
commit is in the OS page cache and is read back at the next start.

OS crash or power loss: run rows and terminal states committed in the last
~2 s (plus the duration of a sync, longer during a storage stall when syncs
fail) can be lost. Concretely:

- **A run that finished can come back as `interrupted`** (its pending row was
  synced, its terminal state was not): history then shows a failure for a run
  that succeeded, run metrics count it as failed, and its exit code, ended
  time and resource usage are gone.
- **A fired run can have no row at all.** Its log file can still exist without
  a run. For a scheduled run, the occurrence uniqueness that the row provides
  is lost too: the scheduler rebuilds its last fire from stored rows
  (ADR-18), so under `catch_up` that occurrence can be **run a second time**
  after the reboot, or counted as missed. Jobs for which a duplicate run is
  unacceptable can set `on_history_unavailable = "skip"`, which keeps their
  history fully synced.
- `started_us` / pid of a run lost as with ADR-18.

WAL ordering is preserved: any later FULL commit (sync, queue, definition,
alert, skip-job history) makes every earlier frame durable, so after a power
loss the database is a prefix of the committed history; an alert or queue
row never survives without the run row committed before it.

This is an exception to AGENTS.md ("display lag alone does not permit ...
incorrect final state"), which is why it needs an owner decision. ADR-8 1B
already accepts a ~1–3 s loss window for log output after an OS crash; X1
applies the same window to run history. Background execution benefits by
removing two fsyncs from each run's launch and completion path and from the
shared writer.

## Tests

Store (`internal/store/history_sync_test.go`):

- `TestDeferredHistorySurvivesDaemonCrash`: a helper process commits a run row
  and terminal state (and a second pending row) relaxed, never syncs, and dies
  by SIGKILL without `Close`; the reopened database has all of them and no
  sync record.
- `TestHistorySyncTimerFiresOnceAndDisarms`: idle store has no timer; four
  relaxed writes arm it once; it fires once, writes the sync record, leaves the
  writer at FULL, clears `dirty` and is not re-armed while idle; a later write
  arms it again.
- `TestHistorySyncRetriesAfterFailure`: with the `meta` table renamed away the
  sync fails, stays dirty and retries; it succeeds once the table is back.
- `TestCloseSyncsDeferredHistory`: `Close` syncs when dirty, not when clean.
- `TestSyncedHistoryWritesAreFull`: `CreateRunSynced` / `FinishRunSynced` make
  no relaxed write and arm nothing.
- `TestHistorySyncOffWithNormal`: `synchronous = "normal"`: no deferral, no
  timer, writer stays NORMAL.
- `TestHistorySyncEnvRestoresPerWriteFull`: `MINICROND_HISTORY_SYNC=commit`:
  no relaxed history write, no timer, no sync on `Close`.
- `TestHistorySyncOffWhenReadOnly`.

Executor (`internal/executor/history_sync_test.go`):

- `TestSkipJobHistoryIsCommittedFull`: a `skip` job's run row and terminal
  state make no relaxed write; a default job's make exactly two.

No power-loss test was run (same gap as ADR-8 1B and ADR-18).

## Measurements

Pending.

## Open items

- Benchmark against main on real storage (fsync count, fire→child and
  exit→ended latency, CPU), A/B with `MINICROND_HISTORY_SYNC=commit`.
- Owner decision on the AGENTS.md exception (finished run recorded as
  `interrupted`, duplicate scheduled occurrence under `catch_up` after a power
  loss). If adopted, AGENTS.md and the operations/configuration docs must
  state the new window; if rejected, consider group commit instead.
- Whether a pending sync should be skipped when another FULL commit happened
  in between (not built: syncs are at most one per 2 s, so the saving is
  small).
- `HistorySyncStats` is not exported as a metric; add one if the experiment
  is adopted.
- Power-loss test on physical storage.
