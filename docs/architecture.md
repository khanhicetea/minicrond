# Architecture

minicrond is a single Go binary: scheduler, executor, supervisor, storage,
API, embedded web UI, and alerting in one process. One daemon owns one data
directory (enforced by an flock on `minicron.lock`).

```
                    ┌────────────────────────────────────────────┐
        TOML ──────▶│ daemon (bootstrap config, reload, secrets) │
   import/export ─▶│                                            │
                    │  scheduler ──▶ executor ──▶ process group  │
   SQLite registry◀─┤      │            │                        │
  (definitions)     │      ▼            ▼                        │
                    │   store ◀──── logstore ──▶ logdb           │
                    │  (runs)      (live zstd    (SQLite         │
                    │               buffers)     log archive)    │
   bearer/TCP ◀────▶│  api (REST + SSE) + embedded SPA           │
   unix socket ◀───▶│                                            │
                    │  alerts (async dispatcher → Telegram)      │
                    └────────────────────────────────────────────┘
```

## Packages

| Package | Responsibility |
|---|---|
| `internal/daemon` | Composition root: config load, wiring, lifecycle, reload, crash recovery |
| `internal/config` | Strict-TOML bootstrap config + import-bundle parsing and validation |
| `internal/scheduler` | Cron evaluation (`@every`, 5-field, DST-safe wall-clock), catch-up, overlap, concurrency gate |
| `internal/executor` | Process spawning (clean/inherit env, secrets, run_as), process-group control, timeout/grace kill |
| `internal/supervisor` | Worker supervision: autostart, restart policies, healthy_after |
| `internal/store` | SQLite registry + run history + audit (guarded state transitions) |
| `internal/logstore` | Live-tier tagged, zstd-compressed chunk files with fsync-before-ack |
| `internal/logdb` | Archive tier: separate SQLite log database, retention sweeps |
| `internal/api` | HTTP API, SSE streaming, embedded SPA assets, auth (bearer + unix peer) |
| `internal/sqlite` | Shared SQLite connection setup (writer + read pool) and schema identity checks |
| `internal/model` | Definition, run, worker and alert-channel types shared by every package |
| `internal/procstats` | Process CPU/RSS sampling for the Monitor page (Linux procfs) |
| `internal/trustedpath` | Validation of `scheduler.trusted_path` executable directories |
| `internal/fault` | Panic errors with stack traces at explicit operation boundaries |
| `internal/alerts` | Async, bounded, best-effort alert dispatch (Telegram; interface for more) |
| `cmd/minicrond` | CLI + daemon entrypoint; embeds the config JSON Schema |
| `cmd/openapi-gen` | Regenerates the checked-in OpenAPI contract |
| `web/` | React 19 SPA source (embedded at build time into `internal/api/assets`) |

## Run lifecycle

```
queued ──▶ pending ──▶ running ──▶ succeeded | failed | timeout | stopped | interrupted
  │           │                    (terminal)
  │           └──▶ failed(start_error) | stopped | interrupted | skipped | missed
  └──▶ skipped (queue_expired, queue_full, …) | stopped (queue_cancelled)
```

| From | Allowed next states |
|---|---|
| queued | pending, skipped, stopped |
| pending | running, failed(start_error), stopped, interrupted, skipped, missed |
| running | succeeded, failed, timeout, stopped, interrupted |
| terminal | none |

- Terminal vocabulary is non-overlapping and enforced by guarded UPDATEs
  in the store; recovery marks unobservable active runs `interrupted`.
- A timeout stays `timeout` even if termination yields exit code 0; an
  operator stop stays `stopped`.
- History follows execution ([ADR-16](adr/0016-execution-first.md)): a run is
  written synchronously while the metadata database is healthy and by a
  bounded background writer otherwise. Only idempotent API triggers, the
  durable queue and jobs with `on_history_unavailable = "skip"` require the
  database before starting.

## Behavioral invariants

- **Registry, import, reload.** A valid hash-bound TOML import is one database
  transaction, then reconciliation; an invalid or duplicate bundle entry leaves
  registry and runtime unchanged. An API/UI edit increments the revision, then
  reconciles. A daemon settings reload validates settings and reconciles the
  runtime from SQLite.
