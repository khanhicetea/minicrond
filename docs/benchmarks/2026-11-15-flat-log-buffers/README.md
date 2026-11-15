# Flat log buffer files (2026-11-15)

Measures [ADR-24](../../adr/0024-flat-log-buffers.md): run log buffers as flat
files `logs/<run>.json` + `logs/<run>.NNNNNN.zst` instead of a directory
`logs/<run>/` per run. Asked for on disk cost, not CPU.

- Host and method as in [2026-11-13](../2026-11-13-happy-path/README.md):
  8-vCPU KVM guest, Linux 7.0, Go 1.27.4, `CGO_ENABLED=0 go build -trimpath`,
  ext4 on `/dev/sda2` (real disk), zero viewers, `scripts/bench-5a/run-profile.sh`,
  120 s windows after a 20 s warm-up, children on CPUs 2-6, sampler on CPU 7.
- Builds: `flatbase` = working tree before the change (main `0103f14` plus
  uncommitted work), `flat` = the same with ADR-24. Interleaved, order rotated,
  2 repetitions per tier (`.bench-happy/ab-flat.sh`, compared with
  `cmp-flat.py`). Full table: [`raw/ab-flat-vs-flatbase.txt`](raw/ab-flat-vs-flatbase.txt).
- `daemon.log` of every run grepped: no ERROR lines.

Outcome: adopted by owner decision on 2026-11-15 despite the neutral result,
for the directory fsyncs it saves while sealed buffers wait for the archiver
(not exercised by these tiers). The migration from per-run directories that
the measured build carried was dropped: nothing runs in production yet.

## Result: no disk change outside noise

Medians of 2 runs, change flat vs flatbase:

| metric | 5A small (`active`) | normal | large |
|---|---|---|---|
| bytes written (write KiB/s) | 0.0% | +1.1% | +0.1% |
| block write IOs/s (system-wide) | −0.2% | −38%, ranges 68–659 vs 70–381 | 0.0% |
| block flushes/s (system-wide) | +1.6% | −45%, ranges 22–213 vs 22–108 | −1.5% |
| du growth | −1.2% | +2.0% | −2.8% |
| daemon CPU | +0.9% | −3.0% | −3.7% |
| runs succeeded | 332 / 332 | 344 / 344 | 3,320 / 3,319 |

The normal tier's flush and IO medians are dominated by one stalled sample in
each build (the disk stall noted on 2026-11-13); the ranges overlap, so it is
noise, not a gain. Every other disk metric moved by about 2% or less, in both
directions.

## Syscalls: the mechanism works, and is small

`strace -c` for 30 s after a 40 s large-tier window, one run per build
([`raw/strace-tier-large.txt`](raw/strace-tier-large.txt)). Per run, using
`renameat` (one index install per run) as the run count (731 vs 894):

| syscall per run | flatbase | flat |
|---|---|---|
| `mkdirat` | 1.08 | 0 |
| `unlinkat` | 3.78 (0.94 failing) | 2.03 (none failing) |
| `fsync` | 2.08 | 2.00 |
| `openat` | 4.18 | 3.81 |
| all syscalls | 260 | 238 |

About three fewer filesystem syscalls per run out of roughly 250. The short-run
fast path (ADR-21/22 image handoff) already removed a buffer with three unlinks
before this change, so `os.RemoveAll`'s ~14 syscalls never applied to most
runs; what flat files remove there is one `mkdir` and one `rmdir`, and the
inode and directory block they allocate. At the large tier's ~28 runs/s that
metadata does not show up in bytes written or block IOs.

## Not caused by the change

`TestSustainedFailuresWithDegradedMetadataStayBounded` (executor) fails
intermittently under full-suite load on its peak-goroutine bound. With
`GOMAXPROCS=2`, 15 runs each: base `0103f14` failed 4, the flat log store on
the same base failed 2, peaks overlapping (79–217 vs 85–118 when failing).
A pre-existing load-sensitive bound, not this change.
