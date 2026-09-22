# Go codebase audit

**Date:** 2026-09-22  
**Revision:** `f257ffc`  
**Scope:** Go production code in `cmd/` and `internal/`, selected tests, module dependencies, release workflow, and relevant operational documentation. Conducted directly, without subagents. Application code was not changed.

## Executive summary

The project has a sensible small-service foundation: parameterized SQLite queries, transactional definition updates, bounded job concurrency, hashed bearer tokens, Unix peer authentication, strict TOML decoding, and useful lifecycle tests.

However, passing tests currently mask important production risks. Prioritize:

1. Bound log ingestion and memory by bytes, not just completed lines/frame counts.
2. Guarantee a single worker lifetime across reload/start/restart.
3. Make trigger idempotency and scheduled occurrence admission durable and atomic.
4. Correct DST recomputation and complete log streaming.
5. Fix shutdown, startup/reload synchronization, and log durability/error handling.
6. Harden non-loopback exposure and keep administrative credentials out of routine logs.

**Threat model:** This is a trusted, single-admin execution service, not a hostile-workload sandbox. An authenticated administrator can intentionally execute commands as the daemon user; with a root daemon this is root-equivalent access. Findings involving authenticated configuration are generally reliability/defense-in-depth issues, not independent privilege-escalation claims. Network exposure increases the severity of transport and resource-exhaustion risks.

**Priorities:** P1 = address before relying on production guarantees; P2 = next hardening/performance iteration; P3 = incremental improvement. “Reproduced” means a targeted observation test demonstrated the behavior; otherwise findings are based on code inspection, not an asserted exploit or measured production incident.

## Verification performed

Environment: `go1.24.4 linux/arm64`.

| Check | Result |
|---|---|
| `go test ./...` | Passed; normal invocation used cached results |
| `go vet ./...` | Passed |
| `CGO_ENABLED=1 go test -race -count=1 ./...` | Passed, fresh execution |
| `go test -cover ./...` | Passed; package coverage ranged from 24.4% for alerts to 83.0% for scheduler, excluding model and commands; commands had 0% |
| Existing scheduler/logstore benchmarks, `-benchmem` | Passed; results below |
| Eight temporary audit observation tests | All demonstrated the behaviors listed below |

Observation tests were supplied through a Go `-overlay` file in `/tmp`, without adding tests to the repository. They assert the **current faulty or noteworthy behavior**, so their passing does not indicate correctness. Promote these scenarios into regression tests that assert the desired behavior when fixing them.

| Targeted scenario | Observed result |
|---|---|
| `drop_old`, 28-byte cap, write `old!` then `new!` | Kept `old!`, rejected `new!` |
| Same cap, three frames with archive flush between writes | Archive retained all three frames: 84 raw frame bytes; consistent with ADR-6's accounting-only cap, but not a total storage cap |
| Write one short log frame without closing/flushing | Successful `Write`, but chunk file remained zero bytes |
| Parse `-1MiB` / `8589934592GiB` | Accepted negative size / overflowed to `-9223372036854775808` |
| Stream a finished 5,001-frame run | Emitted 5,000 frames, then `done` |
| Reuse an idempotency key aged 25 hours | Lookup missed it; saving replacement failed with UNIQUE constraint |
| New York `30 1 * * *`, recompute at `2026-11-01T05:31:00Z` | Selected `06:30Z`, the second 01:30 in the DST fold |
| Reload an unchanged worker that takes time to stop | Observed two concurrent active runs for the same worker |

**Limits:** No live vulnerability-database scan was performed (`govulncheck` was not installed). No dependency is declared vulnerability-free. No remote penetration test, power-loss test, sustained load test, or macOS runtime test was performed. Passing the race detector only covers exercised interleavings. Frontend security was not comprehensively audited.

## Findings and recommendations

### F01 — P1: Log ingestion can exhaust daemon memory

**Evidence:** `internal/logstore/logstore.go:394` (`Write`), `:454` (`Pipe`), `:537` (`decode`).

