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
| `internal/fault` | Panic errors with stack traces at explicit operation boundaries |
| `internal/alerts` | Async, bounded, best-effort alert dispatch (Telegram; interface for more) |
| `cmd/minicrond` | CLI + daemon entrypoint; embeds the config JSON Schema |
| `cmd/openapi-gen` | Regenerates the checked-in OpenAPI contract |
| `web/` | React 19 SPA source (embedded at build time into `internal/api/assets`) |

## Run lifecycle

```
pending ──▶ running ──▶ succeeded | failed | timeout | stopped | interrupted
   │                    (terminal)
   └──▶ skipped | missed
```

- Terminal vocabulary is non-overlapping and enforced by guarded UPDATEs
  in the store; recovery marks unobservable active runs `interrupted`.
- A timeout stays `timeout` even if termination yields exit code 0; an
  operator stop stays `stopped`.
- Lifecycle rows commit **before** spawning (or before returning success),
  so history never loses a run the daemon actually started.

## Failure handling

Expected failures return errors with operation context and preserved causes.
The owning CLI, HTTP, or background-operation boundary records diagnostics;
HTTP internal failures return a generic response. Database result and cleanup
errors are checked instead of silently treating failed operations as success.

Panic recovery is scoped to owned operations. An execution panic closes pipes,
reaps the child, finalizes its logs, and attempts to persist `failed` with
`internal_error`. Completion and alert callback panics retain stack traces;
a completion callback failure does not prevent job retries. A failed scheduling
or supervision loop stops and logs its stack. Maintenance panics and unexpected
HTTP listener failures cause the daemon to shut down and return an error.
Recovery cannot repair corrupt state or stop a callback that ignores its context.

Timeouts and stop requests remain active after a child closes either output
stream. The executor owns the process group for the run's lifetime and kills
remaining descendants before completing the run. Descendants holding output
pipes get at most five seconds to drain. Shutdown blocks new API work and worker
starts, cancels scheduled retries, and forces termination when its run deadline
expires, even if the definition has a longer grace period; forced runs then get
a short bounded join to persist their outcome before the databases close.
Terminal persistence first retries inline for five seconds, then hands off to a
single background finalizer (at most 256 runs; beyond that the completing run
retries inline and keeps its concurrency slot) that keeps retrying until storage
recovers or the daemon stops, and only then sends alerts and schedules retries. Callers must still
verify the stored status before treating a run as terminal. Scheduling and
worker supervision loops retry storage failures with capped backoff instead of
stopping; an occurrence that could not be triggered is handled by `catch_up`.
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
entire class of drift problems (ADR-5).

## Storage

Two SQLite databases, never merged:

- `minicron.db` — definitions, runs, audit. Schema-versioned; binaries
  refuse newer schemas.
- `minicron-logs.db` — the long-term log archive with a rolling age budget
  (`logs.db_keep_for`) and optional size budget (`logs.db_max_size`), pruned
  daily at `logs.db_prune_at`.

Each database has one writer connection (transactions use `BEGIN IMMEDIATE`)
and a small read-only pool. Run listings, metrics and log reads use the pool,
so they never queue behind run transitions or archival.

Live logs take the fast path: every accepted frame is a tagged, zstd-
compressed chunk file write before the next line is read (daemon-crash-safe);
fsyncs are grouped on a shared timer (`logs.sync_interval` /
`logs.sync_max_dirty`) unless `logs.durability = "frame"`. Finished runs are sealed and archived into the
log DB in the background, off the completion path, and buffers are deleted; workers
checkpoint every `logs.worker_flush_interval`. Crash-orphaned buffers are
salvaged at startup up to the last intact frame. Reads merge the archive and
the buffer files; no payload is kept in memory for viewers (ADR-6, ADR-8).

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

- [ADR-5: v0.1 foundations](adr/0005-v01-foundations.md) — stack, SQLite
  authority, run vocabulary, process guarantees
- [ADR-6: hybrid log storage](adr/0006-hybrid-log-storage.md) — file
  buffer + separate SQLite archive
- [ADR-7: Unix-only and proxy UI](adr/0007-unix-only-and-proxy-ui.md) — optional local transport and browser authentication
- [`specs/`](../specs/) — pre-implementation design drafts and the decision
  log (drafts yield to ADRs and these docs where they conflict)
- [`releases/`](releases/) — release notes with the v0.1 acceptance tables
  and measurements
