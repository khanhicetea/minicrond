# 00 — Product Vision

Status: Draft

## Elevator pitch

**minicron** is one small binary that replaces `crond` *and* your process
supervisor: define cron **jobs** and long-running **workers** once, and see
every run — exit code, duration, full output — in a dead-simple web UI.
Metadata lives in SQLite. Logs can stay local or be offloaded to S3.
It manages processes, and can drive Docker too.

> One binary. Every run recorded. Nothing disappears at 3 AM.

## Problem

- `crond` gives you nothing: no history, no output capture, no overlap
  protection, no UI. Debugging a failed backup means grepping mail spools.
- `supervisord`/systemd keep services alive but don't schedule, and their
  observability story is weak (or XML-RPC).
- Workflow engines (Airflow, Temporal, …) are the wrong tool for "run this
  script every night and show me the logs": external DBs, multi-process
  deployments, ops overhead.
- Container-native cron runners lose runs when the container restarts and
  can't supervise anything.

## Target users

1. **Solo / small-team infrastructure developers** managing 1–50 servers or
   VPSes: backups, cert renewal, ETL scripts, queue workers.
2. **Self-hosters & homelab operators** who want a UI, not a vim session.
3. **Teams adopting containers gradually**: mixed shell + docker workloads on
   one box, wanting one manager for both.

## Goals

- Single static binary, zero runtime deps, < 30 MB RSS idle, boots in < 1 s.
- Every run is a first-class record: status, exit code, duration, trigger,
  full stdout/stderr, browsable and streamable.
- Definitions follow a **single source of truth per definition** (ADR-3):
  file-imported definitions are file-authoritative — read-only from the UI,
  API, and DB (which store only a reference) — while db-native definitions
  are editable in the web UI; everything is exportable as TOML any time.
- Safe by default: overlap protection, validate-before-apply config changes,
  crash-recoverable state, graceful stop ladder.
- Runs as root or unprivileged; per-job `run_as` privilege dropping. In
  **system mode** (ADR-4), a root daemon lets registered Unix users manage
  and run their own jobs/workers as themselves, driven from the CLI.
- Developer-experience-first UI: trigger a job and watch its log in one click;
  every UI action reproducible via `curl` (copy-as-curl everywhere).

## Non-goals (v1)

- Multi-host clustering / leader election / distributed locking. One daemon
  owns one machine (OQ-15).
- DAG / workflow orchestration with inter-job dependencies beyond worker
  boot ordering.
- Cloud sync, accounts, telemetry, phone-home.
- Windows (Linux + macOS first; WSL works because it's Linux).
- Web-based terminal / arbitrary command execution outside defined jobs.
- A TUI — the web UI and CLI are the only interfaces (ADR-2).

## Positioning vs. existing tools

| | crond | systemd timers | supervisord | minicron |
|---|---|---|---|---|
| Cron scheduling | ✅ | ✅ | ❌ | ✅ |
| Service supervision | ❌ | ✅ | ✅ | ✅ |
| Web UI | ❌ | ❌ | basic | ✅ (simple, DX-first) |
| Run history + output | ❌ | journal | ❌ | ✅ SQLite (+ S3 logs) |
| Config portability | crontab | unit files | INI | TOML import/export + UI editing |
| Runtime deps | libc | systemd | Python | **none** (one binary) |
| Docker pairing | ❌ | ❌ | ❌ | ✅ (driver + sidecar) |

Deliberate differences from the studied reference product (RunWisp): we allow
UI-side editing with SQLite as the registry (it also imports/exports files),
we choose simpler auth, and we treat log storage as a pluggable sink with an
S3 option from day one of the design.

## Roadmap

| Version | Theme | Contents |
|---|---|---|
| **v0.1 — Core loop** | It works, it's observable | Daemon; TOML config + include import; scheduler (5-field cron, tz, overlap skip/queue/replace); jobs + workers with run_as; stop ladder, timeout, retries; SQLite history + audit; local file logs with streaming SSE; token auth + local socket; minimal web UI (dashboard / job / run / live log); REST + OpenAPI; CLI (`run`, `list`, `reload`, `validate`, `export`, `import`, `logs`); `LogSink` trait so backends are pluggable |
| **v0.2 — Operate it** | Production hygiene | System mode: user registration, scoped CLI, per-source reload isolation (`17`); Docker driver + socket pairing (OQ-14); notifications (webhook + inbox); Prometheus metrics; `service install`; log search; backup/restore command; params at trigger time |
| **v0.3 — Scale the logs** | Off-box storage | S3 log backend (ADR-1); compose file import; config editor in UI with schema validation; run retention policies UI |
| **v1.0 — Stable** | Contract freeze | Config/API compatibility guarantees; hardening pass; docs site; packaging (brew, apt repo, docker multi-arch) |

## Success criteria

- From binary download to first triggered job with visible streamed output:
  **< 5 minutes**, no docs required for the happy path.
- `minicron export` → wipe box → `minicron import` reproduces an identical
  working setup (excluding run history).
- Kill -9 the daemon mid-run: after restart, every in-flight run is marked
  `interrupted`, nothing is silently lost, no duplicate firing of missed
  schedules beyond policy.

## Open questions

- Product name (OQ-11), default port (OQ-8), license (OQ-12).
