# Astra Go code audit

## Scope and verdict

- **Audited revision:** `7198673fa1b37fdac762994769c2ca1920f8bb67`
- **Audit date:** 2026-10-05
- **Scope:** all 24 production Go files in `cmd/` and `internal/`, relevant tests, SQL schemas/queries, and configuration/operational contracts. Frontend implementation and dependency vulnerability analysis are outside this audit.
- **Changes:** documentation only; no production fixes were applied. The original audit's temporary diagnostic tests were removed.
- **Re-review:** background-first design review at the same revision, applying the owner's workload rule: approximately **99.9999% of job/worker execution is unattended**, with logs inspected mainly during setup or troubleshooting. This is a product assumption, not a measured traffic ratio.

**Verdict:** optimize for reliable, low-cost background execution with **zero live viewers**. Worker recovery, scheduling, timeout/shutdown enforcement, bounded pending work, and recoverable logs matter more than live-reader speed. Accept **1–3 seconds of live-log display lag** in exchange for less CPU, allocation, I/O, and complexity. The code also retains a memory tail with no viewers; that always-on cost deserves attention before archive-reader micro-optimizations.

The 17 original findings and their evidence remain; severity is distinct from implementation order. A rare diagnostic reader must still not OOM the daemon, but instant live delivery is not a requirement. Passing tests do not rule out the targeted failure cases below.

### Priority summary

P1 = fix before relying on the affected feature in production; P2 = important correctness/scalability fix; P3 = lower-impact edge case. These are consequence-based severities, not a viewer-weighted task order. A01 is P1 **when live subscriptions are used**; it has no subscriber-queue cost with zero subscribers. See the background-first decisions and revised remediation order below.

| ID | Priority | Finding | Primary impact | Evidence |
|---|---|---|---|---|
| A01 | P1 | Live subscriber buffers bypass log byte quotas | Memory/OOM | Reproduced |
| A02 | P1 | Worker supervision exits before background finalization succeeds | Worker outage | Reproduced |
| A03 | P1 | Root service installer follows user-controlled directory symlinks | Privileged filesystem modification | Safe path-following probe + source |
| A04 | P1 | Shutdown deadlines do not bound several earlier waits | Hung shutdown/reload | Source trace |
| A05 | P1 | Completely corrupt orphan chunks are silently deleted | Loss of recovery evidence/logs | Reproduced |
| A06 | P2 | Archive cursor queries rescan old chunks | CPU/SQLite latency | Query plan + repeated benchmarks |
| A07 | P2 | CLI creates a new Unix HTTP transport for every poll | FDs/goroutines/connection churn | Reproduced |
| A08 | P1 | Multi-fire cron schedules repeat during a DST fold | Duplicate executions | Reproduced |
| A09 | P2 | Catch-up skips the first missed fire and selects an old fire after its safety cap | Missed/wrong executions | Two reproductions |
| A10 | P1 | Execution timeout starts after blocking post-spawn work | Child exceeds runtime limit | Reproduced |
| A11 | P3 | Pipe truncation is absent from the run-level truncation flag | Incorrect observability | Reproduced |
| A12 | P2 | Explicit zero cannot override nonzero job defaults | Unexpected timeout/retries | Reproduced |
| A13 | P2 | Import commits, then reconciles using a canceled request context | Database/runtime disagreement | Source trace |
| A14 | P2 | Expensive non-SSE reads have no admission or work budget | CPU/allocation pressure | Source + repeated benchmarks |
| A15 | P2 | Retry/finalizer/archive backlogs have no global bound | Memory/disk growth | Source trace |
| A16 | P2 | Alert overload synchronously blocks run completion | Lost capacity/storage contention | Source trace |
| A17 | P3 | Archive size conversion can overflow and disable pruning | Disk-budget failure | Reproduced |

## Implementation status

- **Status date:** 2026-10-06
- **Merged revision described:** `57063ba` (`main`, "Merge feat/queue"; merges `fix/exec` 3cdcf48, `fix/logstore` bbd55c3, `fix/config` bd6575f, `fix/cmd` e9e0f96, `fix/sched` fc50f6d, `feat/disk` e815a97, `feat/reads` 3117652, `feat/queue` 57063ba; range `b785adb..57063ba`)
- **How to read this document now:** every finding, probe, benchmark number and code location in the rest of this file was observed at the audited revision `7198673` and is kept unchanged as the original evidence. The statuses below describe the **post-fix tree** (`57063ba`); file/line references in the findings are no longer valid there. Words such as "not implemented", "current behavior" and "documentation only" in the original text refer to `7198673`.
- **Counts:** 17 original findings: **14 fixed, 3 partially fixed (A04, A09, A15), 0 not implemented**. Severity counts (7 P1, 8 P2, 2 P3) are unchanged because they classify the original findings.
- **What was validated:** each stream added regression tests for its findings (listed in `status/<stream>.md`) and reported `gofmt`, `go vet`, `go build`, `go test ./...` and `go test -race` on the packages it touched, plus independent code reviews of the logstore, exec, disk, reads and queue streams. These are per-branch results; the final merged-tree commands for this documentation pass are in `status/docs.md`. The few measurements taken are listed after the tables with their caveats; everything else is test coverage, not a performance claim.

### Per-finding status

