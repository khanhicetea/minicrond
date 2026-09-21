# 03 — System Architecture

Status: Draft

## One binary, many roles

```
┌─────────────────────────────  minicron (single process) ─────────────────────────────┐
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

- **Config Loader** — reads strict daemon settings from `minicron.toml`.
  Definition files are accepted only by the explicit import flow (`05`).
- **Registry** — SQLite is the authoritative definition set. The UI, API,
  and explicit TOML import edit it; the scheduler and supervisor read it.
  In system mode, entries are scoped by an owning user (`17`).
- **Scheduler** — computes next fire times per job (in that job's tz),
  wakes on the earliest deadline, enqueues triggers. Owns overlap/queue
  admission decisions before a run is created.
- **Supervisor** — for workers: keeps declared instances alive, applies
  restart policy/backoff, tracks health, orders boot by `priority` and gates
  on `depends_on` (boot ordering only, never a DAG).
- **Executor Pool** — spawns actual OS processes: builds env, resolves
  `run_as`, creates a new process group, enforces timeout via the stop ladder, and
  collects exit status. One goroutine set per run; bounded global job
  concurrency. Per-child umask/rlimits require the v0.2 launcher protocol.
  Deliberately daemonized descendants can escape process-group control.
- **LogSink** — append-only, tagged-line stream per run; `FileSink` default,
  `S3Sink` optional (`09`).
- **Run Store** — committed SQLite lifecycle writes. Callers await commit
  acknowledgement before spawning a process or reporting API success; crash
  recovery marks unobservable active transitions `interrupted`.
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
- Non-root daemon: `run_as` is disabled; jobs and workers always run as the
  daemon user. Any configured `run_as` fails validation with an explanatory
  error (not a runtime surprise).
- The HTTP listener may drop to an unprivileged port; the process keeps root
  only if `run_as` needs it. Documented hardening: bind `127.0.0.1` by
  default.

## Startup sequence

1. Parse CLI/env; locate config (see `04`).
2. `flock` the data dir (`minicron.lock`) — second instance aborts with the
   holder's PID.
3. Open SQLite (WAL), run migrations (forward-only; newer-schema-than-binary
   is a hard error with version guidance).
4. Load and validate daemon settings; load definitions from the registry.
5. Crash recovery: any run in `pending`/`running` from a previous life →
   `interrupted` (`crash_recovery`); apply missed-run policy per job (`06`).
6. Start supervisor (workers by `priority`, gated by `depends_on`),
  scheduler, HTTP server, sweeper.
7. Emit `ready` (health endpoint flips).

## Reload / reconciliation

Triggered by SIGHUP, `minicron reload`, or `POST /api/v1/daemon/reload`.
Pipeline: load and validate daemon settings → read the SQLite registry →
compute diff classes:

- **added** — start scheduling; workers with `autostart` start now. Jobs do
  **not** fire catch-up for time before they existed.
- **removed** — stop schedules; existing active runs retain their captured
  revision while no new runs are admitted.
- **changed** (content hash differs) — in-flight runs finish under their
  original definition; new runs use the new one; workers restart only if
  their spec changed.
- **unchanged** — no action.

Restart-only settings (bind address, data dir, storage backend) are rejected
in hot reload with a clear "requires restart" error listing the keys.

Definition imports are independently validated and transactionally applied;
a failed import leaves the registry and runtime unchanged.

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

- Source-of-truth model is decided: the SQLite definition registry (`05`);
  system-mode scoping remains per `17`.
