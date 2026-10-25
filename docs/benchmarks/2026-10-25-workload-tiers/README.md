# Workload tiers: tiny, small, normal, large (2026-10-25)

How the minicrond daemon spends CPU, memory and disk I/O at four realistic
sizes, with **zero log viewers** (the [AGENTS.md](../../../AGENTS.md)
primary benchmark). Everything below is measured; nothing is a capacity
guarantee.

- RSS and Go heap are the daemon process only (`VmRSS` of its PID; children
  excluded). RSS includes file-backed mapped pages, not just heap.
- Binary: tree `ae6503a` (clean), `CGO_ENABLED=0 go build -trimpath`, Go 1.27.3.
- Host: 4-vCPU KVM guest (AMD Ryzen AI 9 HX 370), ext4 on a real disk
  (`/dev/sda1`), data dir on that disk, not tmpfs. This CPU is fast; a small
  VPS core will take longer for the same work, so read CPU as relative.
- Harness: `scripts/bench-5a/run-profile.sh` with the new `tier-*`
  scenarios. 180 s measured window after a 20 s warm-up, 2 runs per tier,
  run serially. Cells are median [min–max] over the 2 runs. Only the daemon
  PID is measured; child processes are not counted.
- Pinning: tiny/small daemon on 1 CPU (`GOMAXPROCS=1`); normal/large daemon
  on 2 CPUs (`GOMAXPROCS=2`). Children run on the other CPUs (2-3 for every
  tier). The sampler shares CPU 3 with children.

## The four tiers

| Tier | Workers | Jobs | Output | Daemon CPUs |
|---|---|---|---|---|
| tiny (hobby VPS) | 1 (one ~100 B line/s) | 3, each `@every 1m` | a few lines per run | 1 |
| small (the 5A profile, `active`) | 10 (2 silent, 1 crash loop, 4 one-line/s, 2 bursty, 1 chatty ≈200 lines/s) | 6 (`@every 5s` ×4, `@every 1s` ×2) | modest | 1 |
| normal | 25 (10 silent, 8 one-line/s, 3 bursty, 2 chatty, 2 crash loop) | 60 (20 each at 10 s, 30 s, 1 min) | moderate | 2 |
| large | 100 (40 silent, 30 one-line/s, 10 bursty, 10 chatty ≈2000 lines/s total, 10 crash loop) | 300 (100 each at 5 s, 15 s, 1 min) | heavy | 2 |

Job kinds rotate through 4 s with a line a second, a 100-line burst, silent,
10 lines/s plus stderr, and an instant exit. The exact configs are in
[`raw/*.toml`](raw/). Cadences are shorter than typical cron schedules, so
runs per hour are higher than most real installs of the same size. This
measures a busy day, not an average one.

## Results

| metric | tiny | small | normal | large |
|---|---|---|---|---|
| **CPU** (daemon, ms per second) | 0.58 [0.56–0.61] | 15.2 [14.9–15.5] | 23.8 [22.9–24.7] | 158 [157–160] |
| CPU as % of one core | 0.06 % | 1.5 % | 2.4 % | 16 % |
| CPU in the busiest second (p95 / max, ms) | 2.7 / 7.6 | 22.6 / 30.9 | 78 / 184 | 311 / 364 |
| user / sys split (ms/s) | 0.33 / 0.25 | 8.9 / 6.3 | 14.6 / 9.2 | 91 / 67 |
| context switches/s | 15 | 368 | 737 | 5,460 |
| **RSS** median (MiB) | 23.6 | 48 | 107 [104–111] | 414 [397–432] |
| RSS peak / VmHWM (MiB) | 26 | 52 | 150 [137–163] | 571 [565–575] |
| Go heap in use at end (MiB) | 3.9 | 17 | 34 [25–43] | 264 [256–272] |
| goroutines / threads (max) | 30 / 11 | 87 / 24 | 214 / 82 | 963 / 348 |
| **Disk writes** (daemon, KiB/s) | 9.5 | 438 | 498 | 4,659 |
| write syscalls/s | 7 | 537 | 927 | 6,073 |
| read syscalls/s | 5 | 200 | 315 | 2,498 |
| Durable growth (KiB/min, see caveat) | 1.6 | 642 | 1,221 | 6,632 |
| &nbsp;&nbsp;metadata db | 0 | 101 | 203 | 997 |
| &nbsp;&nbsp;log archive db | 0 | 141 | 276 | 2,265 |
| &nbsp;&nbsp;live log buffers | 1.6 | 400 | 742 | 3,371 |
| Runs started in the 180 s window | 9 | 532 | 577 | 5,282 |
| **Latency** fire→run record started, max (ms) | 44 | 96 | 721 | 2,997 |
| fire→child process started p50 / p95 (ms) | 25 / 45 | 32 / 81 | 181 / 553 | 774 / 2,042 |
| worker restart overhead p50 / p95 / max (ms)¹ | – | 35 / 43 / 46 | ≈43 / 68 / 123 | ≈39 / 352 / 716 |
| Clean shutdown (s) | 0.04 | 0.12 | 0.23 | 0.82 |
| Daemon WARN/ERROR lines | 0 | 0 | 0 | 0 |

