# minicrond: design and performance decisions

## Workload and priorities

- Treat approximately **99.9999% of jobs/workers as unattended**, with **zero live-log viewers**. This is the owner's product assumption, not measured telemetry. People inspect logs mainly during setup or when something fails.
- Prefer the design that makes background execution cheaper and more reliable. **1–3 seconds of live-log display lag is acceptable**; subsecond delivery and polished live-view UX are not optimization goals.
- Decision order: correctness/security and recoverable history → bounded resources and low background cost → simplicity/maintainability → on-demand diagnostic speed.
- Display lag alone does **not** permit delayed termination, broken worker restart policies, lost accepted logs, or incorrect final state. The explicit owner-approved overload/durability exceptions below are separate decisions. Rare readers must still be prevented from exhausting or blocking the daemon.

## Owner-approved choices: 1B, 2A, 3A, 4B, 5A

Accepted design direction, **not implemented by these documentation changes**. See [ADR-8](docs/adr/0008-background-first-trade-offs.md) for scope, consequences, and implementation requirements.

1. **Batched log durability:** write accepted frames through to the OS/file immediately; group fsync on a roughly 1–3-second cadence with a byte limit and final sync. Preserve daemon-crash recovery. Accept recent unsynced-log loss after OS crash/power failure; storage stalls can extend that nominal window. No accepted-output buffering solely in RAM.
2. **Continue on log-storage failure:** keep the child running and drain stdout/stderr, discarding output that cannot be stored. Report missing output/storage failures when possible via bounded observations. This does not relax timeouts or required execution-state persistence.
3. **Disk budget before retention age:** prune oldest eligible logs early under pressure. Account for all log tiers and disk headroom; quarantine needs a separate explicit policy. Never reclaim metadata or pending execution records as log space.
4. **Bounded durable queue before skipping:** queue excess execution work for later with count/byte/age limits, persist before acknowledging durable enqueue, and drain at a bounded rate. Reject explicitly when full or persistence is unavailable; expose expiry. Define replay semantics and respect definition/overlap/retry/catch-up policies, without assuming exactly-once execution.
5. **Small-server-first tuning:** benchmark roughly 1 CPU / 512 MiB, 10 workers, four concurrent jobs, and modest output. This is a planning profile, not a capacity guarantee or fixed runtime limits; child resources are additional. Favor small caches/queues and limited archive concurrency.

Exact sync/byte thresholds, disk budget/watermarks, queue limits/expiry/replay policy, and numerical log-volume targets remain implementation decisions to document and measure. The earlier 5 GiB disk example is not a selected default.

## System design defaults

- Keep capture, execution, recovery, and terminal persistence independent of whether anyone is watching.
- Avoid speculative live-tail payload copies/caches, per-frame fan-out, subscriber queues, polling, or viewer timers with zero viewers. Prefer stored-log cursor reads on demand. Any reader cache must be justified, byte-bounded, and released when no longer needed.
- Prefer simple bounded follow polling every 1–3 seconds. Existing SSE can remain as batched delivery with coalesced sequence hints rather than payload queues; preserve public resume/event contracts. Do not rewrite transports solely to improve live latency.
- Bound reader admission, page bytes, work, and lock occupancy. Prefer throttled/retryable diagnostics over slowing capture or process control. Keep sequence order, retention-gap reporting, and final-tail-before-done behavior correct.
- Favor bounded batching/coalescing where it reduces background CPU, allocations, wakeups, syscalls, or I/O. Define byte/time limits and EOF/seal/shutdown behavior; do not create a ticker per idle writer.
- **Visibility, file writes, fsync, and archival are different boundaries.** ADR-8 authorizes batched log fsync with its explicit power-loss trade-off, not buffering accepted output only in RAM, weakening metadata/queue SQLite durability, or changing archive commit-before-unlink ordering. Update affected log durability contracts, configuration, operations docs, and recovery tests together when implementing it.
- Keep the file-first capture/asynchronous archive architecture unless measurements justify changing it. Archive cadence follows buffer pressure, throughput, disk limits, and recovery—not viewer freshness. Reads must not wait for the next archive checkpoint to see stored output.
- Bound retries, finalizers, archive discovery/backlogs, alert work, and disk use globally, not only active job count. Prefer the approved bounded durable execution queue on overload; never hide unbounded work in goroutines, channels, or timers. Do not silently discard required state to satisfy a memory cap. Missing log output and early history deletion are allowed only under the explicit capture-failure/disk-pressure policies above.
- Alerts and their drop/error reporting must not synchronously hold execution capacity. Preserve failure evidence for later diagnosis with bounded observation work.
- Compute expensive diagnostics on demand; short-lived request-triggered caching/coalescing is preferable to always-on dashboard refresh or precomputation.
- Follow existing project structure and APIs. Prefer the smallest measured improvement over a new storage engine, service, abstraction, or concurrency subsystem.

## Evidence and validation

- Primary benchmark: active jobs/workers **with no viewers**, including sparse/bursty/chatty output, many writers, short-job completion, long-lived-worker archival, degraded storage, and retry/finalizer backlog. Keep a no-output idle control; idle results alone do not establish active capture efficiency.
- Secondary benchmark: the same workload with occasional readers, slow readers, and bounded downloads/metrics. Measure their interference with writers and process control, not just API throughput or live latency.
- Track daemon CPU, allocations/GC, retained heap, goroutines/wakeups, fsync count, write bytes, pipe/lock blocking, completion/restart latency, and disk growth. Separate daemon cost from child cost, allocated bytes from retained memory, and tmpfs from real-disk evidence.
- Run comparisons serially with repeated samples on the same toolchain/hardware/filesystem. Use real storage for durability/I/O claims. Do not claim gains from source inspection alone.
- For implementation changes, run relevant tests and normally `go test ./...`, `go test -race ./...`, and `go vet ./...`. Test cancellation, overload, tier migration, retention gaps, reconnects, and final-tail delivery when affected. State exactly what was run; do not present old audit results as new validation.

## Recording trade-offs

For a design/performance decision, record:
1. The workload and zero-viewer cost being addressed.
2. Alternatives, the chosen option, and the simpler option rejected (if any).
3. Resource bounds, overload behavior, lifecycle ownership, and durability/correctness guarantees.
4. The accepted downside—e.g. slower first read or 1–3-second display lag—and why background execution benefits.
5. Measurements/tests, remaining uncertainty, and conditions for revisiting the choice.

Use an ADR for substantial architecture/durability changes and update operational/configuration docs when contracts change. Older live-tail-oriented drafts are not a reason to optimize for constant viewers, but do not silently break public APIs or durability promises.

`astra-audit-code.md` is the audit source; `astra-audit-code.html` is its visual companion. Keep findings, evidence caveats, design decisions, severity counts, and remediation order synchronized. Distinguish severity from workload-based investment order: a viewer-only OOM risk remains serious when exposed even though reader speed is secondary. The audit recommends changes; it does not mean they are implemented.