- `Pipe` uses `bufio.Reader.ReadBytes('\n')`, accumulating the entire line before `Write` applies `maxLine`. A child continuously writing without a newline can exhaust memory despite `logs.max_line`.
- The history tail retains up to 6,250 frames, not a byte budget. At the default 256 KiB line cap this can retain approximately **1.53 GiB of payload per writer**, even when older disk chunks have been evicted. Multiple workers compound this.
- `decode` allocates the uint32 payload length from the frame header before validating it against a maximum. Corrupted stored data can request an allocation approaching 4 GiB. This is a corruption-resilience issue; ordinary job text cannot directly set framing headers.

**Improve:** Read bounded fragments (`ReadSlice` or an equivalent incremental scanner), discard excess bytes while continuing to drain, and preserve partial/truncation flags. Enforce both per-writer and global tail-byte budgets. Validate decoded lengths and decompression limits before allocation. Bound response bytes as well as frame counts.

**Verify:** Endless newline-free output; concurrent maximum-size lines; malformed frame lengths; RSS plateau under sustained output. Keep tests bounded rather than deliberately exhausting the host.

### F02 — P1: Worker lifecycle operations can overlap or fail to restart

**Evidence:** `internal/supervisor/supervisor.go:53-145`, `:151-188`; `internal/api/api.go:466-474`; `internal/executor/executor.go:88-110`.

**Reproduced:** Reloading an unchanged worker produced two active runs.

`Supervisor.Reload` cancels old loops and immediately starts replacements without joining either the loops or their processes. Cancellation requests `exec.Stop` but does not await completion. Workers bypass the executor's job overlap/capacity checks. Every definition reconciliation reloads all workers, including unchanged ones.

`StartDefinition` checks `active`, but the loop populates that map later; repeated starts or a start during backoff can create multiple loops. `workerRestart` substitutes a fixed 200 ms delay for a completion handshake: if the old worker is still active, the new start may be skipped; the old loop can then follow `restart=never` or backoff instead of an immediate restart.

**Improve:** Give each worker a serialized lifecycle state machine with a reserved starting/backoff/running/stopping slot. Diff definitions on reconciliation. Cancel and join the old lifetime before starting its replacement. Use executor completion channels, not sleeps, for restart. Route worker triggers through the same ownership gate or reject them on the generic job-trigger endpoint. Join supervisor loops at shutdown.

**Verify:** Simultaneous starts; start during backoff; unrelated job edits; repeated reloads; slow graceful termination; restart with `restart=never`; stop racing with start. Assert at most one live worker process, not just one map entry.

### F03 — P1: Trigger idempotency has crash, cancellation, expiry, and retention gaps

**Evidence:** `internal/api/api.go:378-405`; `internal/store/store.go:316-330`, `:569-572`.

The API creates/launches the run before inserting its idempotency record. The mutex prevents concurrent in-process duplicates, but a crash, canceled HTTP context, or database failure between these operations leaves an executing run without the key. Retrying can execute it again.

**Reproduced:** Expiry is only a lookup filter. An expired record still occupies its primary key, so reuse launches a run and then fails to save the key. Repeated retries can repeat this. Separately, retention can remove a run while its unexpired key still points to it, causing replay to fail.

**Improve:** In one transaction, reserve/reuse the key and create a durable pending run. Dispatch only after commit. Replace expired records transactionally; periodically prune them. Retain replay metadata for the promised idempotency window. Define recovery of committed-but-not-started runs and avoid promising exactly-once external side effects.

**Verify:** Fail/cancel after each boundary; concurrent same-key requests; 24-hour rollover; retention before replay; restart between admission and process launch.

### F04 — P1: Scheduling can duplicate DST slots or lose occurrence accounting

**Evidence:** `internal/scheduler/scheduler.go:60-161`, `:163-208`; `internal/store/store.go:606-615`.