| ID | Status | Where | Residual gaps (precise) |
|---|---|---|---|
| A01 | Fixed | `0c8ce4b` (`fix/logstore`) | Payload subscriptions, history and queues are removed; SSE reads stored frames and polls every 2 s per connection. The bundled web UI does not render the new `gap` event and the embedded bundle in `internal/api/assets` was **not rebuilt** (no `node_modules` offline): run `make build-web` before release. Each SSE poll re-decodes the active chunk under the shared run lock (accepted downside, bounded by the 64-stream cap and 1 MiB pages). |
| A02 | Fixed | `af215ca` (`fix/exec`), `203d27a` (workers always use the background finalizer) | Supervision waits (backoff 500 ms→30 s) until the run is terminal. If the run is still nonterminal and no finalizer owns it (only possible at shutdown) the lifetime is treated as failed. |
| A03 | Fixed (per-user install) | `b04f80a` (`fix/cmd`) | Descriptor-relative `openat`/`O_NOFOLLOW` for `--user` installs. The root-only ownership branches (existing directory owned by another uid) are **untested as non-root**. `checkPortConflicts`/`installedDaemonPorts` still read unit-referenced configs with path-based `os.ReadFile` (read-only, returns a port number). Home directories with group/other-writable modes are accepted. The root daemon path (`/etc/minicrond`) is unchanged (root-controlled). A swapped-in empty root-owned 0700 directory during the mkdir→open window of a newly created directory could theoretically be chowned; owner/mode/empty checks mitigate it. |
| A04 | **Partially fixed** | `e94ede3`, `ec59f91`, `af215ca`, `a8b9edb`, `0c8ce4b`, `c659a0b`, `a60e678`, `0e73398`, `a4261b7` | Items 1–5 are addressed: maintenance gets a context and is joined last; `reloadMu` is context-aware; executor admission is a context-aware semaphore with `BeginShutdown`; supervisor join is bounded; `StopArchiver` cancels and joins workers. **Residuals:** (a) `LogDB.usedBytes` is a read on the writer connection that still relies on SQLite's 5 s busy handler (all log DB *writes* are context-sliced); (b) inline archival in `Store.Close`/`finalize` uses `context.Background()`; (c) an fsync already in progress cannot be interrupted: `StopArchiver` abandons the final group sync at its deadline; (d) the 45 s shutdown budget is a target, not a guarantee, and if the maintenance join times out (10 s) the daemon deliberately leaves the databases open instead of closing them under a running sweep; (e) executor runs that outlive the forced-kill join (3 s) can still touch the store afterwards (errors only). |
| A05 | Fixed | `0c8ce4b`, `8e052ee` | Undecodable nonempty chunks are copied to `.quarantine/<run>-<chunk>.zst.corrupt` and removed from the buffer at once; valid siblings are archived; torn tails are still salvaged. If the quarantine move itself fails the chunk stays and the older three-sweep whole-buffer quarantine applies. |
| A06 | Fixed | `600bd6e` (`feat/disk`) | Sequence-oriented seek on `idx_log_chunks_seq`; relies on chunk numbers and last sequences growing together within a run (true for the writer). Benchmarks: see measurements. |
| A07 | Fixed | `ba7d11a` (`fix/cmd`), `6a07f51` (503 `read_busy`/`read_timeout` retry) | One client/transport per command; `logs -f` polls every 2 s once caught up. |
| A08 | Fixed | `8e50e1c` (`fix/sched`) | Any candidate in the second copy of a backward transition is suppressed, including for a job whose anchor lies inside that copy (it waits for the next valid time). |
| A09 | **Partially fixed** | `8e50e1c` | The first overdue occurrence is recovered and `latest` is the exact latest occurrence beyond 10,000 ticks. **Residual:** under `catch_up=none` the missed count is enumerated only up to 100,000; beyond that it is a lower bound and is **only warn-logged** (`missed occurrence count is incomplete`), with no "incomplete" flag in the run model or API. |
| A10 | Fixed | `ec59f91` | Timeout armed right after `cmd.Start`; pumps start first; `StartRun` is bounded at 10 s and runs off the select loop. |
| A11 | Fixed | `0c8ce4b` | — |
| A12 | Fixed | `695cee4` (`fix/config`) | Presence-aware only for `timeout` and `retries` (the audited fields). `GET /api/v1/jobs` (list) still returns plain definitions, so an explicit zero is omitted there; the detail endpoint and export spell it out. |
| A13 | Fixed (import) | `d5c38bc` | Import reconciles on a caller-independent context with 4 bounded attempts; persistent failure still returns 500 and the import stays committed. `putJob`/`deleteJob`/`enable` keep their single `WithoutCancel` reconcile (no retry); there is no periodic reconciliation. |
| A14 | Fixed | `1de8fb2`, `3aa6811`, `7d40147`, `1845e1f`, `6a07f51` (`feat/reads`) | Shared admission (`[reads]`: 4 slots, 32 MiB estimated working set, 20 s work timeout), 503 `read_busy`/`read_timeout`, downloads capped at `max(1, slots-1)`, 1 MiB JSON pages, coalesced metrics (3 s TTL). Per-request memory costs are fixed estimates, not measurements. The coalescing benchmark shows cache hits versus recomputation, not a cheaper computation. The audit's mixed-load test (peak heap, DB waits, run-admission latency under concurrent reads and writers) was **not run**. The web `copyError` change is in an unrebuilt bundle (see A01). |
| A15 | **Partially fixed** | `786bb99`, `45f3a67`, `3c5fc6d`, `203d27a`, `0c57549` (`feat/queue`, `feat/disk`) | One bounded retry scheduler (`max_pending_retries`), bounded finalizer map, capped archiver id queue with sweep rediscovery, counts exported in `GET /api/v1/daemon` diagnostics. **Residuals:** pending retries are still **in memory** and lost on restart; finalizer caps (256 soft, 4× hard, 2 min inline hold) and the archiver cap (4096) are constants, not configuration; past the hard cap a job's finalizer keeps holding its capacity slot (backpressure, nothing discarded); the audit's "disk-pressure admission controls" are **not implemented** (admission is not wired to `DiskPressure()`). |
| A16 | Fixed | `029585c` (`fix/exec`) | Drops go to a bounded newest-wins ring (256) recorded off the completion path; drop recording at shutdown is best effort. |
| A17 | Fixed | `d3c11f5` (`fix/config`) | `logs.db_max_size` is limited to `1<<30` MiB; byte conversion saturates. |
| D01 | Implemented | `0c8ce4b` | No viewer-only payload history; no read-time cache was added (not justified). |
| D02 | Implemented (see 1B) | `0c8ce4b`, `6c92cbf` | The 15-minute worker archive checkpoint is unchanged. |
| D03 | Implemented | `0c8ce4b`, `1de8fb2` | Stored-cursor reads, per-connection poll timer, `gap` event on retention loss, final tail before `done`; bounded read admission (A14). Web UI `gap` rendering: see A01. |

### ADR-8 choices

| Choice | Status | Where | Residual gaps (precise) |
|---|---|---|---|
| 1B batched fsync | Fixed | `0c8ce4b`, `6c92cbf`, `aedaf75`, `c659a0b` | Defaults `logs.sync_interval = 2000` ms (100–60000) and `logs.sync_max_dirty = 1024` KiB (1–65536); one shared timer armed only while a writer is dirty; final sync on EOF/seal/`StopArchiver`; `logs.durability = "frame"` stays strict. **No power-loss test was run**; the OS-crash window is nominal and stalls can extend it. A degraded writer recovers only when a later line arrives or at close (no timers by design). |
| 2A continue on capture failure | Fixed | `0c8ce4b`, `4ce9b7e`, `ebc9173`, `1b13977` | Pipe write/sync failures degrade, discard and later recover with a summary line; `Seal`/pump errors keep the real run status and set `log_truncated`. A pump *read* error while the child still runs still stops that child (it can no longer be drained). The public `Write` (system lines) keeps sticky errors. A capture failure consumes one sequence number and can show as a `gap`. |
| 3A disk budget before retention age | Fixed | `b7027c1`, `ef06454`, `683a6f9` (`feat/disk`), ADR-10 | `logs.disk_min_free` = 512 MiB **on by default** (clamped to a quarter of the filesystem; explicit `0` disables), `logs.disk_budget` off by default, quarantine kept forever unless `quarantine_keep_for`/`quarantine_max_size` are set. **Residuals:** admission is **not** wired to `DiskPressure()` (only log output is dropped under 2A, so "new queued work follows 4B" is not implemented); reclamation latency on a large archive over slow storage is unmeasured; pressure caused by other data on the same filesystem also deletes logs; a pruned run keeps its run record and reads empty without a flag. |
| 4B bounded durable queue | Fixed | `d602d7c`, `786bb99`, `5530bc1` (`feat/queue`), ADR-9 | Defaults `[queue]` `max_items` 100, `max_per_job` 25, `max_bytes` 256 KiB (accounted ceiling, not real bytes), `max_age` 900 s, `drain_rate` 5/s, `max_pending_retries` 500; restart required. Expiry/rejection are `skipped` rows with an `end_reason`, not a new status. **Residuals:** pending retries are in memory (see A15); queue/rejection counters are process-lifetime (the `skipped` rows are the durable evidence); a crash between dequeue and spawn leaves an `interrupted` run (never replayed); queue/finalizer idle cost is structural, not benchmarked. |
| 5A small-server-first | **Partially fixed** | ADR-9/10/11 defaults | The 1 CPU / 512 MiB, 10 workers, four jobs profile was used only to choose conservative defaults. **No benchmark or profile of that fixture has been run**; it is not a capacity guarantee or a runtime limit. |

### Additional capacity observations