¹ Crash-loop workers only. Normal and large use a 5 s restart delay, and the
analysis script subtracts a fixed 2 s, so the raw table in
[`summary.md`](summary.md) shows about 3,040 ms; the figures here subtract the
real 3 s remainder.

Full per-metric table, including all runs' min/max: [`summary.md`](summary.md).
Per-run JSON and configs: [`raw/`](raw/).

> **Update, same day:** the memory figures below were measured with the Go default
> `GOGC=100`. [ADR-17](../../adr/0017-gc-pacing.md) proposes a daemon default of 50, which
> measured about 281 MiB median / 370 MiB peak for the large tier (down from 414 / 571) and
> 84 MiB for normal (down from 107), CPU unchanged. See
> [memory and commit costs](../2026-10-25-memory-and-commits/README.md). The tables below are
> left as measured.

## What this says about how minicrond handles load

**CPU is proportional to run starts and output, not to definitions.** A
hobby setup costs about half a millisecond of CPU per second, effectively
nothing. Small and normal sit at 1–2.5 % of one core. Large (≈29 run starts
per second plus ≈2,000 output lines/s) uses 16 % of 2 cores, about 0.5 core in
its worst second (364 ms of CPU in one second, about 0.36 core). Roughly 40 % of CPU is kernel time (fork/exec, pipe reads,
file writes), as expected for a supervisor.

**Memory is the first thing that stops scaling, not CPU.** RSS grows with the
number of *open runs* (workers plus in-flight jobs). Approximate open runs
(workers plus jobs running at a moment; estimated from the definitions, not
counted) vs median RSS: ~1 → 24 MiB, ~12 → 48 MiB, ~35 → 107 MiB,
~150 → 414 MiB. That works out to
roughly 1.7 MiB of Go heap per open run in the large tier (264 MiB / ~150),
the same figure the [5A baseline](../2026-10-22-5a-baseline/README.md)
flagged. It has still not been profiled, so the per-run buffer explanation
there remains a hypothesis. Consequence: **the large tier does not fit the
512 MiB small-server planning profile** (median 414 MiB, peak 571 MiB, and
children are extra). Tiny, small and normal fit with room to spare (≤150 MiB
peak).

**Disk I/O is steady small writes; the source is not attributed.** "Disk
writes" is the daemon process's `write_bytes` and write-syscall count from
`/proc/<pid>/io`. It covers everything the daemon writes (metadata DB and
WAL, log archive DB, live log buffer files) and excludes children's own
writes. It is **not** split by file or by SQLite vs log buffers. In the large
tier it wrote ~4.6 MiB/s (~270 MiB/min) while durable file growth was only
~6.6 MiB/min, roughly 40x amplification, so SQLite page/WAL rewrites and
batched flushes likely dominate over raw log appends. That is unverified.
Small and normal write 438 and 498 KiB/s; tiny is under 10 KiB/s. Log
buffers are write-through with grouped fsync (ADR-8 1B); fsync counts were
**not** measured (no `strace` on this host).

**Scheduling stays on time, but start latency grows under bursty fan-out.**
Up to normal, a scheduled fire becomes a started run within 0.75 s in the worst
case and a child process within about 0.55 s at p95. In the large
tier, 100 jobs share the same `@every 5s` boundary, so they all fire together;
the daemon (2 CPUs, shared with the host) starts them in a wave: p50 774 ms,
p95 2 s and max 3 s from fire to child start. There were no WARN/ERROR lines
and no queue-full or dropped runs, but **103–127 large-tier runs (about 3 % of
the `@every 5s` fires) were recorded `skipped` / `overlap_skip`**: those jobs
run ~4 s, so with a 5 s cadence the previous run was sometimes still active
at the next fire once start and finish latency reached seconds (the likely
cause; not isolated). Tiny, small and normal had no skipped runs. If
second-level precision matters at that scale, stagger the schedules or leave
more headroom between a job's duration and its interval.