- **Reproduced DST issue:** Fold suppression compares the candidate with the current `after` wall-clock minute. After the first 01:30 fires, a recomputation at 01:31 can select the second 01:30. The loop recomputes every 30 seconds, so the existing direct-next-fire fold test does not establish lifetime correctness.
- Catch-up stops after 10,000 occurrences. With `@every 1s`, an outage exceeding approximately 2h47m makes `latest` select an old slot rather than the actual latest one and undercounts missed occurrences. The main loop then jumps ahead from current time.
- Run creation and schedule-state advancement are separate writes. Trigger failures still advance `last`; state-write failures are ignored. Crashes between writes can duplicate a previously admitted occurrence, while storage failures can silently lose its accounting.

**Improve:** Persist an occurrence identity and enforce uniqueness, e.g. definition/schedule generation/scheduled instant, atomically with pending-run admission and watermark updates. Preserve first-occurrence DST policy across wall-clock recomputations using last-fired state. Calculate interval catch-up arithmetically; use bounded, resumable cron catch-up with explicit summarization rather than silent truncation. Surface storage errors.

**Verify:** Drive a full scheduling loop across a DST fold, including periodic recomputation; multi-day one-second downtime; database failures at each persistence boundary; restart after admission but before watermark update.

### F05 — P1: Log durability is weaker than documented, and close errors can discard buffers

**Evidence:** `internal/logstore/logstore.go:135-157`, `:210-247`, `:343-392`, `:507-520`; `internal/executor/executor.go:185-194`, `:290-312`; `internal/logdb/logdb.go:63-65`; `docs/adr/0006-hybrid-log-storage.md`.

**Reproduced:** One successful short `Write` left a zero-byte chunk file. The zstd writer buffers output, and there is no periodic encoder flush in the ingestion path. A process crash can therefore lose accepted output still in encoder memory. Files are synced when sealed, not on each accepted frame. Index replacement does not sync the temporary file or parent directory.

There is also a concrete error-propagation flaw: executor calls `w.Close()` and ignores its error. `Close` sets `closed=true` before finalization; a subsequent `Store.Close` sees `w.Close()` return nil. It can archive only the indexed chunks and remove the directory despite an earlier failed finalization. Archive-close errors are also ignored by executor cleanup.

SQLite uses `synchronous=NORMAL`. That is a performance/durability trade-off, not automatically a defect, but a power failure can lose recent commits. Removing synced source chunks immediately after a non-power-durable archive commit weakens the end-to-end guarantee.

**Improve:** Define explicit durability levels and a maximum loss window. Add timed encoder flushes and appropriate fsync/checkpoint policy. Preserve the original close error and failed buffers until safely retried. Use a single finalization owner and report failures. If promising power-loss durability, synchronize archive commits before deleting source chunks and durably replace indexes. Otherwise correct the “already durable” documentation.

**Verify:** Fault-inject encoder close, file sync, index rename, archive commit, and cleanup failures; SIGKILL a sparse-output run; verify salvage. Test power-loss behavior separately from process crashes.

### F06 — P1: Shutdown deadlines do not ensure process-tree cleanup

**Evidence:** `internal/executor/executor.go:433-450`, `:531-569`; `internal/daemon/daemon.go:122-143`.

`stopGroup` returns as soon as the group leader exits after the graceful signal, even if same-group children ignore it. Those children never receive the later SIGKILL. Closing their inherited pipes after five seconds does not terminate them. This is separate from the documented limitation that intentionally detached descendants can escape.

`Service.Shutdown` returns when its deadline expires without forcing remaining groups down or reporting incomplete cleanup. Configurable grace may exceed the daemon's 15-second executor budget. The daemon can then close databases and release its lock while old execution goroutines/processes remain. The supervisor does not join its loops either.

Crash recovery also has a spawn-to-`StartRun` window: a process may exist before its PID/start identity is recorded. PID/start-time matching alone does not include an OS boot identifier for reboot recovery.

**Improve:** Stop admission and producers, join supervision, then enforce a bounded TERM-to-KILL sequence for the whole group. Track group liveness independently of leader exit; return shutdown errors and do not close dependencies while users remain. For stronger Linux process-tree guarantees, use per-run cgroups or a service-manager scope. Record the kernel boot identity for recovery and explicitly document the unavoidable launch/recovery boundary.

