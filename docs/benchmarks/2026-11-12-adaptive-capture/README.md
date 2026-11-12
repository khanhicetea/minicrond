# Capture hot paths and adaptive capture (2026-11-12)

Three-way A/B for [ADR-21](../../adr/0021-adaptive-log-capture.md).

- **base** = tree `5058f20` (0.7.1).
- **hot** = base plus the capture hot-path changes: one flush per pipe read
  (bounded at 16 KiB or `logs.sync_max_dirty`) and prepared statements for
  `CreateRun`, `FinishRun` and the archive inserts.
- **adapt** = hot plus adaptive capture: plain first chunks, promotion at
  16 KiB, archive-time compression with one shared encoder, in-memory handoff
  of small sealed runs, encoder release at worker checkpoints. The binary was
  built before one fix: a run whose archive enqueue overflowed kept its
  sealed image in memory. The queue cap is 4096 runs and these workloads
  never reach it.
- 8-vCPU KVM guest (QEMU virtual CPU), Linux 7.0, Go 1.27.4,
  `CGO_ENABLED=0 go build -trimpath`, ext4 on `/dev/sda2` (real disk), zero log
  viewers. The daemon is pinned by `run-profile.sh` defaults; children run on
  CPUs 2-6 and the sampler on CPU 7 ([environment](environment.txt)).
- Daemon tiers: `scripts/bench-5a/run-profile.sh`, **120 s windows after a
  20 s warm-up, serial and interleaved**, order rotated per repetition
  (base/hot/adapt, adapt/base/hot, hot/adapt/base). 3 repetitions of
  `tier-large`, 2 of the other tiers. Cells are medians. Raw summaries are in
  [`raw/`](raw/).

## Daemon tiers

Daemon CPU (user + system), ms per second of wall time:

| tier | base | hot | adapt | adapt vs base |
|---|---|---|---|---|
| large | 148.5 | 129.2 | 122.4 | −17.6% |
| large, quiet | 104.3 | 92.2 | 85.2 | −18.3% |
| normal | 22.5 | 18.9 | 18.1 | −19.3% |
| 5A small (`active`) | 13.7 | 12.3 | 11.5 | −16.2% |

Memory, median RSS over the window (MiB):

| tier | base | hot | adapt | adapt vs base |
|---|---|---|---|---|
| large | 221.1 | 186.3 | 84.5 | −61.8% |
| large, quiet | 182.1 | 149.2 | 81.9 | −55.0% |
| normal | 59.0 | 55.0 | 34.2 | −42.0% |
| 5A small | 33.8 | 32.0 | 24.4 | −27.8% |

Peak RSS falls about as much (large 282.9 → 99.8 MiB). The Go heap at the end
of the window falls 49–80%.

Write syscalls per second:

| tier | base | hot | adapt |
|---|---|---|---|
| large | 6006 | 3130 | 3213 |
| large, quiet | 3760 | 1643 | 1634 |
| normal | 901 | 360 | 373 |
| 5A small | 526 | 242 | 245 |

Disk growth during the window (KiB/min, `du` of the data directory):

| tier | base | hot | adapt |
|---|---|---|---|
| large | 6816 | 3806 | 4074 |
| large, quiet | 4710 | 2905 | 3291 |
| normal | 2933 | 2556 | 2682 |
| 5A small | 1871 | 1641 | 1659 |

Of that, the archive database grows 1–10% less with adapt than with hot. Live
buffers grow 11–21% more, because a plain first chunk holds up to 16 KiB of
uncompressed output until it is sealed. At the end of each window the data
directory is slightly smaller with adapt than with hot (first repetition; large: 9.66 vs
10.04 MB; 5A: 1.12 vs 1.16 MB).

Unchanged within noise in every tier: bytes written (within 2.5%), block flushes and
write IOs, runs completed, worker restart latency, warnings and errors (0).
Read syscalls are 3–11% higher than base, already with hot (the 16 KiB pipe
read buffer). Child exit → run ended stays between 1.1 and 1.7 ms.

**Noise.** Large-tier fire → child p95 ranges from 1.1 s to 7.3 s within a single build
across this and the earlier two-way run (base itself: 1.2, 4.1, 5.3 s), so it
is not attributed to any change. The quiet-tier p50 for adapt (340 and
697 ms) is the same bimodal behaviour.

## Per run

`BenchmarkRunCost` captures one job run (stdout pipe, empty stderr pipe, exit
line, close) with the archiver not running, in `internal/logstore` on ext4.
3 × 2000 runs per shape. Medians:

| shape | base | hot | adapt |
|---|---|---|---|
| 1 line: time | 176 µs | 199 µs | 106 µs |
| 1 line: allocated | 879 KB | 650 KB | 37 KB |
| 4 × 1 line: time | 301 µs | 227 µs | 104 µs |
| 4 × 1 line: allocated | 889 KB | 660 KB | 38 KB |
| 100-line burst: time | 455 µs | 331 µs | 163 µs |
| 100-line burst: allocated | 917 KB | 712 KB | 113 KB |
| 50 × 20 lines (108 KiB): time | 2.80 ms | 0.87 ms | 0.90 ms |
| 50 × 20 lines: allocated | 1.24 MB | 1.03 MB | 1.12 MB |

Most of base's and hot's per-run allocation is the per-run zstd encoder; a run
that never passes 16 KiB no longer creates one. The chatty shape is promoted
after 16 KiB and costs about as much as hot (+3% time, +9% allocated for
the extra plain chunk). Raw output: [runcost.txt](raw/runcost.txt).

The archive pays for plain chunks once, at archive time. `archiveBlob` with
the shared encoder, same shapes:

| shape | plain chunk | archived | time | allocated |
|---|---|---|---|---|
| 1 line | 102 B | 91 B | 4.2 µs | 258 B |
| 4 × 1 line | 488 B | 186 B | 4.5 µs | 1.9 KB |
| 100-line burst | 10,857 B | 547 B | 14 µs | 17 KB |

The archived sizes are smaller than what hot stores for the same runs (106,
231 and 552 B), because one pass over the whole run compresses better than
one zstd block per pipe read.
