# minicrond audit: performance, reliability, robustness

Date: 2026-10-04 · Scope: all Go code under `cmd/` and `internal/`, the web UI's polling, and the existing perf notes (`perf.md`, `docs/database-performance-audit.md`) · Commit: `3e833ae`

## How this audit was done

- Read every non-test Go package and followed the lock and ownership boundaries between them: daemon → scheduler/supervisor → executor → logstore/logdb → store.
- Ran `go vet ./...`: clean.
- Ran `go test -race -count=1 ./...`: all 15 packages pass.
- Reproduced the two most serious findings with throwaway programs. The probe test file was deleted afterwards and the working tree is unchanged.
- Did not repeat items that `perf.md` already measured and settled. Where I disagree with one of its conclusions (per-frame fsync), I give the reasoning.

Overall the codebase is careful. It has guarded state transitions, panic boundaries, crash salvage, bounded memory, and good test coverage. The problems below are mostly in places where two careful subsystems meet.

> **Status (2026-10-04):** findings 1–8 are fixed on `main`. Each fix has a
> regression test that fails on the original code (#7 and the supervisor part
> of #3 are covered by review only). Still open from #3: exposing loop health
> through the API and `/readyz`.
>
> **Status (2026-10-04, performance):** findings 9–13 and the smaller
> performance items are fixed on `main`, except the scheduler min-heap, which
> is deliberately deferred (see "Smaller performance items"). Highlights: logs
> use grouped fsync by default (`logs.durability`), finished runs are sealed
> and archived in the background with at most two transfers at once and no
> global mutex, each database has a read-only pool and `BEGIN IMMEDIATE`
> writes, the log archive prunes in batches with incremental vacuum, WAL
> truncation and an optional `logs.db_max_size`, alert bookkeeping is one
> transaction per batch transition and off `Notify`, and
> `storage.synchronous` is configurable. Pool waits and the archive backlog
> are in `GET /api/v1/daemon` under `diagnostics`. Findings 14 and the
> testing gaps outside performance are not started.

---

## Summary

| # | Severity | Area | Finding | Status |
|---|---|---|---|---|
| 1 | **P0** | daemon/supervisor/executor | Deadlock: changing, disabling or deleting a **running worker** hangs the daemon permanently | **Reproduced** |
| 2 | **P1** | api | `run --wait` (and `?wait=true`) fails with `EOF` for any job longer than 30 s; large raw log downloads are cut off | **Reproduced** |
| 3 | **P1** | scheduler/supervisor | One transient DB error permanently stops a job's schedule or a worker's supervision, and nothing visible reports it | Code-verified |
| 4 | **P1** | executor | Terminal-state persistence gives up after 5 s, which leaves runs stuck as `running` with no alert | Code-verified |
| 5 | P2 | executor/daemon | Shutdown returns after force-kill without joining runs, so finalization races DB close | Code-verified |
| 6 | P2 | executor | A full capacity gate records `overlap_skip`, and retries hit by it are silently dropped | Code-verified |
| 7 | P2 | daemon | `Reload` can apply some changes and fail on others (alert channels swapped before definition sync fails) | Code-verified |
| 8 | P2 | logstore | An orphan buffer with a corrupt index is retried forever and never quarantined | Code-verified |
| 9 | Perf-High | logstore | Per-line `fsync` caps output at about 300 lines/s on disk and **slows the child process** | Measured in `perf.md` |
| 10 | Perf-High | logstore | SQLite archival sits on the run-completion critical path behind one global mutex | Code-verified |
| 11 | Perf-Med | sqlite | A single connection per DB lets slow reads delay scheduler and executor writes | Code-verified |
| 12 | Perf-Med | logdb | The daily prune is one unbounded `DELETE`, and freed space is never reclaimed | Code-verified |
| 13 | Perf-Med | alerts | Per-item, per-attempt fsynced DB writes, and some of them happen on the executor completion path | Code-verified |
| 14 | Low | various | Smaller items (see "Smaller items") | — |

---

## P0: Deadlock when a running worker's definition changes

### What happens

`Daemon.Reload` and `Daemon.Reconcile` hold `d.mu` for their whole duration (`internal/daemon/daemon.go:279-316`, `335-342`). Inside that lock, `supervisor.Reload` cancels changed workers and **waits for their loops to exit** (`internal/supervisor/supervisor.go:96-110`). A worker loop exits only after its run's `executor.Wait` channel closes (`supervisor.go:127-136`, `181-191`). The executor closes that channel only after `notifyFinished` returns (`executor.go:425`). `notifyFinished` calls the daemon's `OnFinished` callback, and **that callback takes `d.mu`** (`daemon.go:94-101`).

The result is a lock cycle: Reload holds `d.mu` and waits for the worker; the worker waits for the executor; the executor waits for `d.mu`.

### Who triggers it

Any of these, while a worker is running and its definition changes, is disabled, or is deleted:

- `PUT /api/v1/jobs/{name}` (`api.go:607`), `DELETE` (`api.go:674`), enable/disable (`api.go:694`)
- `POST import/apply` (`api.go:1237`), `POST reload` (`api.go:538`)
- `SIGHUP` or `minicrond reload` with a changed `[[worker]]` in the main config

After that, `d.mu` is never released. All later reloads hang. Every job completion blocks inside its callback, so job capacity slots leak until scheduled runs are all skipped. **Shutdown also hangs** because the shutdown defer takes `d.mu` at `daemon.go:192`. Under systemd the unit is SIGKILLed after `TimeoutStopSec`.

### Reproduction (goroutine dump from a probe test)

The probe config was a `[[worker]] name='w' command='sleep 30'` running under `Daemon.Run`. The probe then changed the command to `sleep 31` and called `d.Reload`. Reload had not returned after 8 s:

```
goroutine 82 [chan receive]:
  supervisor.(*Supervisor).Reload
  daemon.(*Daemon).reconcileLocked
  daemon.(*Daemon).Reload                      <- holds d.mu
goroutine 37 [chan receive]:
  supervisor.(*Supervisor).loop                <- waiting for exec.Wait(run)
goroutine 51 [sync.Mutex.Lock]:
  daemon.(*Daemon).run.func4                   <- OnFinished wants d.mu
  executor.(*Service).notifyFinished
  executor.(*Service).executeRun
```

The existing tests miss this because the API tests stub `reconcile` and no daemon test edits a running worker.

The scheduler has the same shape but is much harder to hit. `scheduler.Reload` waits for loops under `d.mu`. A loop inside `exec.Trigger` whose `logs.Open` fails calls `notifyFinished`, and that also wants `d.mu`.

### Fix

The callback should never need the daemon's lifecycle lock. Two changes, and either one breaks the cycle:

1. Make the dispatcher (and ideally `cfg`) lock-free to read:
   ```go
   type Daemon struct {
       // ...
       alerts atomic.Pointer[alerts.Dispatcher]
       cfg    atomic.Pointer[config.Config]
   }
   // OnFinished:
   if dispatcher := d.alerts.Load(); dispatcher != nil { dispatcher.Notify(run, definition) }
   ```
2. Split the locking. Add a `reloadMu` that serializes `Reload` and `Reconcile`, and keep `d.mu` only for short field reads and swaps. Never hold `d.mu` while waiting on another goroutine.

Add a regression test using the probe pattern above: a running worker, a definition change, and an assertion that `Reload` returns within a few seconds. Add the same test through the API path (`PUT` of a running worker).

---

## P1: `WriteTimeout: 30s` breaks waited triggers and long downloads

Both HTTP servers set `WriteTimeout: 30 * time.Second` (`internal/api/api.go:285`, `315`). In `net/http`, that deadline is armed when the request is read and is not extended automatically. Meanwhile:

- `trigger?wait=true` waits up to `timeout`, which defaults to **120 s** with a maximum of 240 s (`api.go:1424-1430`, `811-831`). `minicrond run NAME --wait` uses the default (`cmd/minicrond/main.go:197`).
- `GET /runs/{id}/log/raw` streams the whole log (`api.go:1036-1045`) with no deadline extension.

Any job that runs longer than 30 s therefore makes `run --wait` fail. A 30-line program with the same server settings shows it:

```
client error: Post "http://127.0.0.1:42857/": EOF
```

The run itself succeeds; only the response is lost. The tests don't catch this because they use a response recorder, not a real `http.Server`.

**Fix:** do what the SSE handler already does and use `http.NewResponseController(w)`:

```go
// trigger, before waiting:
_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + 10*time.Second))
// raw: extend per page inside RawContext's loop, or clear it and rely on ctx:
_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second)) // each page
```

Add one test that uses `httptest.NewUnstartedServer` with the production server settings and a job longer than the write timeout. The test can shrink the timeout to keep it fast.

---

## P1: Scheduler and supervisor loops die on one transient error

`scheduler.loop` handles a failed state read or write with `slog.Error(...)` followed by `return` (`internal/scheduler/scheduler.go:123-126`, `133-139`, `207-210`, `223-226`, `250-253`, `276-284`). That covers `SQLITE_FULL`, an I/O error, or a busy timeout. A returned loop stays dead until the next `Reload`. On a stable system that may be weeks away, so **the job silently never fires again**.

The supervisor does the same thing when `store.Run` fails after a worker exits (`supervisor.go:200-204`). The worker is then neither restarted nor marked fatal.

Nothing outside the log shows this. `/readyz` stays green, and the UI still shows the stale `next_fire_at`.

**Fix:**
- Treat storage errors inside loops as retryable. Back off from 1 s up to 1 min with jitter, then retry the same step. Return only on `ctx.Done()` or a definition that can never work (a schedule that fails to compile).
- Keep a per-loop health record (`last_error`, `since`), show it in `GET /jobs` and the UI, and make `/readyz` (or a new `/healthz?deep=1`) report degraded when any loop is unhealthy.
- When a scheduled trigger fails, the occurrence is dropped silently: the loop sleeps 1 s and recomputes from `now` (`scheduler.go:256-267`). At minimum record a `missed` run (`RecordMissed` exists) so history shows the gap.

---

## P1: Terminal persistence gives up after 5 s

`finishRun` retries `FinishRun` within a **5 s total budget** (`internal/executor/executor.go:499-522`). If that budget runs out:

- the row stays `running` even though the process has exited;
- `notifyFinished` and `scheduleRetry` are skipped, so no alert is sent and no retry is attempted;
- the run is no longer in `s.active`, so `POST /runs/{id}/stop` returns 404 and the UI shows it running until the next daemon restart turns it `interrupted`.

Five seconds is reachable with a single DB connection (see finding 11). A long metrics scan, a large retention page, or a slow fsync on a busy disk can hold the connection that long.

**Fix:** keep the fast in-line attempt. On failure, hand the terminal record to a small **finalizer queue**: a single goroutine that retries with backoff until it succeeds or the daemon shuts down. It then calls `notifyFinished`/`scheduleRetry`. Count queue depth in diagnostics. On startup, recovery already covers anything that is still pending.

---

## P2 reliability items

### 5. Shutdown doesn't join runs after force-kill

When the shutdown context expires, `executor.Shutdown` closes every `force` channel, SIGKILLs the groups, and **returns at once** (`executor.go:918-934`). `daemon.run` then closes the alert dispatcher and the API, and its earlier defers close `ldb` and `st`. Meanwhile the job `execute` goroutines are still draining pipes for up to 5 s, archiving logs (`logs.Close` → `ldb.PutChunks`), and calling `FinishRun`. Those writes fail with `sql: database is closed`, and the run is later recorded as `interrupted` instead of `timeout`/`stopped`. Workers are joined by `super.Shutdown()`; jobs are not.

**Fix:** after force-kill, wait a short bounded extra time (for example 3 s) for `a.done`, and only then return `ctx.Err()`.

### 6. The capacity gate is reported as `overlap_skip`, and retries are lost

When `max_concurrent_runs` is saturated, `trigger` calls `recordSkipped` (`executor.go:134-139`). That writes `end_reason = "overlap_skip"`, which looks exactly like `on_overlap = "skip"`. An operator debugging a missing run will look in the wrong place.

A second effect: a **retry** attempt that meets a full gate becomes `skipped`. That isn't `failed`, so `scheduleRetry` stops the retry chain, and the job's `retries` budget is silently lost under load.

**Fix:**
- Use `end_reason = "capacity_skip"`.
- For `trigger == "retry"`, reschedule the retry with its delay instead of recording a skip.
- Optionally add `on_capacity = "skip" | "queue"`, a bounded wait for the gate.

### 7. Reload can leave some changes applied and others not

`Daemon.Reload` calls `d.alerts.Reload(cfg.AlertChannels)` **before** `SyncConfigDefinitions` (`daemon.go:308-313`). If the sync fails, for example because of a name conflict with an API definition, the new alert channels are live but `d.cfg` and the definitions are old. A channel removed in the new config may still be referenced by old definitions, and those alerts become `dropped`.

**Fix:** validate everything, run the DB sync, and swap in-memory state (alerts, `cfg`) only after the sync succeeds.

### 8. Orphan buffers that can never be archived are retried forever

`archiveOrphan` returns an error for an unreadable or invalid `index.json`, an unsupported version, or duplicate chunks (`internal/logstore/archive.go:139-158`). `ArchiveOrphans` runs at startup and on every worker-flush tick, so the same failures are logged every 15 minutes forever, and the directory is never cleaned up. Each attempt also holds the global `archiveMu` (see finding 10).

**Fix:** after N failures, or immediately for structural errors, salvage what `salvageFrames` can recover while ignoring the index. If that fails too, move the directory to `logs/.quarantine/<run>` and log the move once.

### Alerting gaps (reliability of *knowing* something broke)

- `Notify` only alerts on `failed` and `timeout` (`alerts.go:116`). These important events produce no alert:
  - a worker reaching **fatal** (`supervisor.go:231-233`, log only);
  - runs marked `interrupted` by crash recovery;
  - a dead scheduler loop (finding 3);
  - dropped alerts ("alert queue is full").
- Telegram `429` responses ignore `parameters.retry_after` (`alerts.go`, `Send`). Three attempts at 1 s and 2 s backoff fail during exactly the bursts where batching matters.

---

## Performance

### 9. Per-line `fsync` on the log hot path

`Writer.Write` calls `enc.Flush()` and `file.Sync()` for **every line** (`internal/logstore/logstore.go:441-446`). `perf.md` measured about 320 frames/s on ext4 and decided to keep it. I'd reconsider, for two reasons that `perf.md` doesn't address:

1. **It changes how the job runs, not just how the daemon performs.** `Pipe` holds the run lock and fsyncs before it reads the next line. A child that writes faster than about 300 lines/s fills the 64 KiB pipe and **blocks in `write(2)`**. A job that prints 30 k lines takes about 100 s longer under minicrond than in a shell. That time counts against its `timeout` and changes its `healthy_after` behavior.
2. **The guarantee is weaker than it sounds.** If the daemon crashes, data the child already wrote is lost anyway: whatever sits in the kernel pipe buffer (up to 64 KiB) plus whatever sits in `Pipe`'s `bufio.Reader` (up to 64 KiB). "Durable when `Write` returns" protects the boundary between the pump and the disk. No external observer can see that boundary.

**Proposal: group commit on idle.** Encode every frame immediately and publish it to subscribers and the tail as today, but `Flush`+`Sync` only when one of these happens:

- the reader has no buffered input left (`br.Buffered() == 0`, so the next read would block);
- N bytes have accumulated (for example 256 KiB);
- T ms have passed (for example 50 ms).

Always sync on rotate, close, and timeout or stop. The crash-loss bound becomes "at most about 50 ms or 256 KiB beyond what was already in kernel buffers". That is the same order as today, and throughput becomes disk-bandwidth bound instead of fsync-latency bound. If keeping the old behavior matters, make it `logs.durability = "frame" | "batch"`, with `batch` as the default.

A side effect: SSE readers stop waiting on fsyncs. `readContext` takes the run's read lock, and `Write` holds the write lock across the sync today.

### 10. Archival on the completion critical path, behind a global mutex

`Store.Close` (called from `executeRun` before `finishRun`) takes **`archiveMu`, which is global**, and then copies every chunk into SQLite with `synchronous=FULL` (`logstore.go:147-170`, `archive.go:78-130`). `FlushActive` (every worker, every tick) and `ArchiveOrphans` (the whole directory) hold the same mutex. So:

- one big worker checkpoint (hundreds of MiB in 8 MiB batches) **serializes every job's completion** behind it;
- while a job waits, it keeps its capacity slot, so the next scheduled fires turn into `skipped`;
- run completion depends on `minicron-logs.db` being healthy.

The comment in `executeRun` asks for "close the log sink before the terminal transition". Sealing (`w.Close()`: finish chunk, fsync, write the index) is enough for that. Moving the chunks into SQLite is not needed for correctness, because reads already merge the file and DB tiers.

**Proposal:**
- Split `Close` into `Seal(runID)`, which is synchronous and does no SQLite work, and an async archiver queue drained by one or two goroutines.
- `ArchiveOrphans` then becomes the retry path for that same queue.
- Replace the global `archiveMu` with a small semaphore that bounds memory, or with per-run ownership.

### 11. One SQLite connection per database

`sqlite.Open` sets `SetMaxOpenConns(1)` (`internal/sqlite/sqlite.go:27-28`). That avoids `SQLITE_BUSY` across connections, but it puts API reads and the scheduler and executor writes in one queue:

- dashboard polling: `/runs` every 2 s on the Runs page (`web/src/pages/Runs.tsx:54`), 4–5 s elsewhere, and metrics every 10 s;
- 30-day `RunMetrics`, `RetentionCandidates`, and alert recording;

all of these share the connection with `CreateRun`, `StartRun`, `FinishRun`, and `schedule_state` writes. At today's measured latencies (2–8 ms) this is fine. It becomes a reliability problem when combined with findings 3 and 4: any read that takes more than 5 s can strand a run as `running`.

**Proposal:** the staged plan in `docs/database-performance-audit.md` is right. When you get there:
- open a second `*sql.DB` per file with `mode=ro` and `_pragma=query_only(1)`, 2–4 connections, for API reads;
- keep exactly one writer connection;
- start write transactions with `BEGIN IMMEDIATE` (`_txlock=immediate`) so read-then-write transactions such as `putDefinition`, `AdmitIdempotentRun`, and `SetEnabled` don't fail with `SQLITE_BUSY_SNAPSHOT` once readers exist.

Measure first by logging `db.Stats().WaitDuration` and `WaitCount`. They are already available and cost nothing to expose in `/api/v1/daemon`.

### 12. Log archive pruning and space

`LogDB.Prune` runs `DELETE FROM log_chunks WHERE archived_us<?` as **one transaction** (`internal/logdb/logdb.go:229-250`). On a large archive, that statement:

- holds the only `logdb` connection for the whole delete, which stalls archival and every log read in the UI;
- writes the entire deleted page set into the WAL, which then stays at that size;
- frees pages that are never returned to the filesystem (no `auto_vacuum`), so `minicron-logs.db` only ever grows to its high-water mark.

**Proposal:**
- Delete in batches, for example `DELETE FROM log_chunks WHERE rowid IN (SELECT rowid FROM log_chunks WHERE archived_us<? LIMIT 2000)` in a loop, yielding between batches. Run the orphan `log_runs` cleanup once at the end.
- After pruning, run `PRAGMA wal_checkpoint(TRUNCATE)` and set `PRAGMA journal_size_limit`.
- Turn on `auto_vacuum=INCREMENTAL`. That needs a one-time `VACUUM` migration for existing files. Then run `PRAGMA incremental_vacuum(N)` after each prune.
- Consider a size budget (`logs.db_max_size`) alongside the age budget. Disk-full is the most likely real-world failure for a log store.

### 13. Alert bookkeeping write amplification

- `Dispatcher.Notify` runs on the executor's completion goroutine. It holds `d.mu.RLock()` while doing a **synchronous, fsynced DB write per channel** (`report(..., "queued")`, `alerts.go:115-139`). Move the `queued` record into the batch goroutine, or record it in the same transaction as `FinishRun`.
- `run()` writes one `UPDATE` per item per attempt (`alerts.go:301-303`) and again for the final status (`331-334`). A 256-item batch with 3 attempts means about 1,000 separate fsynced commits. Add `RecordAlerts(ctx, []AlertDelivery)` that writes one transaction per batch transition.

### Smaller performance items

- **Scheduler wakeups.** There is one goroutine per scheduled job, and each wakes at least every 30 s (`scheduler.go:227-235`). At 1,000 jobs that is about 33 wakeups/s while idle. A single min-heap timer plus a 30 s wall-clock recheck would scale better, but it isn't urgent at current sizes. *Deferred:* `perf.md` measured 0.07 CPU-ms/s idle for 100 jobs, and replacing per-job loops would rework catch-up, overlap and retry-on-error handling that was just made self-healing (#3).
- **`PruneMetadata` audit trim.** `DELETE FROM audit WHERE id NOT IN (SELECT id … LIMIT ?)` (`store.go:1136`) builds a set of 10 k ids every hour. `DELETE FROM audit WHERE id <= (SELECT id FROM audit ORDER BY id DESC LIMIT 1 OFFSET ?)` is a single index seek.
- **UI polling.** The Runs page polls every 2 s even when nothing is active, and several views overlap. A single SSE "events" endpoint for run-state changes (the infrastructure already exists for logs) would remove most of this load and make the UI update instantly. At minimum, apply the existing "only poll while something is active" rule to the Runs page.
- **`synchronous(FULL)` + WAL** costs an extra fsync per commit compared with `NORMAL`. `NORMAL` stays crash-safe for process crashes and loses only the last commits on power loss. Keeping `FULL` fits the product's "trustworthy" positioning. Note that a run's lifecycle is 4+ commits (`CreateRun`, `StartRun`, `FinishRun`, schedule state), plus alerts. Consider making this a documented knob instead of a constant.

---

## Smaller items

- **Worker restart backoff is fixed** (`restart_delay`, `supervisor.go:235`). Use exponential backoff with a cap and jitter, so that a crash-looping worker doesn't hammer its dependencies before it reaches `max_restart_attempts`. The failure counter is in-memory only, so a daemon restart resets it.
- **`CleanupRecovered`** sends SIGTERM, sleeps a **fixed 100 ms**, then SIGKILLs. It does this serially for each recovered run (`executor.go:878-889`). Honor the definition's `grace` (capped) and signal all groups in parallel.
- **`env_file` parsing** (`executor.go:681-708`) keeps quotes literally (`A="x"` gives `"x"`) and doesn't accept an `export ` prefix. Users copying a `.env` file will be surprised. Either support the common dotenv subset or document the format strictly.
- **Run pagination cursor**: `RunsPage` resolves `before` by looking up that run's `queued_us` (`store.go:718-722`). If retention deleted the cursor row, the next page returns 404. Encode the cursor as `(queued_us, run_id)` instead.
- **Retention vs. `logs.db_keep_for`**: job runs are deleted (logs included) after `storage.keep_for_default` (7 days by default), so `logs.db_keep_for = 30` has no effect on job logs. Document this or cap one by the other.
- **Observability**:
  - add pool stats, the finalizer and archiver queue depths, dead or unhealthy loop counts, the orphan-buffer backlog, and alert drop counts to `/api/v1/daemon`;
  - optionally add a Prometheus `/metrics` endpoint;
  - `/readyz` should reflect internal health, not only "the listener is up".

---

## Testing gaps that let the P0/P1 issues through

1. **No integration test drives a real `Daemon` through a worker edit.** Add one for `Reload`/`Reconcile` with a running worker (the probe above), and one through the HTTP `PUT`.
2. **API tests use a response recorder**, so server-level settings (timeouts, header limits) are never exercised. Add a small suite on `httptest.NewServer` that uses the production `http.Server` configuration.
3. **No fault-injection tests for "DB fails mid-loop".** Wrap the store, or close the DB under the scheduler, and assert that the loop recovers once the DB is back.
4. **No long-output test** that measures child slowdown. If `logs.durability` is added, test that a child writing 100 k lines finishes within a time bound in `batch` mode.
5. Consider running `go test -race` with `-count=20` on `internal/executor`, `internal/supervisor` and `internal/daemon` in CI. Lifecycle races show up intermittently.

---

## Suggested order of work

1. **Deadlock (P0).** Change `OnFinished` to use `atomic.Pointer` and add a separate `reloadMu`, plus the regression test. This is small and isolated.
2. **WriteTimeout (P1).** Make two `ResponseController` calls and add a real-server test. Small.
3. **Self-healing loops and the finalizer queue (P1).** Retry with backoff in the scheduler and supervisor, add a terminal-state finalizer, and expose loop health. Medium.
4. **Shutdown join, capacity/retry semantics, Reload ordering, orphan quarantine (P2).** Each is small.
5. **Async archival** (`Seal` plus an archiver queue, removing the global mutex). Medium. This removes logdb from the completion path.
6. **Log durability mode** (group commit on idle). Medium, with benchmarks against `perf.md`'s ext4 numbers.
7. **logdb prune batching, checkpoint, incremental vacuum, size budget.** Medium. It needs a one-time migration.
8. **Read pool and `BEGIN IMMEDIATE`, after measuring `WaitDuration`.** Then alert write batching and UI polling.