**Workers restart promptly at every tier.** Overhead beyond the configured
restart delay was ~35–45 ms median; in the large tier the worst case was
0.7 s. Termination and shutdown were clean at every size (≤0.82 s).

The `failed` runs in every tier except tiny are the deliberate crash-loop
workers (they exit 1 every restart); `stopped` runs are workers stopped at
shutdown.

## Caveats (read before relying on the numbers)

- **180 s windows are short.** Worker logs are only sealed and archived on
  `logs.worker_flush_interval` (default 15 min) and the retention pass runs at
  `db_prune_at` (03:30). Neither happened inside any window, so archival CPU
  and I/O peaks, tier migration and pruning are **not** captured. "Durable
  growth" is the raw first-3-minutes size increase (uncompressed live buffers
  plus databases), extrapolated per minute. Do not multiply it by a day to get
  a disk figure; compression on archive and the log disk budget
  ([ADR-10](../../adr/0010-log-disk-budget.md)) change the steady state.
- **No fsync, syscall-mix or per-file write attribution.** `strace` is not
  installed here, so `STRACE=0` and those rows are empty. No pprof.
- **n = 2 per tier.** Ranges are tight in most rows but this is not a
  statistical study. RSS peak in normal and large varies by ±10 % between runs.
- **Single hardware.** One fast desktop-class CPU, all in a VM that shares the
  host with other work (host busy 1–11 %). Expect slower per-core numbers on
  typical cloud VPSes; use the ratios between tiers more than absolute values.
- **Scripted children.** Children are `sh` loops with `sleep`, so output is
  regular. Real programs will be burstier or quieter.
- **Zero viewers by design.** Reader cost and interference are not measured
  here; see ADR-11 and the earlier A06 benchmark.
- **Large-tier harness note.** The first large rep 2 aborted from a harness
  race (`find` over the data dir while the daemon removed a finished run's log
  directory). The harness now tolerates it, and that run was repeated with the
  same settings; the reported large rep 2 is the rerun. This does not look like
  a daemon fault.

## Rough sizing guidance from these results

| If you run roughly… | Expect daemon | Plan for |
|---|---|---|
| 1–3 jobs, a worker or two | <1 ms CPU/s, ~25 MiB RSS, <10 KiB/s writes | any VPS, 128 MiB is plenty |
| ~10 workers, a handful of frequent jobs | ~15 ms CPU/s, ~50 MiB, ~0.4 MiB/s writes | 1 vCPU / 256 MiB |
| ~25 workers, ~60 jobs | ~24 ms CPU/s, ~110–150 MiB, ~0.5 MiB/s writes | 1–2 vCPU / 512 MiB |
| ~100 workers, ~300 jobs, heavy output | ~160 ms CPU/s, 400–575 MiB, ~4.6 MiB/s writes | 2+ vCPU / **1 GiB+**; stagger schedules; budget log disk |

Add the memory and CPU of the jobs themselves; they are not included.

## Follow-ups

- Profile heap per open run (the ~1.7 MiB/run figure is the likely cause of
  the large tier's memory) and decide whether it can shrink; this would move
  the large tier toward the 512 MiB profile.
- Run a long window (≥30 min, over at least two `worker_flush_interval`
  cycles) on the normal and large tiers to capture archive and pruning cost,
  with `strace` for fsync counts.
- Add a staggered-schedule large variant to confirm that the 3 s start tail is
  caused by synchronized fan-out.

## Reproduce

```sh
CGO_ENABLED=0 go build -trimpath -o /tmp/minicrond ./cmd/minicrond
# short data-dir path required: the Unix socket path is limited to ~108 bytes
SCENARIOS="tier-tiny active tier-normal tier-large" REPS=2 DURATION=180 \
  WARMUP=20 STRACE=0 CHILD_CPUS=2-3 \
  scripts/bench-5a/run-profile.sh /tmp/minicrond ~/mcb/tiers
```

Needs `taskset` and python3; install `strace` and drop `STRACE=0` for
syscall counts. Run it on real disk, not tmpfs.
