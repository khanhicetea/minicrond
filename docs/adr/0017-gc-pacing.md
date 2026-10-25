# ADR-17: GC pacing — collect at +50% heap growth by default

- Status: proposed; the GC default is implemented in the working tree (not yet committed or owner-approved), and the commit-cost items under [Open](#open) need an owner decision
- Date: 2026-10-25
- Related: [ADR-8](0008-background-first-trade-offs.md) 5A (small-server tuning), [ADR-16](0016-execution-first.md), [benchmark](../benchmarks/2026-10-25-memory-and-commits/README.md)

## Context

The [workload tiers benchmark](../benchmarks/2026-10-25-workload-tiers/README.md)
found that resident memory is the first limit to bite: the large tier (100
workers, 300 jobs) needed 414 MiB median and 571 MiB peak RSS, past the
512 MiB small-server planning profile of ADR-8 5A. A pprof run showed the live
heap was only ~119 MiB; the rest was Go heap headroom. With the default
`GOGC=100` the collector lets the heap roughly double between cycles, and
per-run state (a zstd encoder, two pipe buffers, run bookkeeping) is created
and dropped at ~30 runs per second, so the headroom is large and mostly
garbage.

Zero-viewer workload addressed: many active workers and frequent short jobs,
nobody reading logs.

## Decision

The daemon calls `debug.SetGCPercent(50)` at start unless the `GOGC`
environment variable is set; an explicit `GOGC` always wins (`GOGC=100`
restores the previous behavior, `GOGC=off` and `GOMEMLIMIT` still apply).

## Alternatives measured

Large tier, 2 runs each, interleaved, median (full table in the
[benchmark](../benchmarks/2026-10-25-memory-and-commits/README.md)):

| Option | RSS median / peak MiB | CPU ms/s | Verdict |
|---|---|---|---|
| default GOGC=100 | 434 / 594 | 168 | baseline |
| **GOGC=50** | **281 / 370** | 172 | chosen |
| `GOMEMLIMIT=256MiB` | 244 / 318 | **312** | rejected: ~130 MiB live data makes the collector run constantly |
| pool zstd encoders and Pipe buffers | 355 / 411 | 158 | rejected: ~8% less CPU, but with GOGC=50 the median RSS rose to 346 MiB; more code for less memory benefit |
| pooling + GOGC=50 | 346 / 365 | 157 | rejected, same reason |

The simpler option that was rejected for being insufficient: pooling alone.
The simplest option, a one-line GC change, won on the first-order objective
(bounded resources) at no CPU cost.

Small and normal tiers (no regression): median RSS 50 → 43 MiB and
113 → 84 MiB; CPU 15.9 → 16.1 and 24 → 25 ms/s; latency unchanged.

## Consequences

- Resident memory falls by about a third at the large tier and 14–26% at
  small/normal, which brings the 100-worker/300-job case from 414 to ~281 MiB
  median (peak 370 MiB) on the benchmark host.
- The collector runs more often: CPU is flat within measurement noise in the
  benchmarks (up to about +1 ms/s, ≤4%, at the normal tier). GC pause times
  were not measured; the runtime's GC CPU fraction was 0.04% at the default
  setting in the profile.
- Operators can still override with `GOGC`. No configuration file setting was
  added: this is a runtime default, not a product limit, and the 5A profile is
  still not a capacity guarantee.

## Tried and not adopted

- **A fast path for runs with no output** (skip the three close-time fsyncs and
  the archive pass). Every run carries at least the daemon's own exit-status
  line, so no run is empty; the quiet-cron benchmark saw no change. Reverted.

## Open

These are measured findings, not decisions, and touch the durability contract
in AGENTS.md, so they need the owner:

1. **Metadata commits are the large-tier latency limiter.** About 4–5
   `synchronous=FULL` commits per run serialize on one SQLite writer: relaxing
   `[storage] synchronous` to `normal` (a measurement, not a proposal) halved
   block flushes (250 → 118/s), cut CPU by a third and cut median
   fire→child latency from ~850 ms to ~120 ms. Options that keep every
   acknowledged write durable: group commit (batch concurrent writes into one
   transaction), merging the schedule-state update into the run-row insert, or
   putting the three close-time log fsyncs on the existing grouped sync timer.
2. **Overload in a quiet-cron stress variant** (300 jobs, 80% silent at
   5 s) exhausted the concurrency slots with ~100 expected concurrent jobs.
   The cause is not isolated; it needs a per-slot hold-time measurement.
3. Not measured: fsync per call, archive and prune cost over a full worker
   flush interval, per-open-run heap beyond the profile above.

Revisit the GC default if a long-window run (archive/prune included) shows GC
CPU becoming material, or if a future change shrinks per-run state enough that
the headroom no longer matters.
