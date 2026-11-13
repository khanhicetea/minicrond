# ADR-22: Archive queued short runs on the group-sync pass

- Status: accepted (owner decision, 2026-11-13)
- Date: 2026-11-13
- Amends: [ADR-20](0020-log-archive-write-path.md) §2 ("a lone queued run is
  archived alone, at once")
- Related: [ADR-8](0008-background-first-trade-offs.md) 1B,
  [ADR-21](0021-adaptive-log-capture.md),
  [benchmark](../benchmarks/2026-11-13-happy-path/README.md)

## Context

ADR-20 batches archive commits, but only when several runs are already queued.
On a quiet or small daemon most runs finish alone, so each one still gets its
own archive transaction with its own WAL fsync and about 8 WAL pages for a few
hundred bytes of log. ADR-20 measured its 0.08 flushes per run with ten runs
sealed together and says so. Zero viewers read either copy, so this is pure
background cost.

Workload addressed: short scheduled jobs finishing a few per second or less,
nobody reading logs.

## Decision

Queued short runs wait for the next group-sync pass (`logs.sync_interval`,
default 2 s) instead of being archived one by one.

- The archiver takes a batch at once when 32 runs are queued (the ADR-20 batch
  size), when a pass asks for it, at shutdown, or after a fallback of twice the
  sync interval if no pass comes.
- The timer-driven pass first asks the archiver to commit what is queued and
  waits up to 500 ms (`archiveFlushWait`) for it. Buffers the archiver removed
  are dropped from the pass, so they are never fsynced. Past the bound the
  pass syncs the remaining buffers as before.
- With `logs.durability = "frame"` there is no pass, so nothing waits: runs
  are archived at once, as before.
- The shutdown sync does not wait for the archiver; `StopArchiver` drains the
  queue right after it.

Alternative rejected: a fixed linger in the archiver, independent of the sync
pass. The pass would then fsync buffers that are archived a moment later,
which costs more than it saves.

## Bounds, lifecycle, ownership

- No new goroutines. One timer per archive worker (two), and only while the
  queue is not empty.
- The queue stays bounded at 4096 runs and the handed-over images at 8 MiB
  (ADR-21); beyond the image bound the archiver reads the buffer file.
- A pass holds the group-sync lock for at most 500 ms longer while it waits.
  The fsync of live writers is delayed by the same bound when the archive
  database is slow.

## Durability

Unchanged. A finished run's buffer stays on disk until its archive commit, as
before (commit before unlink). Power-loss exposure for a finished run's last
output is still the group-sync window of ADR-20, plus the 500 ms bound when
the archive is slow.

## Accepted downside

- A finished run's buffer and its in-memory image live for up to one sync
  interval instead of milliseconds. A daemon crash therefore leaves more
  buffers for the startup orphan sweep.
- The first read of a just-finished run comes from the buffer file, not the
  archive.
- Live buffers on disk grow: +37% during the window on the large tier in the
  benchmark (lower on the two smaller tiers).

## Measurements

Interleaved A/B against main `379bfef`, ext4, zero viewers, 120 s windows,
2 runs per cell ([benchmark](../benchmarks/2026-11-13-happy-path/README.md)):

| tier | bytes written | write syscalls | daemon CPU | fire→child p50 |
|---|---|---|---|---|
| 5A small | −21% | −16% | −4% | 23.4 → 22.1 ms |
| normal | −21% | −6% | −6% | 87 → 76 ms |
| large | −22% | −10% | −6% | 364 → 294 ms |

Block flushes fell 9–10% on small and large. Runs completed are equal.

The measured binary still waited for the archiver in the shutdown sync, which
made shutdown slower (up to 0.6 s on small, 2.4 s on large). That wait was
removed afterwards; shutdown time was not measured again.

Tests: `TestQueuedRunsWaitForTheGroupSyncPass` (five runs wait, are archived
by one pass, and none of their buffers is fsynced).

## Revisit when

- Daemon crashes show the orphan sweep taking noticeably longer.
- A reader needs a just-finished run from the archive within the sync interval.
- `logs.sync_interval` is raised well above 3 s, which lengthens the linger.