- **Scheduling and workers.** `@every` keeps a database anchor. A schedule hash
  change resets catch-up history. `catch_up = "none"` records one summarized
  `missed` run; `latest` admits one. `on_overlap = "skip"` records the decline.
  Job capacity is independent from workers. A worker operator hold suppresses
  `always` restart.
- **Logs.** Frames carry version, stream, flags, a stable sequence (daemon
  ingestion order), a Unix-microsecond timestamp, a uint32 payload length and
  the original bytes. Partial final lines, invalid UTF-8 and truncation are
  flagged. stdout/stderr drain to EOF before close; a descendant holding an
  inherited pipe gets a 5-second post-exit drain, then the pipe is force-closed
  with a system annotation.
- **Idempotency.** `Idempotency-Key` on trigger is scoped by principal and the
  stored request hash binds the target: the same key and target return the
  original run (200), a different target is 409.
- **Retention.** Only terminal runs are candidates and the newest terminal run
  of each definition is always kept. A run is deleted when either the count or
  the age limit is exceeded; logs are deleted first, and metadata is kept when
  log deletion fails.

## Failure handling

Expected failures return errors with operation context and preserved causes.
The owning CLI, HTTP, or background-operation boundary records diagnostics;
HTTP internal failures return a generic response. Database result and cleanup
errors are checked instead of silently treating failed operations as success.

Panic recovery is scoped to owned operations. An execution panic closes pipes,
reaps the child, finalizes its logs, and attempts to persist `failed` with
`internal_error`. Completion and alert callback panics retain stack traces;
a completion callback failure does not prevent job retries. A failed scheduling
or supervision loop stops and logs its stack. A panicking maintenance loop is
logged and restarted with capped backoff ([ADR-16](adr/0016-execution-first.md));
unexpected HTTP listener failures cause the daemon to shut down and return an error.
Recovery cannot repair corrupt state or stop a callback that ignores its context.

After a successful spawn, the executor attempts the `process started as …`
system line before admitting stdout/stderr frames, independently of the running-state
database write. Both pumps share a startup gate; no viewer queues or payload
buffers are added. The gate waits at most five seconds for a stalled system write,
then capture proceeds with best-effort ordering. This bounded pause uses the
existing pipe buffers, not accepted-output buffering in RAM. Timeout and stop
supervision never waits on the gate; failed spawns emit only a start-error line.

Timeouts and stop requests remain active after a child closes either output
stream. The executor owns the process group for the run's lifetime and kills
remaining descendants before completing the run. Descendants holding output
pipes get at most five seconds to drain. Shutdown blocks new API work and worker
starts, cancels scheduled retries, and forces termination when its run deadline
expires, even if the definition has a longer grace period; forced runs then get
a short bounded join to persist their outcome before the databases close.
Execution comes first ([ADR-16](adr/0016-execution-first.md)): a run row,
running transition or terminal state that the metadata database cannot take
never prevents, kills or delays a run. The outcome is kept in memory; alerts,
retries and worker restart decisions use it at once, and a single background
history writer (at most 1024 runs; beyond that the run's history is dropped and
counted) stores the row, running and terminal states in order once storage
recovers. While storage is known to be failing, launches skip the database. The
scheduler keeps each job's schedule position in memory, so failed
schedule-state writes do not stop firing. API callers should still verify the
stored status before treating a run as terminal.
Pending retries live in one bounded scheduler (`queue.max_pending_retries`), not
a goroutine and timer each. A job trigger (`schedule`, `manual`, `retry`) that
finds `max_concurrent_runs` full is persisted in a bounded durable queue
(status `queued`, [ADR-9](adr/0009-durable-execution-queue.md)) and started later
by one drain goroutine that is armed only while the queue is non-empty; when the
queue is full or disabled it is recorded `skipped` with `queue_full` (manual
triggers get an explicit 429 instead).

Definitions and run records are copied at asynchronous ownership boundaries.
Configuration rejects durations that would overflow, and invalid UID/GID values
cannot silently become a different execution identity. Failed log finalization
and invalid orphan indexes preserve buffers for repair or another archive attempt.