**Verify:** Leader exits on TERM while child ignores it; grace exceeds shutdown budget; descendants close stdout but keep running; restart while old groups remain; crash immediately after spawn.

### F07 — P1: SIGHUP during startup can race initialization or panic

**Evidence:** `cmd/minicrond/main.go:81-100`; `internal/daemon/daemon.go:44-121`, `:169-216`.

The SIGHUP goroutine starts before `Daemon.Run` initializes its fields. `Run` writes fields without `d.mu`, whereas `Reload` reads them under the mutex. Once `cfg` and `store` exist, `ready()` permits reload even though `alerts`, scheduler, or supervisor may still be nil. `d.alerts.Reload` can dereference nil during that window. The signal loop also has no context-based exit or `signal.Stop` cleanup.

**Improve:** Publish a fully initialized runtime under synchronization, with explicit starting/ready/stopping states. Register or enable reload only after readiness; reject or defer earlier requests. Make the signal loop observe cancellation and unregister notifications on exit.

**Verify:** Repeated SIGHUP during deliberately paused startup stages, failed startup, and shutdown under `-race`.

### F08 — P2: Log streaming and tier migration can omit output

**Evidence:** `internal/api/api.go:553-618`; `internal/logstore/logstore.go:135-157`, `:492-520`, `:595-667`; `cmd/minicrond/main.go:162-196`.

**Reproduced:** SSE reads only one 5,000-frame backlog page and then emits `backlog_done`/`done`. A completed longer run is silently truncated. For active runs, frames between that page and subscription time can also be missed.

Other inspection findings:

- Database read, file glob/open, and memory snapshot are not one consistent view. Archival can remove files after the DB read or glob, producing missing frames or transient ENOENT failures.
- A caller can obtain an active writer, race its close, then subscribe to the already-closed writer. `Subscribe` does not check `closed`, leaving a channel that never gets its normal completion signal.
- CLI non-follow logs return after the first 1,000-frame page.

**Improve:** Page backlog until a captured high-water sequence is reached, then drain the subscribed live stream with deduplication. Coordinate tier handoff through a manifest/generation or a retryable consistent read; do not merely suppress missing-file errors and silently lose frames. Immediately close subscriptions to closed writers. Expose an explicit gap/truncation marker when requested history has expired. Drain all pages for non-follow CLI output.

**Verify:** More than 5,000 frames, close during subscription, concurrent flush/archive/retention, slow clients, and CLI output above 1,000 frames.

### F09 — P2: Log retention semantics need correction and explicit storage budgets

**Evidence:** `internal/logstore/logstore.go:394-436`; `internal/logdb/logdb.go:110-134`, `:172-187`; ADR-6.

**Reproduced ring bug:** When `log_max` is below the 1 MiB chunk threshold, `drop_old` can fill the current unsealed chunk and refuse every subsequent frame. Eviction only removes sealed chunks, and rotation occurs after the capacity check. It behaves like `drop_new` until an external flush seals the chunk.

**Design caveat, not an undisclosed bug:** ADR-6 deliberately limits ring accounting, not archive size. Evicting an archived chunk only removes its file/accounting entry; its archive row remains. A high-output worker can consequently consume substantial storage within the age-retention window despite a small `log_max`.

Archive pruning deletes an entire run based on its **first archive time**. A worker older than the cutoff loses recently archived chunks together with old ones; this is not a rolling per-chunk retention window. SQLite deletion also does not normally shrink the database file immediately.

**Improve:** Make drop-old work within small budgets by rotating before eviction or using byte-bounded segments. Name/document separate buffer, archive, and global disk budgets. If rolling age retention is desired, timestamp/prune chunks rather than whole active runs. Monitor free space, WAL size, and freelist growth; adopt a measured incremental-vacuum/maintenance policy.

**Verify:** Caps below/equal/above chunk size, archive flush between writes, high-rate long-lived workers, and recently written chunks on an old run.

### F10 — P2: Validation accepts unsafe limits and silently ignored settings

