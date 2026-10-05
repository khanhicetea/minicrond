# ADR-8: Background-first durability, overload, and resource trade-offs

- Status: accepted; implemented in `b785adb..57063ba` (merged 2026-10-06; see [Implementation status](#implementation-status)). 5A is a planning profile only: no benchmark of it has been run
- Decision source: owner selected **1B, 2A, 3A, 4B, 5A** after reviewing alternatives
- Related: [ADR-6](0006-hybrid-log-storage.md), [ADR-9](0009-durable-execution-queue.md) (4B), [ADR-10](0010-log-disk-budget.md) (3A), [ADR-11](0011-read-admission.md) (5A-sized read limits), [audit](../../astra-audit-code.md), [agent guidance](../../AGENTS.md)

## Context

Almost all jobs/workers execute unattended. Live logs are mainly used during
setup and troubleshooting; 1–3 seconds of display lag is acceptable. Optimize
zero-viewer execution rather than instant live delivery. The following choices
are explicit approvals beyond display-lag tolerance, not inferences from it.

## Decisions and accepted costs

### 1B — Batch log fsync; preserve daemon-crash recovery

Write accepted log frames through to the OS/file before acknowledging capture;
do not leave accepted frames solely in daemon memory or the compression buffer.
Group disk syncs on a roughly 1–3-second cadence with a dirty-byte threshold and
final sync on EOF/seal/orderly shutdown. Sparse output must flush without another
line arriving; avoid periodic timers on idle writers.

Accept fewer disk syncs in exchange for possible loss of recent unsynced output
on OS crash/power failure. Storage stalls can extend the nominal sync window;
1–3 seconds is not an unconditional maximum loss bound. Preserve recovery after
a daemon-only crash, including required file/directory metadata ordering. Test
these guarantees rather than assuming a file write alone establishes them.

This approves changing log sync behavior, not weakening metadata/queue SQLite
transactions, changing archive commit-before-unlink ordering, or removing an
existing explicit strict-durability mode. Specify the exact interval, byte limit,
error handling, and configuration/API contract changes before implementation.

### 2A — Keep executing when log capture fails

When log storage is full/unavailable, keep draining stdout/stderr and discard
output that cannot be stored rather than stopping or indefinitely blocking the
child. Record missing-output/truncation and storage-failure evidence when
possible through a bounded path; avoid per-line error storms. Recover capture
when feasible under a bounded retry policy.

Accept incomplete diagnostics in exchange for continued useful work. This
choice concerns log capture only: timeout, cancellation, security checks, and
required execution-state persistence remain enforced. Do not report complete
capture when output was discarded. An optional strict per-job policy can be
considered separately; it is not required by this decision.

### 3A — Log disk budget outranks normal retention age

Delete the oldest eligible logs early when approaching the configured log disk
budget, even if their normal retention age has not elapsed. Accept shorter
historical coverage during noisy periods in exchange for predictable storage
use. Account for live/sealed buffers, archive/WAL space, and maintenance headroom;
an archive-only daily quota is not a complete disk-safety mechanism.

Define eligible data and pressure watermarks before implementation. Quarantined
corruption evidence needs a separate explicit purge policy. Do not delete
metadata, pending execution records, or unrelated files to reclaim log space.
If reclamation is insufficient, existing processes follow 2A and new queued work
follows 4B. The earlier 5 GiB example was illustrative, not an approved default.

### 4B — Queue bounded work durably for later execution

Prefer a bounded durable pending-execution queue over skipping immediately when
execution capacity is exhausted. Set maximum count/bytes and age, persist before
acknowledging durable enqueue, recover pending records after restart, and drain
at a bounded rate. Reject additional work explicitly when full or unable to
persist it; an unavailable database cannot accept a durable enqueue. Expiry and
rejection must be visible, not silent loss.

Accept late execution, extra persistence, and implementation complexity in
exchange for fewer missed runs. Keep retry/finalizer/archive budgets bounded as
well; a durable execution queue does not bound those independently. Revalidate
definition changes and respect overlap, retry, and catch-up policies. Specify
ordering/fairness, expiry outcomes, and crash/replay semantics before coding;
this decision does not promise exactly-once execution or authorize blind replay
of a possibly already-started command.

### 5A — Tune for a small, quiet server first

Use the small-server profile as the initial benchmark target: roughly 1 CPU /
512 MiB environment, 10 workers, four concurrent jobs, and modest aggregate
output. These are planning fixtures, not guaranteed capacity, fixed concurrency
defaults, or a 512 MiB daemon allocation. Child-process resources are additional.
Favor conservative caches, small queues, and limited archive concurrency; allow
configuration for larger deployments rather than charging all installations for
large-server throughput. Accept slower burst recovery and historical reads.

## Implementation status

The decision text above is unchanged. This section records what the merged tree
(`57063ba`) does; the per-finding table is in the audit's
[Implementation status](../../astra-audit-code.md#implementation-status).

| Choice | Chosen defaults and limits | Where |
|---|---|---|
| 1B | `logs.durability = "batch"` (default; `"frame"` stays strict). `logs.sync_interval` = **2000 ms** (accepted 100–60000; `0` selects the default), `logs.sync_max_dirty` = **1024 KiB = 1 MiB** per run buffer (accepted 1–65536). Frames are still written through to the file before capture proceeds. One shared timer is armed only while some writer is dirty; no ticker per writer. Final sync on pipe EOF, chunk seal/close and `StopArchiver`. | `0c8ce4b`, `6c92cbf`, `aedaf75`, `c659a0b` |
| 2A | Pipe write/rotate/sync failures degrade the writer: output is drained and discarded (counted), one log line per episode, retry with doubling delay 1 s→30 s on later lines (no timers), a `system` summary line on recovery, evidence in `index.json` (`dropped_frames`, `dropped_bytes`, `sync_failures`) and `log_truncated`. `Seal`/pump errors keep the real run status. | `0c8ce4b`, `4ce9b7e`, `ebc9173`, `1b13977` |
| 3A | `logs.disk_min_free` = **512 MiB**, on by default (clamped to a quarter of the filesystem; explicit `0` disables). `logs.disk_budget` = **0 (no byte budget)** by default. Watermarks are constants: budget starts above 90% and reclaims to 80%; headroom starts below the minimum and reclaims to 125% of it. Eligible: archived chunks of completed runs (oldest first), then sealed buffers of runs without a writer. Never: live buffers, active runs' chunks, SQLite metadata, pending execution records. Quarantine kept forever unless `logs.quarantine_keep_for` / `logs.quarantine_max_size` are set; it is never purged under pressure. See [ADR-10](0010-log-disk-budget.md). | `b7027c1`, `ef06454`, `683a6f9`, `fd8b82e`, `20ca6e2` |
| 4B | `[queue]`: `max_items` **100**, `max_per_job` **25**, `max_bytes` **256 KiB** (ceiling on accounted identity strings, about 250 B per item), `max_age` **900 s**, `drain_rate` **5/s**, `max_pending_retries` **500**; restart to change. Finalizer cap 256 (hard cap 4×, 2 min inline hold) and archiver id queue 4096 are code constants. Full queue: manual trigger → HTTP 429 `queue_full`; scheduled/retry → `skipped/queue_full`. Persistence failure → HTTP 503 `queue_unavailable`. Expiry → `skipped/queue_expired`. A possibly started run is never re-queued. See [ADR-9](0009-durable-execution-queue.md). | `d602d7c`, `786bb99`, `5530bc1`, `7b96e08`, `428b106` |
| 5A | Planning profile only (about 1 CPU / 512 MiB, 10 workers, four jobs, modest output). It sized the conservative defaults above and the `[reads]` limits of [ADR-11](0011-read-admission.md) (4 slots, 32 MiB estimated working set, 20 s work timeout). **It is not a capacity guarantee or runtime limit, and no benchmark or profile of this fixture exists.** | ADR-9/10/11 |

### Measurements and their limits

- Sparse output on one ext4 virtual disk (KVM guest, not tmpfs, not physical SSD/HDD): fsyncs per line **1.000 → 0.005** for `BenchmarkSparsePipe/batch` (200 lines, 1 ms apart, no viewer); strict mode unchanged. 3 runs, medians.
- **No power-loss or OS-crash test was run.** The guarantee tested is daemon-only crash recovery of unsynced frames (`TestDaemonCrashLosesNothingInsideTheUnsyncedWindow`). The 2 s window is nominal; storage stalls extend it.
- Not measured: daemon-level CPU/allocation/heap with active zero-viewer capture, archive-induced writer blocking, disk-pass reclamation latency, queue/retry/finalizer cost, anything for the 5A profile.

### Open items

1. 5A: run the small-server fixture (zero viewers first, then occasional readers) on real storage and revisit the defaults from the results.
2. Power-loss / OS-crash testing for 1B on physical storage.
3. Admission is not wired to `DiskPressure()`: when reclamation is insufficient, existing and new runs keep running and drop output (2A); "new work follows 4B" is not implemented. Decide whether a refused or queued admission under disk pressure is wanted.
4. Pending retries are in memory and lost on restart ([ADR-9](0009-durable-execution-queue.md) rejects durable retry timers for now). Queue and drop counters are process-lifetime; the `skipped` rows are the durable evidence.
5. A04 is only partly closed: `LogDB.usedBytes` read on the writer connection, inline archival in `Store.Close`/`finalize` under `context.Background()`, an in-progress fsync cannot be interrupted (abandoned at the deadline), and the 45 s shutdown budget is a target.
6. A09: the missed count under `catch_up=none` is a lower bound above 100,000 occurrences and is only warn-logged; no flag in the run model or API.
7. A15/ADR-9 caps that are constants (finalizer 256/4×/2 min, archiver 4096) may need configuration if operators need them.
8. Embedded web bundle (`internal/api/assets`) was not rebuilt; the UI does not render the SSE `gap` event. Run `make build-web` before release.
9. Metadata growth (soft-deleted definitions, peak `minicron.db` size) and writer-connection metadata reads on state-feeding paths are unchanged.

## Rollout and validation

The text below was written before implementation and is kept as the original
plan; the implementation status above records what was actually delivered.
Update configuration/operations docs and tests together with
implementation; do not treat this ADR as proof that behavior exists without the
tests named in the status files. Keep the audit's original evidence separate from
new measurements.

Measure on real storage with zero viewers first, then occasional diagnostics.
Cover sparse/bursty output, daemon crash versus OS/power loss, sync/write failure,
full disk with a continuing child, pressure pruning, queue bounds/expiry/restart,
definition changes while queued, and recovery without an execution storm.
Choose numeric limits from the small-server measurements. Revisit these choices
if observed workloads or capture/compliance requirements change.
