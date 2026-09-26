# CPU and memory opportunities

Static code review, 2026-09-26. These are implementation opportunities, not measured speedups. The only recorded baseline is the older [v0.1 measurement](docs/releases/v0.1-measurements.md) (15,417,344 bytes idle RSS on Linux/arm64); it is not a measurement of the current tree. The existing scheduler and log writer benchmarks cover only individual operations, not a running daemon under load.

The suggested changes preserve the current API, UI refresh behavior, exact run metrics, schedule semantics, log retention, and the guarantee that each accepted log frame is durable when `Writer.Write` returns.

| Priority | Workload affected | Opportunity | Expected resource effect |
| --- | --- | --- | --- |
| 1 (implemented) | Many scheduled jobs, especially low-frequency cron jobs | Avoid unchanged schedule-state writes and repeated schedule parsing | Less idle CPU, SQLite work, and WAL I/O |
| 2 | High-volume logs or many simultaneous log readers | Use constant-time live-tail eviction and smaller internal read batches | Less CPU per frame and lower peak heap |
| 3 | Large run history with the dashboard or metrics page open | Make metrics scan index-friendly and bound temporary work | Less query CPU and temporary memory |
| 4 (implemented) | Idle daemon or long-running alert batches | Replace the alert poll ticker with a deadline timer | Fewer idle wakeups |
| 5 (implemented) | Large retained history | Select and delete retention candidates in bounded pages | Lower sweep memory and shorter database stalls |

## 1. Scheduler: persist only changed state, compile once per job

The core P1 changes are implemented in [`scheduler.go`](internal/scheduler/scheduler.go) and [`store.go`](internal/store/store.go). The remaining ideas below are optional follow-up work.

### Why the previous loop cost resources

Each enabled scheduled job has its own loop. Suppose a job runs at 02:00 daily. At 02:01, the loop calculates tomorrow's 02:00, writes it to `schedule_state`, and sleeps for at most 30 seconds. Previously, at 02:01:30 it calculated the **same** tomorrow-02:00 instant and wrote the same row again. This repeated until the fire. The 30-second wakeup lets the scheduler notice wall-clock changes; the repeated database update was unnecessary. The loop now compares the result with its last successfully persisted state before writing ([`Scheduler.loop`](internal/scheduler/scheduler.go)).

`SetScheduleStateWithNext` executes `INSERT ... ON CONFLICT DO UPDATE` when called ([`store.go`](internal/store/store.go)). With 1,000 daily jobs, the old 30-second cadence could issue about 120,000 redundant updates per hour (1,000 × 120), plus real fire/reload work. This is a code-derived rate, not a measured throughput. The main SQLite pool allows one connection and uses WAL with `synchronous=FULL`, so those updates competed with run transitions and API reads and generated avoidable database/WAL work.

Every recalculation still calls `nextFireDistinct`. Previously this reparsed cron or `@every` text and reloaded the location on each call. The loop now compiles those inputs once. The DST check still walks toward the candidate in six-hour steps ([`scheduler.go`](internal/scheduler/scheduler.go)); a distant fire can therefore consume CPU on a 30-second check. The actual saving depends on job count, schedule frequency, timezone, and storage; it needs measurement.

### Implemented and possible follow-up work

1. **Implemented: suppress identical writes.** Each loop reads the persisted next fire on startup and tracks the last successfully persisted `last fire` and `next fire`. It writes after initialization, catch-up, and a fire, or when recomputation changes either value. The in-memory copy advances only after a successful database write. The 30-second clock check and the API's `next_fire_us` display remain in place ([`api.go`](internal/api/api.go), lines 431–450).
2. **Implemented: compile scheduling inputs once per loop.** The loop parses the cron expression or `@every` duration and resolves the timezone once, then reuses them for ordinary next-fire, DST gap/fold, and catch-up calculations. A reload creates fresh compiled inputs; the durable hash and watermark still reset the anchor after a schedule/timezone change.
3. **Consider reusing the pending fire between clock checks.** The loop can compare wall-clock elapsed time with monotonic elapsed time and recalculate only after a detected clock jump or a real fire. Keep the maximum 30-second check interval and recompute immediately after a jump. This is more delicate than the first two steps; small clock corrections and DST behavior need explicit review. Never recompute from `now` once `now >= next` before firing, because `nextFire` is strictly after its input and would skip the due occurrence (the code already calls this out at lines 165–172).
4. **Make reload selective only if reload cost remains material.** [`Scheduler.Reload`](internal/scheduler/scheduler.go) currently cancels and restarts every job loop (lines 35–56), and [`Daemon.reconcileLocked`](internal/daemon/daemon.go) invokes it after definition mutations (lines 333–342). Retaining unchanged loops would avoid their setup calculations and state writes. Treat this as a separate lifecycle change: the loop for an edited job must stop before its replacement starts, and the existing watermark, catch-up, and enabled-state behavior must survive a reload near a fire.

The implemented steps target redundant SQLite writes and parsing without changing when a job fires or when the UI sees its next fire. Validate with many daily jobs, `@every` jobs, DST gaps/folds, a wall-clock jump, a reload near a fire, and a failed schedule-state write; compare SQLite write counts and scheduler CPU before and after.

## 2. Logs: bound work per frame and per reader