**Evidence:** `internal/config/config.go:183-300`; `internal/logstore/logstore.go:716-728`; `internal/model/model.go:27`, `:49`; `internal/daemon/daemon.go:75-82`.

**Reproduced:** Negative byte sizes and multiplication overflow are accepted. Negative `MaxBytes` disables checks that only run for positive values.

Validation also omits important enum/range checks: `restart`, `env_base`, nonnegative/positive duration semantics, restart attempts, success-code range, global concurrency, global retention settings, and a valid bounded `logs.max_line`. Invalid max-line settings silently fall back at startup; invalid global keep duration makes retention skip work. Unknown JSON fields are accepted. `run_on_start`, worker `priority`, and `storage.audit_keep` are exposed but have no implementation references beyond their declarations.

**Improve:** Use overflow-safe size parsing (`value <= MaxInt64/multiplier`), explicit zero semantics, and practical upper bounds. Validate all supported enums, ranges, secret references, and JSON fields before persistence. Reject unsupported settings or implement them; do not silently accept operational promises. Check for trailing JSON documents as well as unknown fields.

**Verify:** Table-driven boundary tests and fuzzing for size, duration, JSON, TOML, and schedule inputs. Assert each exposed setting changes behavior or is explicitly rejected.

### F11 — P2: Missing secrets fail open; clean environment does not control executable lookup

**Evidence:** `internal/executor/executor.go:321-342`, `:383-431`.

Unreadable `env_file` and unresolved `secret_env` references are silently omitted. Jobs can run with missing credentials or fall back to inherited/default values instead of failing safely. `secret_env` reference syntax is not validated by definition validation.

`exec.Command` resolves a bare executable using the **daemon's PATH before `cmd.Env` is assigned**. Thus `env_base=clean` and its `/usr/bin:/bin` PATH do not constrain which executable is selected. This matters especially for a root service launched with a writable or unexpected PATH; exploitation requires control of that environment/path, not just ordinary unauthenticated API access.

**Improve:** Return explicit, redacted errors for required environment/secret failures. Bound file sizes and verify secret-file ownership/permissions where appropriate. Resolve executables against an explicit trusted PATH or require absolute paths for privileged execution. Document that administrators selecting `run_as` can still delegate daemon-readable secrets and that this is not a multi-user permission boundary.

**Verify:** Missing/unreadable/malformed secret references; duplicate environment precedence; an untrusted executable earlier in daemon PATH; root-to-user execution.

### F12 — P1 for remote exposure, P2 locally: Harden transport, credentials, and slow-client behavior

**Evidence:** `internal/api/api.go:129-156`, `:173-202`, `:553-618`, `:669-691`; `internal/daemon/daemon.go:109-110`; `internal/logstore/logstore.go:669-692`.

- The server accepts arbitrary bind addresses and serves plaintext HTTP. A non-loopback deployment without an external secure tunnel/proxy exposes an administrative bearer credential and command/configuration traffic in transit.
- The initial token is emitted through `slog.Warn`. This intentionally documented bootstrap flow also puts a root-equivalent credential into journals/log collectors with potentially broader access and retention.
- Write timeouts are disabled for all endpoints, not only SSE. There are no connection/SSE admission limits or per-write streaming deadlines. A non-reading client can pin a handler/socket; public assets make some slow-reader pressure possible without authentication.
- Raw download and SSE paths ignore write errors, and log reads use `context.Background()`. A disconnected client can leave avoidable database/decompression work running.

**Improve:** Default-deny accidental non-loopback HTTP or require explicit insecure opt-in with documented TLS/tunnel deployment. Deliver bootstrap credentials through a restricted one-time channel instead of normal structured logs. Set normal-route write budgets; use `http.ResponseController` write deadlines around streaming writes and bounded SSE admission. Return on write/flush errors and propagate request contexts into storage reads. Mark token/API responses `Cache-Control: no-store` where sensitive.

Do not replace streaming with a blanket short timeout that breaks legitimate long-lived SSE or waited triggers. Rate-limit failed authentication as defense in depth, not as a substitute for transport security or high-entropy tokens.

