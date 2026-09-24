# 01 — Domain Model & Terminology

Status: Draft

Vocabulary is frozen early and never silently renamed (pre-1.0 rename churn is
a documented failure mode of this product category). Rule: past-tense for
terminal states, `max_*` for caps, singular entity names in APIs.

## Entities

### Job
A **run-to-completion** definition: a command with a schedule (cron-like),
triggered by schedule, at daemon start, or manually. Jobs terminate.

- Identity: `name` (unique, `^[a-z0-9][a-z0-9_.-]{0,99}$`, lowercase).
- Kind: `job` (this) or `worker` (below) — one registry, two kinds.

### Worker
An **always-on** definition: a process the daemon keeps alive with restart
policy and backoff. Workers have no schedule; they start at boot (unless
`autostart = false`) and are supervised continuously. Optionally `instances`
parallel copies.

### Definition (spec)
The full declarative description of a job or worker (schedule, command, env,
run_as, policies…). Stored as canonical JSON in SQLite with a content hash.
SQLite is the single source of truth. UI/API edits and explicit TOML imports
produce a new revision and an audit record (`05`). In system mode, definitions
are additionally scoped by an owning user (`17`).

### Run
One execution instance of a job (or one supervised lifetime of a worker
instance). Identity: `run_id`, a **UUIDv7** (time-sortable, index-friendly;
chosen over ULID for ecosystem availability).

A run carries:

| Field | Notes |
|---|---|
| `run_id` | UUIDv7 |
| `job` | name + current definition revision |
| `status` | lifecycle below |
| `trigger` | `schedule` \| `startup` \| `manual` (UI/CLI/API share this) \| `retry` |
| `attempt` | 1 for first try; N for retry chain |
| `parent_run_id` | set for retries |
| `pid`, `exit_code`, `signal` | process facts |
| `started_at`, `ended_at`, `duration_ms` | |
| `end_reason` | taxonomy below |
| `params` | values supplied at trigger time (v0.2) |
| `log_ref` | backend-neutral log location (see `09`) |
| `run_as` | user[:group] actually used |

**Run status:** `pending` (queued) → `running` → one of `succeeded`,
`failed`, `stopped` (operator/policy initiated), `interrupted` (daemon died
mid-run), `skipped` (overlap policy declined), `missed` (recorded, not
executed — see catch-up in `06`), `timeout`.

**End reasons** (orthogonal detail on terminal status): `exit`,
`exit_nonzero`, `signal`, `timeout`, `overlap_skip`, `overlap_replace`,
`queue_full`, `stop_signal`, `sigkill`, `daemon_shutdown`, `crash_recovery`,
`dst_skip`, `start_error`, `log_overflow`.

Only these words. UI/API/docs use them verbatim.

### Schedule
Parsed representation of a job's timing: 5-field cron expression, descriptor
(`@daily`, …), or interval (`@every 90m`). Evaluated in the job's timezone.
Details in `06`.

### Log stream
The captured output of a run: interleaved **tagged lines** (each line knows
its origin: `stdout` | `stderr` | `system`). Written through the `LogSink`
abstraction (file or S3 backend). One immutable sequence per run with
absolute line numbers. Details in `09`.

### Event / Notification
Internal event bus facts (`run.failed`, `worker.fatal`, `disk.low`, …) routed
to channels (in-app inbox, webhook, later SMTP/Slack). Coalesced by
(kind, job) within a window. Details in `16`.

### Audit record
Who changed what, when, through which channel (`file:<path>`, `ui`, `api`,
`cli:<cmd>`, `user:<name>` in system mode), before/after definition
snapshots. Powers UI history and explicit import/export traceability. Stored in
SQLite, capped.

## State machines

```
Run:    pending ──▶ running ──▶ succeeded | failed | timeout | stopped | interrupted
          ├──────▶ skipped
          └──────▶ missed

Worker (per instance):
  stopped → starting → running ⇄ restarting (backoff) → fatal (gave up)
```

- A worker instance's each lifetime is **also** a Run (so it has history,
  logs, exit codes like any job run).
- `fatal` is entered after N consecutive failed starts (unhealthy within
  `healthy_after`); it notifies and stays down until operator restart or
  config change. Manual restart always bypasses backoff.

## Counting rules

- Job retries are separate runs linked by `parent_run_id` with increasing
  `attempt` values. Pending retry timers are in-memory and do not survive a
  daemon restart; durable retry scheduling is future work.
- Each run succeeds when its exit code ∈ `success_codes` (default `[0]`);
  retries are separate runs, not a change to the failed run's status.
- Duration includes process runtime only, not queue wait (queue wait is its
  own column).

## Open questions

- OQ-4: table style in config (`[[job]]` arrays vs `[jobs.<name>]` keyed
  tables) — this doc assumes `[[job]]`/`[[worker]]`.
