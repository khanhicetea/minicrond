# ADR-18: Commit the running transition and schedule state without their own fsync

- Status: proposed; implemented in the working tree, owner decision on the metadata durability change pending
- Date: 2026-10-25
- Related: [ADR-16](0016-execution-first.md), [ADR-17](0017-gc-pacing.md) §Open 1, [benchmark](../benchmarks/2026-10-25-relaxed-commits/README.md)

## Context

ADR-17 found that metadata commits limit the large tier: with
`synchronous=FULL` every run makes 4 commits on the single SQLite writer, each
with its own WAL fsync. They are the run row (`CreateRun`), the `running`
transition (`StartRun`), the terminal state (`FinishRunWithUsage`) and one
schedule-state upsert per fire. (ADR-17 counted 4–5; the pre-fire write is
skipped when the value is already stored, so it is 4.) Relaxing all of them to
`synchronous=normal` cut median fire→child latency 7×, but that weakens every
metadata write.

Zero-viewer workload addressed: many frequent scheduled jobs firing on shared
boundaries, nobody reading logs.

## Decision

With `storage.synchronous = "full"` (the default), two of the four writes are
committed with `synchronous=NORMAL` on the writer connection, which is then
switched back to `FULL`:

1. **`StartRun`.** The stored pid/pgid/process start ID are used for exactly
   one thing: stopping orphaned children after a daemon crash **in the same
   boot** (`CleanupRecovered` checks the boot ID). After a power loss or OS
   crash the children are gone, and recovery marks `pending` and `running`
   runs `interrupted` alike. Losing the transition then loses only
   `started_us` of a run that was interrupted anyway.
2. **The schedule-state upsert** (`last_fire_us`, `next_fire_us`).
   `next_fire_us` is only shown by the API. For `last_fire_us`, every
   scheduled fire (run, skipped, missed, caught up, queued) stores a run row
   with `trigger='schedule'` and `scheduled_for_us`, committed with full
   durability. When the scheduler loads the stored state, it takes
   `max(last_fire_us, MAX(scheduled_for_us))` (one seek on
   `idx_runs_schedule_occurrence`), so a lost upsert cannot fire an occurrence
   twice or over-count missed ones.

In WAL mode a NORMAL commit reaches the OS before it returns, so it survives
a daemon crash; it skips only the fsync. The next FULL commit (or checkpoint)
makes it durable. `CreateRun` and the terminal state stay fully synced. With
`synchronous = "normal"` configured, nothing changes.

Lifecycle: `Store.relaxed` pins the writer connection, sets NORMAL, runs the
statement and restores FULL with its own 5 s context, so a canceled caller
cannot leave the writer relaxed. If the switch or the restore fails, the
connection is discarded (`driver.ErrBadConn`), and database/sql reopens it
with the configured pragmas. There are no new goroutines, timers or queues.

## Alternatives

- **Group commit** of concurrent writes into one transaction: keeps every
  write fully synced, but needs a committer, per-caller cancellation rules
  (a caller timing out after its statement ran) and handling of a
  transaction that SQLite rolled back on its own. Not chosen now, but it can
  still be added later; this change does not block it.
- **Skip `StartRun` for runs shorter than about 1 s:** saves nothing for runs
  longer than the delay, and a daemon crash in that window orphans a child.
  Rejected.
- **Drop the per-fire schedule-state write entirely** (next fire from scheduler
  memory, the table written only on schedule change and shutdown): saves the
  statement, not just the fsync, but touches the API and recovery more.
  Deferred until measurements show the statement cost matters.
- **`synchronous=normal` for everything:** measured in ADR-17, not proposed.

## Accepted downside

After a power loss or OS crash, up to the last few relaxed writes can be lost:

- a run shows no `started_us` / pid (it is `interrupted` either way);
- `next_fire_us` is stale until the scheduler's first write;
- `last_fire_us` is stale, but it is rebuilt from the run rows. Exception: a
  fire whose run row was itself never stored (ADR-16 deferred history dropped
  while storage was failing) can be treated as missed, or fired again under
  `catch_up`, if a power loss hits at the same time.

## Tests

- `TestRelaxedSwitchesSynchronousAndRestores`: NORMAL inside the write, FULL
  after; a store configured NORMAL stays NORMAL.
- `TestRelaxedRestoresAfterStatementError`, `TestRelaxedRestoresWhenCallerCanceled`.
- `TestRelaxedWritesSurviveDaemonCrash`: a helper process commits both writes
  and dies by SIGKILL; the reopened database has both.
- `TestLastScheduledFire`: newest scheduled row of any status; manual and retry
  rows ignored; the query uses the occurrence index.
- `TestStaleScheduleStateUsesNewestScheduledRun` (scheduler): stale state plus
  a newer run row gives `missed_count` 2, not 6; fails without the fix.

No power-loss test was run (same gap as ADR-8 1B).

## Results

Interleaved A/B against `5f08d26` on a real ext4 disk, zero viewers, 120 s
windows, median of 3 (large) or 2 (normal, small) runs; full table in the
[benchmark](../benchmarks/2026-10-25-relaxed-commits/README.md).

| tier | block flushes/s | fire→child p50 ms | fire→child p95 ms | CPU ms/s |
|---|---|---|---|---|
| large | 254 → **190** | 793 → **478** | 2,096 → 1,509 | 166 → 150 |
| normal | 34 → 27 | 180 → 101 | 523 → 342 | 25 → 23 |
| small (5A) | 26 → 21 | 32 → 20 | 87 → 45 | 16 → 16 |

Large-tier `overlap_skip` per run fell from 115 [37–130] to 63 [1–81], which
is noisy. The exit→ended p95 rose 5–7 ms on large and normal (not isolated).
RSS is unchanged.

## Revisit

If a power-loss test on physical storage shows a NORMAL commit lost after a
later FULL commit returned (that would contradict SQLite's WAL ordering), or
if group commit is built, which would make the relaxation unnecessary.