**Verify:** Slow readers, abandoned downloads, connection saturation, token handling in service logs, and a documented secure remote deployment.

### F13 — P2: Metrics and history reads can block execution persistence

**Evidence:** `internal/store/store.go:59-82`, `:126-159`, `:365-505`, `:543-567`; `internal/api/api.go:266-282`; `internal/logdb/logdb.go:137-157`; `internal/logstore/logstore.go:595-667`.

Each database has one connection. This is simple and safe for SQLite writers, but large read operations monopolize it:

- Global run listing lacks a leading `queued_us` index; the existing `(job, queued_us)` index primarily serves per-job listing.
- Metrics scans candidate runs, loops across up to 288 buckets for each row, retains duration arrays, and repeatedly sorts for percentiles: approximately O(runs × buckets) plus sorting. This delays other users of the metadata connection.
- Retention scans/sorts terminal runs by definition without a matching definition/time index.
- Job listing performs one schedule lookup per scheduled definition.
- Log archive decompression runs inside the row callback while its sole connection is held. File-tier reads start from the oldest chunk each page, decompressing already-consumed history again.

**Improve in order:**

1. Measure query plans and latency on realistic retained history; add targeted indexes for global time listing, retention, and metrics predicates.
2. Replace per-row bucket loops with interval/difference-array accumulation; sort each duration set once. Consider cached/preaggregated metrics when exact raw-history recomputation is unnecessary.
3. Batch schedule-state loading and use chunk sequence indexes for file-tier reads.
4. Move bounded decompression work outside long-lived DB cursors. Introduce read-only connections only if measurements justify it; retain controlled writer concurrency and apply connection-local PRAGMAs to every connection.
5. Propagate deadlines and expose DB wait statistics. Do not simply increase the pool size and assume WAL removes all contention.

**Verify:** Concurrent metrics/download traffic while measuring trigger latency, scheduled-fire lateness, terminal-state persistence latency, allocations, and `sql.DB.Stats().WaitDuration`.

### F14 — P2: Runtime failures can leave misleading run states and readiness

**Evidence:** `internal/executor/executor.go:185-319`; `internal/store/store.go:336-342`; `internal/api/api.go:153-156`, `:205-215`.

Log pump failures are only consumed after process wait completes. If a pump exits on storage failure while a child continues writing, its unread pipe can fill and stall a no-timeout job indefinitely. Terminal-state persistence failure is logged once; executor cleanup still removes the active entry and closes its completion channel. The database can remain `running` until daemon restart, and worker supervision gives up when it observes a nonterminal row.

Guarded `StartRun`/`FinishRun` updates do not verify affected-row counts, so a no-op transition can be reported as success. Listener `Serve` errors are ignored. Readiness is a boolean set at startup, not an indication of storage failure or stopped runtime components.

**Improve:** Observe process, context, and log-pump failures together. Choose an explicit disk-full policy: fail/stop the run or continue draining while flagging log loss. Retry/reconcile final-state writes under a bounded policy; expose unresolved transitions. Validate affected-row counts. Supervise listener errors and publish meaningful degraded-state metrics/readiness without making health probes expensive.

**Verify:** Disk full during output, temporary database write failure at completion, zero-row transitions, failed listener, and a pump error before process exit.

### F15 — P2: Retention leaves indefinitely growing metadata and deleted-definition history

**Evidence:** `internal/daemon/daemon.go:231-265`; `internal/store/store.go:126-159`, `:161-181`, `:238-256`, `:316-330`; `internal/config/config.go:44`.

Retention enumerates only non-deleted definitions. Runs belonging to soft-deleted definitions therefore stop receiving normal run retention. Definition revisions, audit rows, and expired idempotency rows have no cleanup path; `audit_keep` is unused. Repeated edits can store full definition bodies in both revisions and audit records indefinitely. Errors during candidate selection/deletion are frequently ignored.

**Improve:** Base retention on stored runs/definition lifecycle, including deleted definitions. Establish independent metadata/audit/revision/idempotency policies, preserve required replay/audit windows, and delete in bounded batches. Report cleanup errors and backlog size. Document backup/restore of **both databases and buffer files**; copying only `minicron.db` does not preserve log history.