1. `log_max` is not a total disk quota — **addressed** by the disk budget (3A); `disk_budget` is off by default, `disk_min_free` on.
2. Metadata file size does not shrink; soft-deleted definitions never purged — **not addressed**.
3. Quarantine has no budget — **addressed** as an opt-in purge policy (default keeps forever).
4. Read/archive pages can delay a run's log writes — **partially**: writer-lock waits and maintenance durations are exported; lock ownership was not shortened and archive-induced writer blocking was not measured.
5. Metadata reads on the writer connection — **partially**: API-facing reads and retention selection moved to the read pool; state-feeding paths (executor, supervisor, scheduler) still use the writer connection.
6. Physical storage testing — **partially**: sparse-output fsync count was measured on one ext4 virtual disk (below); no power-loss test, no SSD/HDD characterization.
7. Workers outside the job concurrency budget — **unchanged by design**.

### Measurements taken during implementation

All on Go 1.27.0, 4 vCPU, KVM guest; real ext4 disk (`/dev/sda1`, `TMPDIR` off tmpfs) for the first two; serial runs, medians of repeated samples. They are not production data, not power-loss tests, and not a whole-daemon CPU/heap profile.

- **A06** (warm page cache, 1-byte blobs, so index traversal only): final page at 10,000 chunks 859 µs → 44 µs; full pagination at 100,000 chunks 58.6 s → 1.24 s (raw files in `docs/benchmarks/2026-10-05-a06-archive-cursor/`).
- **1B** (`BenchmarkSparsePipe/batch`, 200 lines 1 ms apart, no viewer): 1.000 → 0.005 fsyncs/line; strict mode unchanged at 1.000. `Write` microbenchmarks dropped from ~1.8 ms to ~8 µs per call mostly because `Write` no longer fsyncs per call in batch mode, so they are not the same guarantee.
- **A14** (tmpfs, CPU/allocation only): `RunMetrics` per-computation cost is at parity (about 31–33 ms and 7 MB at 30,000 rows); `BenchmarkMetricsEndpointParallel` shows coalesced requests at about 61–69 µs versus about 1.4 ms uncoalesced, i.e. the effect of coalescing and TTL cache hits.
- Not measured: the 5A small-server profile, active zero-viewer capture CPU/heap/GC at daemon level, queue/retry/finalizer cost, disk-pass reclamation latency, `BenchmarkWorkerLiveSSE`, `BenchmarkActiveWriters64` (prints `NaN`; pre-existing).

## Background-first design decisions

### Decision rule

Choose correctness/security and recoverable history first, then bounded resources and low unattended execution cost, then simplicity. Spend resources on diagnostics **when requested**, not continuously for a hypothetical viewer. Prefer a slower reader to a slower child process. Display-lag tolerance alone does **not** relax process control, worker restart correctness, or capture durability. The owner subsequently approved the separate durability, overload, and retention trade-offs below; those are deliberate policies, not incidental consequences of a slower viewer.

| Area | Decision under the new rule | Trade-off accepted |
|---|---|---|
| Zero-viewer capture | Avoid viewer-only payload copies, tail retention, fan-out, and wakeups; keep capture and recovery independent of subscriptions | First inspection may read/decompress stored chunks |
| Following logs | Prefer bounded cursor reads every 1–3 seconds while following; SSE may remain as a batched transport, not a per-frame requirement | Live display is delayed; readers may be throttled or disconnected with resume information |
| Background capacity | Bound retry/finalizer/archive backlogs; durably queue excess execution work within count/byte/age limits | Late execution and extra persistence; reject when full or unable to persist |
| Durability | Write accepted logs through to the OS/file; batch fsync on a roughly 1–3-second cadence with a byte limit | Recent unsynced output may be lost on OS crash/power failure; stalls can extend the window |
| Capture failure | Keep running and drain pipes; discard output that cannot be stored and report gaps when possible | Incomplete diagnostics rather than stopping useful work |
| Log disk pressure | Prune oldest eligible logs before normal retention expiry | Shorter history to protect disk capacity |
| Metrics | Compute on demand; optionally coalesce/cache for a short interval after a request | Stale diagnostic snapshots are fine; no eager refresh with no readers |
| Architecture | Keep the file-first capture and asynchronous archive baseline; optimize the measured write path before adding a new storage engine | No live-view-driven storage rewrite or extra always-on service |

### Owner-approved choices — 1B, 2A, 3A, 4B, 5A