The live tail is already capped at 5,000 frames/16 MiB per run and 64 MiB globally ([`logstore.go`](internal/logstore/logstore.go), lines 31–36 and 98–102). Once full, each new frame evicts the oldest with `slices.Delete(w.history, 0, 1)` (lines 411–415), shifting the remaining frame headers every time. Replace the front-deleted slice with a circular buffer or head index. Clear removed `Frame.Payload` references, keep both existing limits, and preserve `Snapshot`, `Subscribe`, sequence order, and `discardHistoryThrough` behavior. This helps high-line-rate jobs without weakening the memory limit.

`Writer.Write` clones every payload before encoding ([`logstore.go`](internal/logstore/logstore.go), lines 390–408). The clone is needed when a frame is retained by history or an SSE subscriber, because `Pipe` reuses its line buffer. It can be avoided when neither will retain it; decide that while holding the writer lock and keep the encoding synchronous. This is a smaller allocation improvement than the ring buffer, so measure it after the larger changes.

`ReadContext` can accumulate 16 MiB of decoded frames per call, with a one-frame exception for an oversized valid frame ([`logstore.go`](internal/logstore/logstore.go), lines 640–665). The SSE backlog asks for up to 5,000 frames per read, and up to 64 SSE streams are admitted ([`api.go`](internal/api/api.go), lines 149–150 and 884–931). Give internal SSE and raw-download reads a smaller byte budget or a streaming frame iterator, while leaving the public paginated log API behavior intact. A single large frame must still advance the cursor. Keep the run-tier lock or an equivalent stable snapshot so archival cannot make a reader skip frames. Smaller reads also shorten the period that log writers wait behind readers.

For file-backed chunks, `ReadContext` calls `chunkFiles` and walks from the first file on every page, while the archive database already skips chunks using `last_seq` ([`logstore.go`](internal/logstore/logstore.go), lines 688–719; [`logdb.go`](internal/logdb/logdb.go), lines 155–177). Use sealed-chunk `First`/`Last` metadata from the index to skip files wholly before `after`, and read the active unsealed chunk as today. Preserve crash-salvage behavior when the index is missing or incomplete. This matters for large worker logs, SSE reconnects, and raw downloads.

Do **not** batch away `w.enc.Flush()` and `w.file.Sync()` per accepted frame ([`logstore.go`](internal/logstore/logstore.go), lines 395–402). The code explicitly treats `Write` as a durability boundary; reducing sync frequency would change crash behavior.

## 3. Metrics: reduce full-history scans without changing values

`RunMetrics` reads every run whose queue or end time intersects the requested window, plus active runs ([`store.go`](internal/store/store.go), lines 700–800). The query is `queued_us>=? OR ended_us>=? OR status IN (...)` (line 738). There is an index on queue time and a partial active-status index, but no standalone `ended_us` index ([`store.go`](internal/store/store.go), lines 175–179). On a large database, the `OR` query may scan many unrelated rows; confirm with `EXPLAIN QUERY PLAN` against realistic data. If needed, add an end-time index and express the three branches as a deduplicated union of run IDs or another plan that uses the indexes. Account for the added index cost on run creation and completion.

The function also stores durations in global, per-job, and per-bucket slices, then sorts them for exact p50/p95 (lines 703–708 and 811–834). For large windows this creates temporary memory proportional to matching finished runs, roughly three retained duration values per run before slice overhead. Retain exact percentile semantics while reducing duplication: for example, process one group at a time, or use an exact selection algorithm on reusable storage. Avoid approximate histograms unless the product explicitly changes its metrics contract. The frontend currently polls these metrics every 10–60 seconds on both dashboard and metrics views ([`queries.ts`](web/src/queries.ts), lines 39–44; [`Dashboard.tsx`](web/src/pages/Dashboard.tsx), lines 24–37), so server-side query improvement benefits each refresh without slowing the UI.

## 4. Alerts: wake only at the next deadline

The alert batcher now uses one resettable timer for the earliest pending `until` and disables it when the map is empty ([`alerts.go`](internal/alerts/alerts.go)). It recomputes the deadline after an item arrives, a batch flushes, or a channel reload splits a batch. Configured batch windows and immediate shutdown flush remain in place. The previous 100 ms ticker woke even with no pending batches. The absolute CPU saving has not been measured.

## 5. Retention: process candidates in pages

The hourly sweep now selects eligible runs in pages of 128 and checks cancellation between pages and deletions ([`daemon.go`](internal/daemon/daemon.go); [`store.go`](internal/store/store.go)). The query uses a stable newest-first cursor, retains the newest terminal run even when it is older than `keep_for`, and skips runs protected by a live 24-hour idempotency key before deleting their logs. A new expression index supplies the retention order; a run/time index supports the idempotency check. The query plan uses both indexes, but the sweep's memory, stall time, and added index write cost have not been measured.

## How to validate changes

Measure idle CPU wakeups and RSS with zero jobs, then with hundreds of daily and `@every` jobs. Separately measure log ingestion with short and maximum-length lines, many concurrent SSE readers, a large archived worker log, a 30-day run history with the dashboard open, and a retention sweep. Compare CPU profiles, heap profiles/peak RSS, SQLite write counts and WAL growth, and API latency before and after each change. The existing [`BenchmarkNextFire`](internal/scheduler/scheduler_bench_test.go) and [`BenchmarkFrameEncoding`](internal/logstore/logstore_bench_test.go) provide starting points; add workload benchmarks for schedule wait/reload, tail eviction, log pagination, and `RunMetrics` because the current benchmarks do not exercise those costs.

Check feature equivalence with DST gap/fold schedules, clock jumps, catch-up and reload races, log crash recovery and stream cursor continuity, exact percentile values, and retention boundaries. No performance measurements or tests were run for this review.