**Verify:** Delete a high-volume definition, run maintenance, and confirm the documented history policy; repeat imports/edits over a simulated retention window; inject partial cross-database deletion failures.

### F16 — P2: Reload and registry mutations do not consistently match their API promises

**Evidence:** `internal/daemon/daemon.go:169-216`; `internal/api/api.go:306-364`, `:684-710`; `internal/store/store.go:126-143`, `:238-314`.

- Reload checks only `server.bind` for restart requirements. Changing `server.unix_socket`, concurrency, or `logs.max_line` can report success while the live server/executor keeps old settings. Alerts/config are replaced before reconciliation succeeds, so reload is not fully atomic.
- Definition mutation commits before reconciliation using the HTTP request context. Cancellation after commit can leave runtime stale until another reconcile.
- Soft deletion preserves the global UNIQUE name; recreating a deleted name attempts an insert and fails. Decide explicitly whether names are reserved forever or reusable.
- `SetEnabled` changes neither revision/hash/spec nor audit history. Optimistic concurrency and historical definition hashes therefore do not fully represent enable/disable mutations. Invalid `If-Match` parses to zero, bypassing revision checking.

**Improve:** Classify settings as live/restart-only and reject unsupported live changes. Preflight reloads, publish coherent runtime generations, and reconcile committed registry generations independently of client cancellation. Define name-reuse semantics and align indexes/upserts. Make enabled-state changes versioned/audited, or explicitly separate runtime state from immutable definition identity. Reject malformed preconditions.

**Verify:** Cancel immediately after commit, fail reconciliation, change each restart-only option, delete/recreate a name, enable concurrently with an editor save, and submit malformed `If-Match`.

### F17 — P2: Alerts are best-effort and particularly lossy during outages

**Evidence:** `internal/alerts/alerts.go:50-59`, `:88-141`, `:179-201`.

A single goroutine sends deliveries from a 256-item in-memory queue. A slow provider can block every channel; queue overflow drops alerts, and failed sends are not retried. Process crashes lose queued notifications. `Close` timing out does not cancel ongoing delivery work or the remaining queue.

**Improve:** If alert delivery is operationally important, persist a small outbox with run completion, use bounded per-provider concurrency, retry transient failures with jitter and `Retry-After` handling, and record attempts/dead letters. Otherwise explicitly document best-effort semantics and expose sent/failed/dropped counts. Bound and cancel shutdown work, drain bounded response bodies for connection reuse, and validate provider success responses.

**Verify:** Provider timeout/429/5xx, queue saturation, one slow channel alongside a healthy channel, daemon restart, and shutdown during delivery.

### F18 — P2: Release automation lacks security and correctness gates

**Evidence:** `.github/workflows/release.yml`; `scripts/release.sh`; `Makefile`; `go.mod`.

The checked-in GitHub workflow builds/uploads releases without running tests, race checks, vet, vulnerability checks, or contract checks. It uploads checksums but does not invoke the signing performed by the separate release script. Actions use mutable major-version tags and workflow-level write permissions. Only the release workflow was present in `.github/workflows` during this review.

**Improve:** Gate release on fresh tests/vet and an appropriate race-test job; run a pinned `govulncheck`, dependency review, and generated-contract consistency checks. Test the shipping `CGO_ENABLED=0` configuration. Pin actions to reviewed commit SHAs, scope write permissions to publishing, and generate verifiable artifact provenance/signatures. Establish a supported Go/security-patch update policy and dependency-upgrade cadence.

No CVE claim is made here: dependency versions were inventoried, not checked against a live advisory database.

## Additional improvements