**Accepted direction; implemented since this section was written (see [Implementation status](#implementation-status)).** The owner selected batched log fsync (1B), continued execution despite log-storage failure (2A), disk budget ahead of retention age (3A), a bounded durable execution queue rather than immediate skipping (4B), and small-server-first tuning (5A). See [ADR-8](docs/adr/0008-background-first-trade-offs.md) for the decision record.

- **Durability boundary:** preserve daemon-only crash recovery; do not keep accepted frames solely in RAM/compression buffers. The accepted OS/power-loss window is nominal, not a hard guarantee under stalled I/O. Metadata/queue SQLite durability and archive commit-before-unlink ordering are not weakened by this choice.
- **Failure boundary:** continue draining stdout/stderr even when output must be discarded; report missing output when possible through a bounded path. Timeouts, cancellation, and required execution-state persistence still apply. Quarantine needs an explicit separate purge policy, not silent deletion as ordinary history.
- **Queue boundary:** persist before acknowledging durable enqueue; reject explicitly if full or unable to persist, expose expiry, and drain at a bounded rate after recovery. Define crash/replay and definition-change behavior before coding; no exactly-once promise or blind replay of possibly started commands. Preserve overlap/retry/catch-up policies.
- **Tuning target:** roughly 1 CPU / 512 MiB, 10 workers, four concurrent jobs, and modest output as a benchmark profile, not guaranteed capacity or fixed runtime limits. Child resources are additional. Prefer conservative caches, small queues, and limited archive concurrency.
- **Still to specify:** exact sync interval/byte threshold, disk budget/watermarks and eligible data, queue count/byte/age limits and expiry/replay outcomes, and measured log-volume fixtures. The earlier 5 GiB example is not an approved default. Update runtime/configuration/operations contracts and tests with implementation, not ahead of it. *(Resolved by ADR-8's implementation status and ADR-9/10/11; the chosen defaults are listed in the ADR-8 table above.)*

### D01 — Remove speculative live-tail work from the zero-viewer path

**Design opportunity · Source-confirmed cost, not a measured speedup.** In `internal/logstore/logstore.go:771-781`, every successful frame attempts `reserveTail` and clones its payload when history has room, even with `len(w.subs)==0`. History retains up to 5,000 frames / 16 MiB per writer, within a 64 MiB store-wide accounting limit. This is bounded, not A01's unaccounted subscriber queue. An idle-daemon benchmark with no output does not exercise it.

The read path at `:1133-1287` already checks the archive and chunk files before the memory snapshot, and writes flush compressed data to the active file before returning. **Preferred target: no viewer-only payload history by default.** Read from stored chunks on demand; add a small, byte-bounded cache only during actual reads if measurements justify it, and release it after use. Verify active-chunk decoding, tier migration, retention gaps, final-tail delivery, and reconnect cursors before removing history; it is not a one-line deletion of `Snapshot`.

### D02 — Batch delivery; treat capture durability as a separate decision

**Design review · Physical-storage measurements still required.** `Writer.write` flushes the zstd encoder per frame (`internal/logstore/logstore.go:743-759`); `Pipe` calls `Sync` before waiting for more input (`:860-875`). Thus sparse logging may pay an fsync per line with no viewer, even in batch mode. `Write` itself is a durability boundary. These are current durability choices, not evidence that SSE requires per-line disk I/O.

The owner has now explicitly approved **batched log fsync (1B)**, separately from viewer latency. Keep accepted frames written through to the OS/file, including the metadata ordering needed for daemon-crash recovery; do not simply defer zstd flush with accepted bytes still in process memory. Group fsync on a roughly 1–3-second cadence with a dirty-byte limit and final sync on EOF/seal/orderly shutdown. Accept possible loss of recent unsynced output after OS crash/power failure; storage stalls can extend the nominal window. Ensure a dirty sparse writer can sync without another line arriving, and avoid periodic timers on idle writers.

Measure compression, sync count, write bytes, and pipe-block time on real storage under sparse and bursty output. Implement the new log durability contract and recovery tests together, preserving explicit strict mode and metadata/queue SQLite durability. Under write/sync failure, apply 2A: keep draining and running, expose degraded capture when possible, and do not report successful durable capture falsely. These are approved targets, not current behavior or benchmark-proven gains.

The 15-minute worker archive checkpoint is tier migration, not live-display freshness. Do not shorten it to 1–3 seconds for viewers: reads already merge the file buffer with the archive. Adjust archive cadence only for measured buffer pressure, throughput, and recovery needs.

### D03 — Make rare readers safe, not fast at the writer's expense

Prefer on-demand, byte-bounded pages using stable sequence cursors. Polling is the simpler default recommendation; keeping the existing SSE API is fine if it batches delivery at a 1–3-second cadence and avoids payload queues. If notifications are retained, coalesce sequence/dirty hints rather than cloning every frame. Create viewer timers/state only while following and tear them down on disconnect; preserve SSE event/resume contracts if changing its internals.

Readers must not require the producer to wait for them. Bound read admission, page bytes, and lock occupancy; throttle or reject diagnostic work under pressure instead of increasing producer buffers or reader concurrency. A cursor falling behind retention must expose a gap, not silently skip data; a completed run must drain its final stored frames before `done`. Do not trade correctness for simpler polling. Archive cursor improvements (A06) still help large postmortem downloads, but millisecond response-time gains are not the primary objective.

## Validation and measurements

### Re-review evidence boundary

*Original text, revision `7198673`; see Implementation status for the post-fix tree.* The re-review checked the current capture/read/archive paths, SSE handler, worker finalization, maintenance cadence, durability tests, and benchmark fixtures against the unchanged revision. The design recommendations above are **not implemented or benchmark-proven**. All test/probe/benchmark outcomes below are retained from the original audit, not new runs. In particular, the original idle result does not establish the cost of active logging with zero viewers.

### Original baseline checks

The original audit reported all passed:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Some unchanged package results in the ordinary test run were cached. The race run passed across all packages. Targeted diagnostic probes were run with `-count=1` and repeated under `-race` without race reports; they logged observed behavior rather than treating the known defects as test failures.

Environment: Go `1.27.0`, Linux/amd64, four visible CPUs, AMD Ryzen AI 9 HX 370 under KVM. Temporary databases/files were on **tmpfs**. Therefore these measurements are not physical-disk throughput or fsync benchmarks. No production workload, long-duration soak, power-loss test, or production heap/CPU profile was collected.

### Repeated benchmark results

Benchmarks were run serially, not concurrently. The table reports observed medians and ranges, not statistically established optimization gains. There was no before/after implementation change.

| Workload | Repetitions | Result |
|---|---:|---|
| Idle daemon, 100 annual jobs, 31-second observation | 3 | **0.040–0.073 CPU ms/s**, zero WAL growth in every observation; post-GC Go heap 0.935–0.960 MiB |
| Metrics: 30,000 old rows + 500 in-window rows | 5 | Median **0.564 ms/request**, about 94.9 KB allocated/request |
| Metrics: 30,000 in-window rows | 5 | Median **30.92 ms/request**, range 30.59–36.18 ms; about **6.99 MB and 360,114 allocations/request** |
| Read final archive chunk, 100 chunks | 5 | Median **24.98 µs** |
| Read final archive chunk, 1,000 chunks | 5 | Median **120.92 µs** |
| Read final archive chunk, 10,000 chunks | 5 | Median **860.57 µs** |
| Archived SSE-style backlog, 5,000 × 4 KiB payloads | 5 | Median **10.74 ms**, approximately **27.3 MB allocated** across the entire transfer |
| Raw download of the same archived fixture | 5 | Median **9.37 ms**, approximately **27.1 MB allocated** across the entire transfer |

Allocation totals are **not retained heap or RSS**. The idle benchmark's RSS counter is a process high-water mark, so it is not used to infer per-daemon resident memory. Archive cursor scaling was tested with tiny blobs to isolate query work; actual large BLOB storage can have additional costs.

Reproducible existing benchmark commands:

```sh
go test ./internal/daemon -run '^$' \
  -bench 'BenchmarkDaemonIdle/jobs_100$' -benchtime=1x -count=3

go test ./internal/store -run '^$' \
  -bench 'BenchmarkRunMetrics(History|Window)$' \
  -benchmem -benchtime=200ms -count=5

go test ./internal/logstore -run '^$' \
  -bench 'BenchmarkArchived(StreamBacklog|RawDownload)$' \
  -benchmem -benchtime=200ms -count=5
```

The temporary archive-cursor benchmark inserted 100/1,000/10,000 chunks for one run, with `Number = First = Last = i+1` and one-byte blobs. It repeatedly called `EachChunk(ctx, runID, n-1, callbackReturningFalse)` using `-benchmem -benchtime=200ms -count=5`.

## Findings

### A01 — Live subscriber queues bypass all byte quotas

**P1 · Memory · Reproduced**

**Locations:** `internal/logstore/logstore.go:771-786,927-945`; `internal/api/api.go:1070-1176`.

`Subscribe` allocates a channel for **256 frames**, irrespective of payload size. `write` clones payloads when subscribers exist and sends them into those channels. These references are not charged against the store's 64 MiB tail quota, the writer's 16 MiB history quota, or `log_max`.

The SSE handler subscribes **before** reading the backlog and does not consume live frames until backlog delivery finishes. A client need not stop reading completely to accumulate a full live queue: a large backlog is enough.

**Observed probe:** with `MaxLine=256 KiB`, `MaxBytes=1 MiB`, one non-consuming subscriber, and 256 writes:

```text
subscriber_queued_frames=256
subscriber payload bytes=67,108,864
hot buffer bytes=262,168
store tail bytes=262,168
```

That is **64 MiB of queued payload for one subscriber at the default maximum line size**, despite a 1 MiB hot-buffer budget. At the permitted 16 MiB line size, the theoretical queue payload reaches **4 GiB**. The 64-SSE limit does not make this safe. Subscribers of the same writer can share a payload allocation, so do not multiply identical shared frames blindly; independent runs and differently lagging readers still retain substantial independent data.

**Background relevance:** absent with zero subscribers; nevertheless a single rare troubleshooting session can threaten background execution. Keep the P1 safety classification, but do not put live-view performance ahead of unattended reliability. The always-on history cost is separate (D01).

**Fix:** prefer removing payload subscriptions in favor of bounded cursor reads at a 1–3-second display cadence (D03). If retaining notifications, coalesce sequence hints; if retaining payload queues temporarily, enforce per-subscriber and global retained-byte limits. Never increase buffers to hide viewer lag or make writers wait for readers. Detect overflow during backlog as well as live delivery, and preserve reconnect/gap semantics.

**Regression test:** establish the zero-subscriber baseline, then attach slow/backlogged readers with maximum-sized frames. Assert bounded retained bytes, no subscriber-induced write blocking, and resumable overflow/gap reporting; verify final frames arrive before completion.

### A02 — A temporary terminal-write failure permanently stops an `always` worker

**P1 · Correctness/availability · Reproduced**

**Locations:** `internal/executor/executor.go:252-266,453-517`; `internal/supervisor/supervisor.go:177-235`.

When inline terminal persistence fails, the executor launches `finalizeLater`, removes the run from `active`, and closes its completion channel. The supervisor then calls `terminalRun`. That function retries query **errors**, but returns a successfully read row even if it is still `running`. The caller sees a nonterminal status and exits the supervision loop.

Later successful background finalization sends notifications but does not recreate worker supervision.

**Observed probe:** a temporary SQLite trigger rejected updates to `status='succeeded'`, while other reads/writes remained operational. An `always` worker executed `true`:

```text
supervisor_exited=true persisted_status=running
# remove the injected failure; background finalization succeeds
runs=1 latest_status=succeeded supervision_loop=false restart_policy=always
```

The worker remains down until an external start/reload, without exhausting the failed-start budget.

**Fix:** distinguish process cleanup from terminal-state completion. Keep supervision waiting, with cancellation/backoff, until the state becomes terminal or an explicit finalization failure policy applies. Do not treat a nonterminal read as a reason to silently abandon supervision.

**Regression test:** fail only terminal updates, allow reads, restore writes after inline retries, and assert a worker with `restart=always` starts its next lifetime.

### A03 — Root service installation follows user-controlled symlinks and then chowns their targets

**P1 · Privileged filesystem correctness/security · Source + safe probe**

**Locations:** `cmd/minicrond/service.go:212-247`.

`service install --user NAME` runs as root and derives configuration paths beneath the target user's home. `writeServiceConfig` uses `os.Stat`, `os.MkdirAll`, `os.WriteFile`, and `os.Chown` without restricting symlinks or safely traversing user-owned ancestors.

For example, if a user prepares `~/.minicrond` as a symlink to another existing directory, and `config.toml` does not already exist there, the installer writes through that symlink. It then calls `os.Chown` on the directory path, which **follows the symlink and changes ownership of its target**. A privileged directory can consequently be transferred to the target user.

A safe temporary-directory probe confirmed that `writeServiceConfig` writes through such a directory symlink. No privileged target was modified during the audit; the root ownership consequence follows from the production call path and `os.Chown` semantics.

**Fix:** use descriptor-relative, no-symlink filesystem operations and validate ownership/type while walking user-controlled paths. A standalone `Lstat` check is insufficient against concurrent replacement. Consider root-owned configuration locations for root-run provisioning.

**Regression test:** reject symlinked configuration directories and files, including replacement races, without creating files or changing target ownership outside the intended directory.

### A04 — Shutdown/reload can wait outside the advertised timeout budgets

**P1 · Availability/resource lifecycle · Source trace**

**Locations:**

- `internal/daemon/daemon.go:207-233,286-289,300-350,489-507`
- `internal/supervisor/supervisor.go:78-110,130-135,358-375`
- `internal/executor/executor.go:981-1034`
- `internal/logstore/logstore.go:310-333,425-478`
- `internal/logstore/archive.go:31-85,124-142`

Several waits are not governed by the shutdown context:

1. The last registered daemon defer cancels maintenance and calls `maintenance.Wait()` **before** the main shutdown defer runs. Maintenance can be inside `FlushActive`, `ArchiveOrphans`, or log deletion, all of which perform work without the maintenance context.
2. Shutdown takes `reloadMu` before starting the executor's 15-second budget. A reload holding that lock can be waiting for an old worker's potentially very long configured grace period.
3. `executor.Shutdown` takes the admission mutex before checking its context. Admission includes storage/log setup.
4. After executor shutdown times out, `super.Shutdown()` still joins its loops without a deadline; those loops can still be joining stuck execution cleanup.
5. `StopArchiver` handles context expiration by setting an abort flag and then unconditionally waiting on `<-done`. In-progress database/lock/file operations are not canceled by that flag.

SQLite's five-second busy timeout does **not** bound Go mutex waits, database-pool queueing, or an entire many-run sweep. This is not a claim that ordinary shutdown always hangs; storage stalls, a large recovery backlog, or a long-grace worker being reloaded expose it.

**Fix:** propagate lifecycle contexts through maintenance/archive APIs; stop admission and signal worker shutdown before joining reload work; apply an overall shutdown budget. Ensure operations can actually stop before closing their databases—simply timing out the caller and abandoning resource-owning goroutines is unsafe.

**Regression tests:** hold an archive transaction, cancel during a large sweep, and race shutdown with replacement of a worker having a long grace period. Verify both bounded shutdown and absence of post-close database access.

### A05 — A completely corrupt orphan chunk is treated as successfully recovered

**P1 · Data preservation · Reproduced**

**Locations:** `internal/logstore/logstore.go:1054-1068`; `internal/logstore/archive.go:162-193`; quarantine handling at `internal/logstore/logstore.go:479-515`.

`salvageFrames` returns only a frame slice. Decoder failure and an actually empty stream both produce zero frames. `archiveOrphan` skips a file with no recovered frames, then returns `os.RemoveAll(dir)` without recording a recovery error.

**Observed probe:** a nonempty invalid `000001.zst` in an inactive run directory, with no index:

```text
archive_error=<nil>
original_deleted=true
archive_runs=0 chunks=0
```

The quarantine policy never gets a chance to act because the operation reports success. Salvaging a valid prefix from a torn final chunk is intentional; **silently deleting an entirely undecodable nonempty chunk** is the defect identified here.

**Fix:** return recovery status/error alongside recovered frames. Preserve or quarantine nonempty undecodable input, and report partial corruption explicitly. Differentiate a legitimate empty chunk from unsupported/corrupt data.

**Regression test:** malformed zstd data and unsupported frame versions must not disappear with a nil archive error; retain tests that successfully salvage a valid prefix.

### A06 — Archive pagination repeatedly scans already-consumed chunks

**P2 · CPU/database bottleneck · Measured**

**Location:** `internal/logdb/logdb.go:201-224`; caller resets through `internal/logstore/logstore.go:1122-1123,1226-1240`.

Every `EachChunk` invocation starts `lastNumber=-1` and runs:

```sql
SELECT number,first_seq,last_seq,raw_bytes,blob
FROM log_chunks
WHERE run_id=? AND last_seq>? AND number>?
ORDER BY number LIMIT 1;
```

The observed SQLite plan was:

```text
SEARCH log_chunks USING INDEX sqlite_autoindex_log_chunks_1 (run_id=? AND number>?)
```

The engine walks the `(run_id,number)` index and filters `last_seq`, rather than seeking directly to the frame cursor. A new log page resets the number cursor, so old chunks are revisited. The sequence index exists but does not solve this ordering/selection combination in the observed plan.

The final-chunk microbenchmark increased from **25 µs at 100 chunks** to **861 µs at 10,000 chunks** despite returning the same one-byte result. Reading a long run page by page can approach quadratic cumulative index traversal when each page consumes only a few chunks. Repeated empty tail polls also pay for historical chunks.

**Background relevance:** this is reader-triggered work, not a continuous zero-viewer cost. Fix it to bound interference and support large postmortem retrieval, after unattended correctness/capacity work; do not add a continuously maintained reader cache for subsecond tail latency.

**Fix:** locate the first matching chunk using a sequence-oriented seek, then continue by chunk number; or retain an appropriately invalidatable chunk cursor in `StreamReader` only while reading. Verify query plans on realistic data rather than merely adding another index.

**Regression benchmark:** final-page and empty-tail reads at 100, 1,000, 10,000, and 100,000 chunks, plus full pagination across the same histories.

### A07 — `logs -f` builds and abandons an HTTP connection pool on each request

**P2 · FDs/goroutines/CPU · Reproduced**

**Locations:** `cmd/minicrond/main.go:201-246,321-346,375-384`.

`requestJSON` calls `client()` for every request. In Unix-socket mode, `client()` creates a fresh `http.Transport`. The response body is closed, but the transport's idle connections are not explicitly closed or reused. Its `IdleConnTimeout` is also unset.

**Observed local Unix-server probe:** 100 small requests followed by a GC produced:

```text
active_connections=100 goroutine_delta=300 fd_delta=200
```

Both client and server ran in that test process, explaining two FDs and approximately three goroutines per connection. This demonstrates connection accumulation, not 200 client-only FDs.

The real daemon has a **60-second server idle timeout**, so a quiet follow loop is not an infinite connection leak against this server: two polls/second can keep roughly 120 idle connections outstanding per CLI, while rapid backlog pagination can create a much larger transient spike. A server without idle expiry retains them longer. TCP mode uses the shared default transport and is not affected in the same way.

**Fix:** create one client/transport for the command and reuse it for every page/poll; close its idle connections on command exit. Prefer simple bounded polling at a 1–3-second cadence once caught up; no polling when follow is not requested. Do not switch to SSE merely for lower latency, especially while A01 remains. A slower poll interval reduces churn but does not replace the transport-reuse fix.

**Regression test:** 1,000 Unix requests should reuse a small bounded connection set; check both accepted connections and goroutine/FD counts after cleanup.

### A08 — DST-fold suppression only works when the immediately previous minute matches

**P1 · Scheduling correctness · Reproduced**

**Locations:** `internal/scheduler/scheduler.go:352-360,388-394,410-414`.

Fold suppression compares the candidate's wall minute only with the immediately preceding fire/input. A schedule firing several times in the repeated hour advances from the last first-hour fire to an earlier wall minute in the second hour. That wall minute differs from the last fire, so it is accepted even though it already fired earlier that day.

**Observed example:** `*/15 1 * * *`, `America/New_York`, 2026-11-01:

```text
last = 01:45 EDT (05:45 UTC)
next = 01:00 EST (06:00 UTC)
```

01:00 already occurred in the first copy of the hour. This violates the documented first-occurrence-only policy and can duplicate non-idempotent jobs.

**Fix:** detect candidates inside the second copy of a backward-transition interval, rather than comparing only adjacent wall minutes. Preserve this behavior through reload/recovery.

**Regression tests:** `* * * * *`, `*/15 1 * * *`, and a one-fire-per-day schedule across both one-hour and non-hour folds.

### A09 — Catch-up has two incorrect boundary behaviors

**P2 · Scheduling correctness · Reproduced**

**Locations:** `internal/scheduler/scheduler.go:154-187,195-251,254-256`.

**First overdue occurrence:** catch-up runs only if `last` is nonzero. When a new schedule has persisted its anchor and next fire but the daemon stops before its first fire, recovery ignores the overdue `persistedNext`. A probe with `@every 1h`, a three-hour-old anchor, an overdue next fire, `last=zero`, and `catch_up=latest` created **zero catch-up runs** and waited for a future occurrence. The stored pending fire establishes that this is not time before the job existed.

**More than 10,000 cron occurrences:** enumeration stops after 10,000 ticks and uses that early tick as `latest`. It then calculates the next normal fire from current time, bypassing the remaining missed interval. A probe with an every-minute cron and a last fire 10,020 minutes ago executed a catch-up fire approximately **20 minutes old**, not the most recent missed minute. Under `catch_up=none`, the reported missed count is likewise capped rather than complete.

**Fix:** use persisted pending state/anchor to recover the first missed occurrence. Separate a CPU-safety enumeration bound from the semantics of `latest`; compute the actual latest occurrence or explicitly surface incomplete recovery instead of silently advancing to current time.

**Regression tests:** restart before the first scheduled fire, and downtime exceeding 10,000 cron ticks under both policies.

### A10 — Runtime timeout is armed only after a child has already encountered blocking setup work

**P1 · Resource enforcement · Reproduced**

**Locations:** `internal/executor/executor.go:305-363`.

After `cmd.Start`, execution performs a `StartRun(context.Background())` database write and a synchronous system-log write before creating the runtime timer and entering the stop/timeout select loop.

A running process is therefore outside its timeout enforcement while these operations block. It can also fill stdout/stderr pipes because the pumps have not yet started. The captured `started` timestamp does not fix this: the timer still receives the full configured duration afterward.

**Observed probe:** a FIFO synchronized command setup so a second database connection could hold the metadata write transaction before the child started. With `timeout=1`, the child was still alive after **1.3 seconds** of blocked `StartRun`. Releasing the transaction let the full one-second timer start; total observed elapsed time was about **2.33 seconds**.

**Fix:** establish the absolute runtime deadline at successful process start. Ensure cancellation/termination can act independently of metadata persistence and log I/O; use bounded contexts and start pipe draining promptly.

**Regression test:** hold metadata persistence and the log sink after spawn; a child must still be terminated according to timeout plus its explicitly configured grace period.

### A11 — Oversized pipe lines do not set `Run.LogTruncated`

**P3 · Observability · Reproduced**

**Locations:** `internal/logstore/logstore.go:710-714,860-913,998`; terminal stats read at `internal/executor/executor.go:420-426`.

`Pipe` truncates its line buffer and supplies `FlagTruncated`, but `write` sets `w.truncated` only if it performs its own length truncation or hits the file-buffer budget. The payload is already at the limit when `Pipe` calls it.

Probe with `MaxLine=4` and `123456789\n`:

```text
payload="1234" frame_truncated=true run_truncated=false
```

Consequently the persisted run and final index can claim no truncation despite lost output.

**Fix:** propagate an incoming `FlagTruncated` into the writer's aggregate flag.

**Regression test:** assert the frame flag, writer stats, final index, and persisted run all report truncation after a pipe-level oversized line.

### A12 — Explicit `timeout=0` and `retries=0` cannot override defaults

**P2 · Configuration correctness · Reproduced**

**Locations:** `internal/config/config.go:258-281`; numeric representation in `internal/model/model.go:19-66`.

Numeric fields cannot distinguish omission from an explicitly supplied zero. `cmp.Or(d.Timeout, defaults.Timeout)` and the equivalent retry merge interpret both as inheritance.

An imported job explicitly setting `timeout=0` and `retries=0`, with daemon defaults of 60 seconds and three retries, was normalized to **timeout=60, retries=3**. This contradicts zero's useful meaning of no timeout/no retries and can unexpectedly terminate a job or repeat a non-idempotent command.

**Fix:** use presence-aware input fields during decoding/default merging, then normalize into the runtime definition. Preserve the distinction through API editing and import/export.

**Regression test:** explicit zero overrides nonzero defaults, while an omitted field inherits them.

### A13 — A disconnected import can leave committed definitions unreconciled

**P2 · Correctness · Source trace**

**Locations:** `internal/api/api.go:1261-1298`; contrast `putJob`, `deleteJob`, and `enable` at `:686,:705,:728`; `internal/daemon/daemon.go:391-399`.

`importApply` commits `ImportDefinitions` and then calls `s.reconcile(r.Context())`. If the client disconnects or its context is canceled after commit, the subsequent definition query/scheduler reload can fail immediately. There is no periodic registry reconciliation to repair this automatically.

The database can show a changed, disabled, or replaced definition while its old scheduler/worker loop remains active until another reconciliation. The other mutation endpoints already use `context.WithoutCancel` after committing; import is inconsistent with them.

**Fix:** schedule post-commit reconciliation on a daemon-owned, bounded context, independent of the HTTP caller, and retry failures. Merely returning HTTP 500 does not roll back the committed import.

**Regression test:** cancel immediately after import commit; verify old loops are replaced/stopped without another API call or daemon restart.

### A14 — Non-SSE expensive reads have no shared admission/work budget

**P2 · CPU/memory scalability · Source + benchmarks**

**Locations:** `internal/api/api.go:969-991,1042-1079`; `internal/store/store.go:761-948`; `internal/logstore/logstore.go:1095-1102,1133-1287,1293-1327`.

Only SSE acquires `streamSlots`. Ordinary JSON log pages, raw downloads, and metrics requests have no comparable admission gate or shared byte/work quota.

- A JSON log page can contain roughly **16 MiB of raw payload**, plus frame objects and base64/JSON encoding buffers. Parallel requests multiply that live working set.
- Raw downloads are paged, which is good, but an unlimited number of downloads can decompress concurrently.
- Metrics allocate one duration sample per relevant run and sort the samples. Cost is O(N) memory and O(N log N) sorting per request. At 30,000 in-window rows, the existing fixture already costs approximately **31 ms and 6.99 MB of allocations per request**.
- The four-connection SQLite reader pool bounds simultaneous SQL access, not all post-query sorting/encoding or waiting HTTP goroutines. `WriteTimeout` is not a handler CPU deadline; the metrics sort does not check context cancellation.

Importantly, the metrics query **does use `MULTI-INDEX OR` and the queued/end-time/active indexes** in the measured revision. This finding is not an allegation of an unconditional whole-history table scan.

**Background relevance:** these costs are request-driven. The purpose of limits is protecting job/worker execution during rare diagnostics, not maximizing dashboard throughput.

**Fix:** add bounded admission and byte/work budgets for expensive reads, separate request-work deadlines from socket write deadlines, and keep downloads paged. Coalesce/cache metrics on demand for short intervals if needed; do not add eager refresh or continuously maintained quantile structures without a measured need. Prefer stale results, throttling, or a retryable busy response to competing with capture, terminal persistence, or process control.

**Regression/load test:** concurrent metrics, JSON logs, raw downloads, and active writers; measure peak heap, allocation rate, DB waits, and run admission/timeout latency—not just response throughput.

### A15 — Background pending work is not bounded by the execution limit

**P2 · Memory/disk growth under sustained imbalance · Source trace**

**Locations:** `internal/executor/executor.go:453-578`; `internal/logstore/logstore.go:132-140,353-405`; `internal/daemon/daemon.go:402-410,489-507`.

The running-job semaphore does not constrain:

- One retry goroutine/timer and retained run/definition snapshot per failed run awaiting retry.
- Background finalizers spawned when terminal persistence fails.
- The archiver's growable `queue []string` and queued-ID map, or the sealed directories waiting behind it.

Long retry delays with frequent failing runs accumulate pending timers even while only a small number of commands run simultaneously. An update-specific storage failure can similarly accumulate finalizers while new admissions still succeed. If completed-log production outruns archival, sealed files grow independently of active-run limits; repeated archive failure leaves them for later orphan sweeps.

This is a workload-dependent capacity risk, not evidence that those queues grew during the idle test. For illustration, 100 failures/second with a one-hour retry delay represents roughly 360,000 waiting first retries before overlap/budget policies are applied.

**Background relevance:** this can grow with no viewers for hours or days, so it moves ahead of archive-read speed in the remediation order.

**Fix:** explicit global pending budgets; one bounded retry scheduler instead of a goroutine per pending item; durable/coalesced finalization records where necessary; disk-pressure admission controls and bounded in-memory archive discovery. Under approved policy 4B, durably queue excess execution work with count/byte/age limits and bounded replay, rather than skipping immediately. Reject explicitly when full or persistence is unavailable, and expose expiry; never silently discard required finalization/retry state. This execution queue does not replace independent archive/finalizer budgets. Log loss is allowed only under the explicit capture-failure/pressure policies (2A/3A), with missing-output evidence when possible. Export pending retry/finalizer counts and sealed-buffer bytes, not only queue length.

**Regression/load test:** sustained fast failures with long retry delays and independently degraded metadata/archive writes; ensure bounded memory and a documented overload outcome.

### A16 — Alert overload blocks the run-completion path on SQLite

**P2 · Contention/availability · Source trace**

**Locations:** `internal/alerts/alerts.go:153-194`; `internal/executor/executor.go:252-266,509-517,605-613`.

Successful alert enqueue is asynchronous. However, a missing channel or full queue calls `report` synchronously while holding `Dispatcher.mu.RLock`. `reportAll` can wait up to five seconds on the metadata database **for each affected channel**.

The executor calls `Notify` before removing the run from `active` and releasing its job-capacity slot. During the exact overload condition where the system should shed notification work cheaply, completed processes can keep execution capacity occupied. Registry replacement also waits for the notification read lock.

**Background relevance:** unattended runs rely on failure evidence and alerts, not an operator watching live output. This completion-path dependency matters even with zero viewers.

**Fix:** record drops through a separately bounded/coalesced observation path, or use nonblocking counters with later persistence. Preserve notification failure/drop observability without making job completion depend on the alert database write. Do not compensate with unbounded queues or a goroutine per dropped alert.

**Regression test:** fill alert queues, stall the recording callback, and verify executor completion/capacity release remains prompt and registry updates remain bounded.

### A17 — `db_max_size` accepts values that overflow its MiB conversion

**P3 · Numeric safety/disk budget · Reproduced**

**Locations:** `internal/config/config.go:361-363`; `internal/daemon/daemon.go:544-578`.

Validation checks only that the MiB value is nonnegative. On the audited 64-bit platform:

```text
db_max_size=8796093022208
int64(db_max_size)<<20 = -9223372036854775808
```

The configuration loads successfully, but `pruneLogs` interprets the negative byte limit as disabled size pruning. Other very large inputs can wrap to small positive budgets. These are extreme inputs, so this is lower priority than the ordinary-workload issues.

**Fix:** bound MiB values before shifting (`value <= math.MaxInt64 >> 20`) and consider a realistic operational maximum.

**Regression test:** largest valid value, first overflowing value, and values that wrap to zero/positive byte limits.

## Additional capacity and operational observations

These are risks/design limits rather than newly demonstrated correctness defects:

1. **`log_max` is not a total disk quota.** It limits each run's raw hot-buffer bytes; successful archival releases that capacity. The global archive size policy runs daily, is disabled by default, and excludes live buffers/quarantine/WAL. A configured archive budget can be exceeded substantially between sweeps. Monitor total data-directory free space and enforce an earlier disk watermark if a hard safety bound is required. References: `internal/daemon/daemon.go:523-579`, `internal/logstore/logstore.go:169-178`, `docs/operations.md:29-74`.
2. **Main metadata file size does not shrink after retention.** This is already documented. Deleted SQLite pages can be reused, but peak `minicron.db` allocation remains on disk. Soft-deleted definitions are never purged, each retains up to 100 revisions, and retention always protects the newest terminal run. Continuous churn through new definition names therefore has no global metadata bound. References: `internal/store/store.go:285-305,457-478,1008-1051,1142-1161`.
3. **Quarantine has no automatic byte/age budget.** Preserving damaged logs is appropriate, but `DeleteRuns` removes only the ordinary run directory and archive data, not `.quarantine/<id>`. Alert operators to quarantined bytes and provide a deliberate repair/purge policy. References: `internal/logstore/logstore.go:486-515,1350-1406`.
4. **A read page and a worker archive batch can delay that run's log writes.** Reads pin the per-run layout while querying/decompressing. Worker archival holds the run and writer locks across file reads, a database transaction, unlinking, and index fsync. Batching limits the work but does not remove storage stalls from the stdout/stderr path. Measure archive-induced writer blocking with **zero viewers** first, then incremental interference from bounded reads. Prefer shorter lock ownership and throttled diagnostics to more archive/read concurrency; preserve commit-before-unlink and migration consistency. References: `internal/logstore/logstore.go:1133-1287`, `internal/logstore/archive.go:31-85`.
5. **Some metadata reads still use the single writer connection.** `Definitions`, `Definition`, `Run`, and retention selection use `s.db`; listings decode whole definition specifications while rows hold that connection. Under many definitions or expensive specs, API traffic can delay admission/state updates despite the separate reader pool. Move appropriate reads only after reviewing consistency requirements. References: `internal/store/store.go:258-323,958-965,1008-1051`.
6. **Physical storage still needs testing.** Frames flush zstd output individually; fsync batching depends on buffered input. Sparse output can approach one sync per line even in batch mode. `storage.synchronous=full`, index rewrites, and archive copies add writes. Tmpfs measurements cannot establish SSD/HDD latency, write amplification, or endurance. D02 now records explicit approval for batched log fsync and its OS/power-loss trade-off; this is not permission to weaken metadata/queue durability for live-view latency. References: `internal/logstore/logstore.go:653-689,743-759,860-875`.
7. **Workers and child resources are outside the job concurrency budget by design.** A large worker registry or memory-hungry children can exhaust resources independently of `max_concurrent_runs`. Use systemd/cgroup CPU, memory, task, and I/O limits for trusted workloads; process groups are not a sandbox. Reference: `internal/executor/executor.go:30-33,141-157`.

## Remediation order

Markers reflect the post-fix tree `57063ba` (2026-10-06): **[done]**, **[partial]**, **[remaining]**.

1. **Unattended correctness and safety:** repair worker finalization (A02) **[done]**, timeout/shutdown enforcement (A10 **[done]**, A04 **[partial]**: residuals a–e above), orphan preservation (A05) **[done]**, and DST/catch-up semantics (A08 **[done]**, A09 **[partial]**: missed count above 100,000 is only warn-logged). Harden root provisioning (A03) **[done]** for per-user installs (root-only branches untested as non-root; `checkPortConflicts` still path-based). These are not acceptable live-lag trade-offs.
2. **Background capacity and configuration correctness:** implement the approved bounded durable execution queue (4B) **[done]** and bound independent pending work/disk pressure (A15 **[partial]**: retries still in memory, no admission control from disk pressure); remove synchronous alert-overload persistence from completion (A16) **[done]**; fix post-commit reconciliation and explicit-zero defaults (A13/A12) **[done]**. Preserve truthful truncation flags and size validation (A11/A17) **[done]**, especially for later diagnosis.
3. **Zero-viewer logging cost:** make payload history demand-driven or eliminate it (D01) **[done]**; implement the approved batched-fsync contract with recovery tests (D02/1B) **[done]**; measure physical-storage capture/archival contention **[remaining]** (one sparse-output fsync-count measurement on a KVM virtual disk exists; no power-loss test, no daemon-level zero-viewer benchmark, no archive-induced writer-blocking measurement). Preserve the low-idle baseline; do not trade it for always-on timers or refresh work.
4. **Reader isolation, not instant UX:** eliminate or byte-bound subscriber payload queues (A01) **[done]**, add expensive-read admission limits (A14) **[done]**, and reuse the CLI transport with slower follow polling (A07) **[done]**. Remaining: rebuild the embedded web bundle and render the `gap` event; run the A14 mixed-load interference test.
5. **On-demand retrieval efficiency:** repair archive cursor seeks (A06) **[done]**; request-scoped caching/coalescing was added only for metrics. Remaining overall: 5A small-server benchmarks, disk-pressure admission (3A/4B), durable retry timers if wanted, and the metadata-growth and writer-connection observations (2, 4, 5 above).

This is the revised investment order, not a requirement to delay small safety fixes behind a larger refactor. Keep severity and feature exposure visible.

## Suggested ongoing regression coverage

- **Lifecycle:** worker completion with readable-but-not-yet-terminal state; cancellation during archive, reload, and import; timeout during blocked persistence.
- **Scheduling:** complete DST fold/gap matrices, restart before the first fire, and more than 10,000 missed cron occurrences.
- **Memory:** maximum line sizes, slow/backlogged SSE readers, repeated raw/JSON requests, long-delay retry storms; inspect retained heap separately from allocation volume.
- **Disk/capture:** archive unavailable for hours, continued child execution and pipe draining during log write/sync failures, truthful missing-output flags, quarantine growth, and pressure pruning before normal retention expiry. Test daemon-crash recovery separately from the accepted OS/power-loss window and verify EOF/seal/shutdown sync behavior.
- **Durable queue:** count/byte/age limits, unavailable persistence, explicit rejection/expiry, restart recovery, definition changes while queued, overlap/retry/catch-up interactions, and bounded replay without an execution storm.
- **Primary performance gate — zero viewers:** active short jobs and long-lived workers with sparse, bursty, chatty, and large-line output; include archive checkpoints, many simultaneous writers, long failure/retry periods, and a no-output idle control. Measure daemon CPU, allocations/GC, retained heap, goroutines/wakeups, fsync count, write bytes, writer-lock/pipe-block time, completion/restart latency, and disk growth. Keep child resource use separate from daemon overhead.
- **Secondary performance gate — occasional diagnostics:** compare the identical background workload with one reader, then slow/backlogged readers and concurrent bounded downloads/metrics. Validate 1–3-second normal display freshness for caught-up readers while prioritizing writer progress; this is not a hard catch-up/shutdown SLA under overload. Measure interference, not just response throughput. No subscriber timers, payload queues, or eager metrics refresh should remain after the last viewer disconnects.
- **Storage/retrieval:** physical-disk fsync/write-byte measurements; cursor cost versus retained chunk count; metrics cost versus in-window row count. Compare changes serially on the same machine/toolchain/filesystem with repeated samples; allocation volume is not retained heap. Existing `BenchmarkFrameEncodingNoTail` is a diagnostic starting point, not proof of a shipped no-tail mode; `BenchmarkPipeThroughput` covers bursts, not sparse output. `BenchmarkWorkerLiveSSE` is a secondary test, and the loaded-daemon benchmark includes API/metrics traffic rather than a pure unattended baseline.
- **Observability:** retry/finalizer counts, subscriber retained bytes, hot/sealed/quarantine bytes, maintenance durations, writer-lock waits, and terminal-persistence lag.

The audit found no evidence of an unconditional idle CPU spin, and the baseline race detector reported no races. Those results should be retained as a baseline, not interpreted as proof against the workload-dependent and semantic defects above.