## Definitions: SQLite is the authority

Browser, CLI, and API edits write the registry directly; TOML is an
explicit import/export format only (previewed, hash-checked, idempotent by
name). There is no watched config file for definitions — this removes an
entire class of drift problems (ADR-5). Main-config `[[init]]`, `[[job]]` and
`[[worker]]` entries are file-owned, applied on start and reload, and
read-only in the UI.

## Alert channels

SQLite owns alert settings and literal provider credentials; bootstrap TOML no
longer accepts channels. Startup and UI/API mutations load the full registry
into memory (at most 100 channels). Delivery performs no per-alert DB lookup or
config polling. Registry swaps leave queued batches on their original immutable
transport. API responses/audits redact credentials; metadata backups contain
secrets. Optional reference removal and channel deletion are one transaction.
See [ADR-13](adr/0013-database-alert-channels.md).

## Storage

Two SQLite databases, never merged:

- `minicron.db` — definitions, alert-channel settings and credentials, runs,
  audit, the durable execution queue.
- `minicron-logs.db` — the long-term log archive with a rolling age budget
  (`logs.db_keep_for`) and optional size budget (`logs.db_max_size`), pruned
  daily at `logs.db_prune_at`.

Each file carries a SQLite `application_id` and a schema version (both 1 as of
0.6.0). A new file gets the full schema in one transaction; an existing file
must match exactly. A newer schema, or a pre-0.6 development database (no
`application_id`), is refused, never upgraded in place.

Each database has one writer connection (transactions use `BEGIN IMMEDIATE`)
and a small read-only pool. Run listings, metrics and log reads use the pool,
so they never queue behind run transitions or archival.

Live logs take the fast path: every accepted frame is a tagged, zstd-
compressed chunk file write before the next line is read (daemon-crash-safe);
fsyncs are grouped on a shared timer (`logs.sync_interval` /
`logs.sync_max_dirty`) unless `logs.durability = "frame"`, seals and pipe EOF
included. Finished runs are sealed and archived into the
log DB in the background, off the completion path, and buffers are deleted; workers
checkpoint every `logs.worker_flush_interval`. Small chunks become blobs, short
runs are committed in batches, and chunks of 64 KiB or more are renamed into
`log-archive/` with a row pointing to them instead of being copied (ADR-20). Crash-orphaned buffers are
salvaged at startup up to the last intact frame. Reads merge the archive and
the buffer files; no payload is kept in memory for viewers (ADR-6, ADR-8).
Archive cursor reads seek by frame sequence. Log disk use is bounded by a
disk budget and a free-space headroom that outrank retention age, reclaiming
the oldest logs first while keeping each running worker's recent tail (ADR-10).

## Web UI

A React 19 SPA (React Compiler, wouter, daisyUI 5, TanStack Query) built
ahead of time and embedded into the binary — deploying the UI means
deploying the daemon. TypeScript model types are generated from the Go
model with Tygo (`make generate-types`). The UI shell is public; every
`/api/` call uses a bearer token over TCP. A Unix-only deployment may
serve the same UI through an authenticated reverse proxy to the private
socket; the UI then makes token-free requests, and the proxy owns browser
authorization. See [ADR-7](adr/0007-unix-only-and-proxy-ui.md).

## Design records

All accepted decisions are in [`adr/`](adr/) (ADR-5 to ADR-20; earlier numbers
were retired drafts). Start with:

- [ADR-5: foundations](adr/0005-v01-foundations.md) — stack, SQLite
  authority, run vocabulary, process guarantees
- [ADR-6: hybrid log storage](adr/0006-hybrid-log-storage.md) — file
  buffer + separate SQLite archive
- [ADR-8: background-first trade-offs](adr/0008-background-first-trade-offs.md) — batched fsync, capture failure, disk budget, durable queue, small-server tuning
- [ADR-16: execution first](adr/0016-execution-first.md) — runs never wait for history storage; PID 1 init
- [ADR-20: log archive write path](adr/0020-log-archive-write-path.md) — file chunks, batched archive commits, deferred seal syncs
- [`releases/`](releases/) — release notes
