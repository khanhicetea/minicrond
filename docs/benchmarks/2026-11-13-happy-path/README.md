# Happy-path batch and single-change experiments (2026-11-13)

An audit looked for costs a successful run pays for the sake of rare failure
cases. 16 changes were implemented and measured, first together and then the
contested ones alone. **Read this before re-proposing any of them.**

- Host: 8-vCPU KVM guest (QEMU virtual CPU), Linux 7.0, Go 1.27.4,
  `CGO_ENABLED=0 go build -trimpath`, ext4 on `/dev/sda2` (real disk), zero log
  viewers. Daemon pinned by `run-profile.sh` defaults, children on CPUs 2-6,
  sampler on CPU 7.
- `scripts/bench-5a/run-profile.sh`, 120 s windows after a 20 s warm-up,
  serial and interleaved, order rotated per repetition.
- The disk on this host stalls now and then: a metadata write hit its
  deadline in 7 large-tier runs across the single-change sessions below, 3 on
  the baseline and 4 on experiments (two of them in the `schedule_state`
  session). Treat large-tier skip counts and p95 values as noisy.
- `summary.json`'s `daemon_log_warn_error` reported 0 for runs whose
  `daemon.log` had ERROR lines. Grep the log; do not trust that field.

## What was adopted

| change | where | result |
|---|---|---|
| No-trade set: pidfd wait, indexed recovery query, two unused indexes dropped, prepared `StartRun`, pooled pipe buffers, held run-lock reference, parallel writer fsync, fewer seal and cleanup syscalls | commit `379bfef` | part of the batch result below; not measured alone |
| Archive queued short runs on the group-sync pass | [ADR-22](../../adr/0022-archive-on-group-sync.md) | −21% bytes written on every tier, large-tier fire→child p50 364 → 294 ms |

## What was tried and not adopted

Each was measured alone against main `379bfef` (3 tiers, 2 runs per cell).
"Noise" means the min–max ranges of the two builds overlap. Full tables:
[`raw/`](raw/).

| change | measured | why not |
|---|---|---|
| **Skip `index.json`** for a run handed to the archiver in memory | −3% bytes written; CPU, flushes and latency within noise | A daemon crash between seal and archive commit loses the run's job/kind label and takes the salvage path. Not worth 3%. A tmpfs profile had put the index write at about 45% of short-run capture CPU; that did not show on a real disk with the whole daemon running. |
| **Release the concurrency slot before seal and the terminal commit** | no metric outside noise; large-tier overlap skips were 6 and 71 in two identical runs | It loosens what `max_concurrent_runs` and `on_overlap = "skip"` mean, for no measured gain. The idea that slot hold time explains the large-tier overlap skips is not supported at this load. |
| **`wal_autocheckpoint` 1000 → 4000 pages** | small and normal: flushes +8–9%, CPU +4–5%; large: CPU −3%, block write IOs −5%; bytes written −2–3% | Worse on the small-server profile (ADR-8 5A), and both WAL files grow to their new ceiling: +20–25 MiB of disk during a run. |
| **No per-fire `schedule_state` write** (next fire published in memory, row written when the loop stops) | −3–5% bytes written; large-tier fire→child p50 353 → 332 ms; exit→ended p95 +0.2–0.3 ms on small and normal; 0.7% fewer runs completed on large | Small gain, and it widens the case where an occurrence can fire again after an unclean stop (ADR-18 already accepts it for power loss). The small regressions are unexplained. ADR-18 had deferred this "until measurements show the statement cost matters"; they show it barely does. |
| **Release the admission lock before the run-row insert** | large tier: about 1,450 runs completed instead of 3,355, about 1,760 skipped | Every job of a schedule boundary then takes its capacity slot at the same instant. A wave larger than `max_concurrent_runs` overflows into the durable queue, and at 30 runs/s the queue never drains. Do not retry unless the capacity take moves behind the insert too. Note on `Service.admission`. |

Also left alone, without measurement:

- **Dropping `log_runs`** from the archive database: `log_chunks` references it,
  so it needs a table rebuild, and the three prune paths count removed runs
  through it.
- **Dropping `idx_runs_end_time` and `idx_runs_job_time`:** only viewers use
  them, but without them a per-job run listing scans the whole history.
- **Writing the start-up system line from the stdout pump:** tests cover the
  stalled-sink behaviour of the separate goroutine on purpose; the saving is
  microseconds.

## Not built: needs an owner decision

- **X1, deferred history sync:** commit the run row and terminal state without
  their own fsync and sync on the 1–3 s cadence. The largest remaining lever
  (two fsyncs per run on the launch and completion path). After a power loss a
  run that finished in the window can be recorded `interrupted`, which
  AGENTS.md does not allow without an explicit exception. Group commit is the
  alternative that keeps every write synced.
- **X5, lazy running state:** do not commit `StartRun` for runs that end
  within about a second. ADR-18 rejected it because a daemon crash in that
  window orphans a child outside containers. Saves one unsynced commit per
  short run, no fsync.

## Finding: queue overload on the large tier

On `tier-large-quiet`, and on `tier-large` with the admission change, the
daemon enters a state it does not leave: once the durable queue holds runs,
every new trigger joins it, the 5 s jobs skip for overlap while their previous
occurrence is queued, and about half of all runs are skipped with fire→child
p95 near 25 s. The baseline does this too on the quiet tier. It is not fixed
by anything on this page and is a better target than further per-run savings.

## The batch as a whole

Base `4b06ae0` against all changes together except the admission one, which
was backed out after the first pass. Medians of 2 runs (3 for large). This is
not the tree that was committed: it also contains the four changes that were
later not adopted.

| metric | 5A small | normal | large | large, quiet |
|---|---|---|---|---|
| daemon CPU | −17% | −16% | −27% | −16% |
| block flushes/s | 14.8 → 11.8 | 20.7 → 14.9 | 125 → 90 | 94 → 71 |
| bytes written | −34% | −35% | −37% | −28% |
| median RSS | same | −7% | −10% | −8% |
| threads (max) | 25 → 15 | 81 → 16 | 348 → 16 | 364 → 17 |
| fire→child p50 ms | 22.7 → 21.6 | 86 → 77 | 364 → 297 | 340 → 309 |
| runs succeeded | 334 → 333 | 355 → 357 | 3,307 → 3,342 | 1,531 → 1,564 |

The committed tree (no-trade set plus ADR-22) was not measured as one build
against `4b06ae0`. From the pieces: main `379bfef` measured about 100–104 ms/s
CPU and 98–100 flushes/s on the large tier across five sessions, against
123 ms/s and 125 for `4b06ae0`, and ADR-22 adds its own table on top.

The raw per-run summaries of these sessions were not kept in the tree.
