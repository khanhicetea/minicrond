# 03 — System Architecture

Status: Draft

## One binary, many roles

```
┌─────────────────────────────  minicrond (single process) ─────────────────────────────┐
│                                                                                       │
│  ┌──────────┐   ┌─────────────┐   ┌──────────────────┐   ┌─────────────────────────┐  │
│  │ Config   │──▶│ Registry    │──▶│ Scheduler        │   │ Supervisor              │  │
│  │ Loader   │   │ (SQLite:    │   │ (next-fire heap, │   │ (worker lifecycles,     │  │
│  │ (toml +  │   │  defs,      │   │  tz-aware ticks) │   │  restart backoff,       │  │
│  │ includes)│   │  revisions, │   └────────┬─────────┘   │  instances)             │  │
│  └──────────┘   │  audit)     │            │ trigger     └──────────┬──────────────┘  │
│                 └─────────────┘            ▼                        ▼                 │
│  ┌────────────────────────────  Executor Pool  ─────────────────────────────────┐    │
│  │  spawn (setuid/setgid, umask, rlimits, process group) · stop ladder · retry  │    │
│  └──────────┬───────────────────────────────┬───────────────────────────────────┘    │
│             │ stdout/stderr tagged lines    │ run state transitions                  │
│             ▼                               ▼                                        │
│  ┌──────────────────┐             ┌───────────────────┐   ┌────────────────────┐     │
│  │ LogSink (trait)  │             │ Run Store         │   │ Event Bus          │     │
│  │ ├─ FileSink      │             │ (runs table,      │   │ (broadcast to SSE, │     │
│  │ └─ S3Sink        │             │  async write      │   │  notifications)    │     │
│  │   [v0.3]         │             │  queue)           │   └────────────────────┘     │
│  └──────────────────┘             └───────────────────┘                              │
│                                                                                       │
│  ┌───────────────────────────────  HTTP Server  ───────────────────────────────────┐  │
│  │  REST /api/v1 · SSE streams · embedded web UI · /healthz /metrics · OpenAPI     │  │
│  └────────────────────────────────────────────────────────────────────────────────┘  │
│        ▲ local Unix socket (peer-auth, CLI)          ▲ TCP (token auth)             │
└────────┴─────────────────────────────────────────────┴──────────────────────────────┘
```

## Components

- **Config Loader** — reads `minicrond.toml` + include globs, applies
  substitutions, validates (strict, positional errors), computes a content
  fingerprint. Produces a desired-state definition set. Never talks to the
  executor directly.
- **Registry** — SQLite holds the definition set: authoritative copies of
  `db`-authority definitions, and references + cached parses for
  `file`-authority ones (`05`, ADR-3). The loader *imports into* the
  registry; the UI and API edit only `db`-authority entries; the scheduler
  and supervisor *read* the registry. In system mode, entries are scoped by
  an owning user (`17`).
- **Scheduler** — computes next fire times per job (in that job's tz),
  wakes on the earliest deadline, enqueues triggers. Owns overlap/queue
  admission decisions before a run is created.
- **Supervisor** — for workers: keeps declared instances alive, applies
  restart policy/backoff, tracks health, orders boot by `priority` and gates
  on `depends_on` (boot ordering only, never a DAG).
- **Executor Pool** — spawns actual OS processes: builds env, resolves
  `run_as`, sets umask/rlimits, creates a new process group (so children die
  with the run), enforces timeout via the stop ladder, collects exit
  status. One tokio task per run; bounded global concurrency.
- **LogSink** — append-only, tagged-line stream per run; `FileSink` default,
  `S3Sink` optional (`09`).
- **Run Store** — async persistence of run state through a bounded queue;
  state changes are idempotent upserts so a crash mid-write loses at most the
  last transition (recovered as `interrupted`).
- **Event Bus** — in-process broadcast: run lifecycle events → SSE clients,
  notification router, metrics recorder.
- **Retention Sweeper** — periodic task enforcing `keep_runs`/`keep_for`,
  log deletion via sink, soft-delete purge, audit cap.

## Process & privilege model (D-5)

- Daemon starts as root or as an unprivileged user. Data dir `0700`, SQLite
  `0600`, socket `0600`.
- Root daemon: jobs/workers with `run_as = "user[:group]"` are spawned via
  `setgroups`/`setgid`/`setuid` after `chdir` to the target working dir;
  `~` in paths resolves against the run-as user's home.
- Non-root daemon: `run_as` must equal the daemon user or be absent —
  otherwise validation fails with an explanatory error (not a runtime
  surprise).
- The HTTP listener may drop to an unprivileged port; the process keeps root
  only if `run_as` needs it. Documented hardening: bind `127.0.0.1` by
  default.

## Startup sequence

1. Parse CLI/env; locate config (see `04`).
2. `flock` the data dir (`minicrond.lock`) — second instance aborts with the
   holder's PID.
3. Open SQLite (WAL), run migrations (forward-only; newer-schema-than-binary
   is a hard error with version guidance).
4. Config load → validate → **import** into registry → diff against live set.
5. Crash recovery: any run in `pending`/`running` from a previous life →
   `interrupted` (`crash_recovery`); apply missed-run policy per job (`06`).
6. Start supervisor (workers by `priority`, gated by `depends_on`),
  scheduler, HTTP server, sweeper.
7. Emit `ready` (health endpoint flips).

## Reload / reconciliation

Triggered by SIGHUP, `minicrond reload`, or `POST /api/v1/daemon/reload`.
Pipeline: load + validate (reject whole reload on any error) → import to
registry → compute diff classes:

- **added** — start scheduling; workers with `autostart` start now. Jobs do
  **not** fire catch-up for time before they existed.
- **removed** (in registry, not in any import source and not db-authority)
  — stop schedules, kill active runs per policy, soft-delete definition
  (restorable; see OQ-18).
- **changed** (content hash differs) — in-flight runs finish under their
  original definition; new runs use the new one; workers restart only if
  their spec changed.
- **unchanged** — no action. Definition *source-path* moves (same content,
  different file) MUST NOT restart workers.

Restart-only settings (bind address, data dir, storage backend) are rejected
in hot reload with a clear "requires restart" error listing the keys.

Each import source validates **independently**: an invalid source is
rejected alone, leaving every other source (including other users' sources
in system mode) to apply normally (`17`).

## Concurrency & footguns

- Scheduler tick, run spawns, and log pumps never block on SQLite: DB access
  goes through the async write queue / read pool.
- Global `max_concurrent_runs` (default 32) enforced at admission; breach →
  job's queue policy or `queue_full` end reason.
- Every spawned run is in its own process group; daemon shutdown walks
  groups through the stop ladder, waits `shutdown_timeout` (default 15 s),
  then marks the rest `interrupted`.

## Failure posture

- `kill -9` at any point is recoverable: WAL SQLite + immutable log files +
  recovery pass at boot. Never resume a killed run.
- Log write failure mid-run: run continues, gap recorded via `system` line;
  persistent sink failure escalates to a `logs.unwritable` event (and
  optionally kills the run: `log_on_full = "kill"`, see `09`).

## Open questions

- OQ-13: opt-in file-watch auto-reload vs manual-only reload.
- Source-of-truth model is decided: provenance-locked authority (ADR-3,
  spec `05`); system-mode scoping per `17`.
