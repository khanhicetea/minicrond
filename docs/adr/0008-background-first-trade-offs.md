# ADR-8: Background-first durability, overload, and resource trade-offs

- Status: accepted design direction; not implemented by this documentation change
- Decision source: owner selected **1B, 2A, 3A, 4B, 5A** after reviewing alternatives
- Related: [ADR-6](0006-hybrid-log-storage.md), [audit](../../astra-audit-code.md), [agent guidance](../../AGENTS.md)

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

## Rollout and validation

Existing code, configuration defaults, and operational guarantees remain as
implemented until follow-up changes land. Update configuration/operations docs
and tests together with implementation; do not treat this ADR as proof that the
behavior already exists. Keep the audit's original evidence separate from new
measurements.

Measure on real storage with zero viewers first, then occasional diagnostics.
Cover sparse/bursty output, daemon crash versus OS/power loss, sync/write failure,
full disk with a continuing child, pressure pruning, queue bounds/expiry/restart,
definition changes while queued, and recovery without an execution storm.
Choose numeric limits from the small-server measurements. Revisit these choices
if observed workloads or capture/compliance requirements change.
