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
  (`logs.db_keep_for`), pruned daily at `logs.db_prune_at`.

Live logs take the fast path: every accepted frame is a tagged, zstd-
compressed chunk file write, fsynced before `Write` returns (crash-safe).
Finished runs archive into the log DB and buffers are deleted; workers
checkpoint every `logs.worker_flush_interval`. Crash-orphaned buffers are
salvaged at startup up to the last intact frame. Reads merge all tiers
plus a byte-bounded in-memory tail (ADR-6).

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
