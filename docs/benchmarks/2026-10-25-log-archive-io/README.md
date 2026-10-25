# Log archive write path (2026-10-25)

A/B for [ADR-20](../../adr/0020-log-archive-write-path.md): large chunks
archived as files, batched archive commits, seal and pipe-EOF fsyncs folded into
the group sync.

- **base** = tree `ef18673`
- **new** = `ef18673` plus ADR-20. The binary was built before two error-path
  fixes (lock order when a batched buffer cannot be removed, sync of a chunk
  whose index write fails). Neither runs in these workloads.
- Same host and setup as the [relaxed commits](../2026-10-25-relaxed-commits/README.md)
  page: `CGO_ENABLED=0 go build -trimpath`, Go 1.27.3, 4-vCPU KVM guest (AMD
  Ryzen AI 9 HX 370), ext4 on `/dev/sda1` (real disk), zero log viewers.
  Daemon on 2 CPUs for normal and large, 1 CPU for small (`active`, the 5A
  profile); children on CPUs 2-3.
- Daemon tiers: `scripts/bench-5a/run-profile.sh`, **120 s windows after a
  20 s warm-up, 2 runs per cell, serial and interleaved** (base, new, base,
  new). Cells are the two runs. Raw summaries are in [`raw/`](raw/).

## Per run: where the bytes and flushes go

`internal/logstore/io_cost_bench_test.go` (opt-in: `MINICRON_IO_BENCH_DIR`,
`MINICRON_IO_BENCH_DEV`). It runs the log store alone on the real disk, with
the archiver running as in the daemon, and counts bytes this process wrote
(`/proc/self/io`) and block-device flushes (system-wide). Each run is 3 lines
of journal text, the exit line only, or ~4 MiB of journal text. There are 300
runs per scenario (6 large), 10 at a time, and 2 repetitions. Raw output:
[before](raw/io-per-run-before.txt), [after](raw/io-per-run-after.txt).

| per run | before | after |
|---|---|---|
| short: bytes written | 46.4 KiB | 10.1–10.3 KiB |
| short: block flushes | 1.44 | 0.08–0.09 |
| silent: bytes written | 41.4 KiB | 9.9 KiB |
| silent: block flushes | 1.34 | 0.08–0.09 |
| large: bytes written | 2,803 KiB | 1,081 KiB |
| large: block flushes | 15.7–17.0 | 2.3–2.7 |

**Before**, 34 KiB of the 46 KiB short-run cost was the archive transaction
(pages of five B-trees, written to the WAL and then checkpointed), and
1.75 MiB of a large run's 2.8 MiB was the copy of its chunks into SQLite.

**Split mode**, where runs are sealed with no archive and archived later (as
when the log database is down), also improved: buffer flushes per short run
went from 0.81 to 0.51. A first version that synced deferred seals one by one
was *worse* than before (2.0), because sequential fsyncs cannot share journal
commits. The pass now runs up to 8 at a time.

A second finding: ext4 issues a cache flush on every `fsync`, even of a clean
file. Re-syncing already-synced chunks before their archive commit cost 4–5
flushes per large run. The archiver now skips chunks this process already
synced (see ADR-20).

## Daemon tiers

| metric | small base | small new | normal base | normal new | large base | large new |
|---|---|---|---|---|---|---|
| block flushes/s (fsync proxy) | 20.6, 20.6 | **12.0, 12.1** | 26.7, 27.1 | **17.2, 17.2** | 194, 194 | **129, 126** |
| block write IOs/s | 66, 64 | **40, 39** | 81, 80 | **53, 53** | 601, 603 | **407, 397** |
| CPU ms/s | 15.8, 14.9 | 13.9, 14.3 | 23.4, 22.6 | 20.7, 19.5 | 148, 150 | **128, 125** |
| RSS median MiB | 43.6, 43.0 | 40.7, 42.1 | 89.9, 89.5 | 84.0, 85.4 | 271, 292 | 292, 288 |
| RSS max MiB | 45.6, 45.2 | 44.2, 45.0 | 115, 129 | 115, 121 | 418, 423 | 455, 460 |
| fire→child p50 ms | 20.2, 20.8 | 20.7, 19.9 | 101, 103 | 91, 81 | 494, 491 | **327, 370** |
| fire→child p95 ms | 47, 46 | 48, 46 | 357, 328 | 314, 270 | 1,447, 1,353 | 1,055, 1,209 |
| child exit→run ended p50 ms | 18.3, 15.9 | **0.67, 0.94** | 31.9, 27.7 | **0.73, 0.66** | 28.9, 31.0 | **0.50, 0.56** |
| runs started in window | 356, 353 | 357, 355 | 387, 386 | 392, 388 | 3,526, 3,513 | 3,524, 3,523 |
| daemon WARN/ERROR lines | 0 | 0 | 0 | 0 | 0 | 0 |

Reading it:

- **Flushes fall by a third or more on every tier** (−42% small, −36% normal,
  −34% large), and write IOs by about as much. These are system-wide counters
  on an otherwise quiet host.
- **Run completion no longer waits for an fsync.** Child exit → run ended falls
  from 16–32 ms to under 1 ms, because sealing used to fsync the chunk, index
  and directory inline.
- **CPU falls 10–15%** on normal and large, and less clearly on small.
  Large-tier start latency (p50) falls by a quarter to a third.
- **Bytes the daemon writes (`write_bytes`) barely move.** The counter is
  charged when a page is dirtied, and live buffers dominate it. 120 s windows
  include no worker checkpoint (every 15 min), so file chunks (change 1) are
  not exercised here. The per-run numbers above cover them. Archive database
  growth is unchanged, as expected: short runs are still blobs.
- **Peak RSS on large is ~35 MiB higher** (455/460 vs 418/423 MiB) while the
  median is unchanged. Not investigated. Candidates are the up-to-8-MiB blob
  batch per archive worker, and closed writers waiting up to one sync interval
  for the group pass (as before, but now without an EOF sync).

## Bug found by this benchmark

The first "new" run of the large tier grew to 1.5 GiB of heap. When the group
sync timer fired during a slow pass, the single-pass guard left the fired timer
in place. It looked armed, so nothing armed a new one, and dirty writers (each
holding a zstd encoder) accumulated forever. Fixed before the runs above, with
`TestGroupSyncTimerDoesNotStackPasses`, which fails on the buggy version. The
invalid run is not reported.

## Not measured

- Power loss: the deferred seal window has not been tested by cutting power.
- Worker checkpoints and file chunks inside the daemon (15-min cadence), and
  retention or disk-pressure deletion of file chunks at scale.
- Filesystems other than ext4 (`data=ordered`).