- **P2 — Validate log run IDs at the HTTP boundary.** `api.log/raw/stream` pass path values directly into `logstore.Read`, which uses `filepath.Join` and `filepath.Glob` (`internal/api/api.go:533-579`, `internal/logstore/logstore.go:595-629`). Require a canonical run UUID and verify the run exists; avoid glob metacharacters or encoded path components influencing filesystem selection. This is defense in depth within an already administrative API, not a demonstrated unauthenticated file-read exploit.
- **P2 — Serialize token rotation.** Database writes and `setTokenHash` are separate (`internal/api/api.go:114-125`). Concurrent local rotations can publish in-memory hashes in a different order from persisted hashes. Serialize the complete operation; test the active token before and after restart.
- **P3 — Bound CLI requests and reuse clients.** `cmd/minicrond/main.go:268-307` creates transports repeatedly and supplies no total request timeout. Reuse a client, set operation-appropriate deadlines (accounting for waited triggers), and cap error bodies. Consider sanitizing terminal control sequences from job logs by default, with an explicit raw mode.
- **P3 — Cache embedded assets and ETags.** `internal/api/api.go:712-740` rereads and hashes immutable asset bytes on every request, including requests that become 304s. Precompute these at initialization. This is a straightforward optimization, not currently a measured bottleneck.
- **P3 — Strengthen OpenAPI contracts.** `internal/api/contract.go:22-35` lists routes but does not describe the real request/response/authentication contracts in detail. Generate/validate these from typed handlers or shared schemas so clients can rely on them.
- **P3 — Harden privileged filesystem setup.** Validate ownership and avoid symlink following for sensitive paths before truncating lock/config files, especially when root uses an operator-selected data directory (`internal/daemon/daemon.go:362-379`, `cmd/minicrond/main.go:102-130`). Keep the existing private-directory assumption explicit; use exclusive creation for initialization.

## Performance baseline and measurement plan

Existing microbenchmarks on this audit host:

| Benchmark | Time | Allocations |
|---|---:|---:|
| `BenchmarkNextFire` | 636.5 ns/op | 576 B/op, 21 allocs/op |
| `BenchmarkFrameEncoding` | 981.2 ns/op; reported 1043.67 MB/s | 10,676 B/op, 2 allocs/op |

These are not service throughput claims. The schedule benchmark covers one UTC expression; it does not represent DST transitions, thousands of definitions, or downtime catch-up. The log benchmark uses repeated 1 KiB zero payloads and does not exercise the full pipe/archive/HTTP path or representative compression ratios.

Add workload benchmarks before major architectural changes:

1. **Scheduler:** 100/1,000/10,000 definitions; synchronized due times; UTC and DST zones; wall-clock changes and long downtime. Record CPU, allocations, admission latency, and p99 fire lateness. Cache parsed schedules/timezones and avoid rewriting unchanged `next_fire` every 30 seconds before considering a shared timer heap.
2. **Logs:** sparse output, incompressible output, newline-free streams, default maximum lines, many simultaneous workers, archive handoff, and slow subscribers. Measure RSS, CPU, compression/fsync latency, loss/gap counters, and disk growth. Benchmark zstd concurrency settings rather than defaulting each writer to unrestricted CPU use.
3. **SQLite/API:** realistic retained history under concurrent dashboard metrics, downloads, maintenance, and triggers. Capture query plans and CPU/heap/block/mutex profiles; keep profiling endpoints private.
4. **Reliability:** run deterministic fault-injection and lifecycle tests alongside throughput tests. Faster code that duplicates runs, loses logs, or weakens durability is not an improvement.

## Suggested implementation order

1. **Correctness and containment:** F01–F08, including worker ownership, idempotent admission, DST regression, shutdown joining, and close-error preservation.
2. **Security and operational limits:** F09–F12 plus strict IDs, serialized token rotation, secure deployment/bootstrap guidance, and explicit storage budgets.
3. **Durable runtime behavior:** F14–F17; reconcile failures, complete retention, honest reload semantics, and alert delivery policy.
4. **Measured optimization and continuous assurance:** F13/F18, realistic benchmarks, fault-injection CI, vulnerability scans, and signed releases.

Retain the project's current simple design where it works. The highest-value improvements are tighter lifecycle ownership, bounded resources, explicit durability contracts, and tests at failure boundaries—not a broad rewrite or additional abstraction layers.
