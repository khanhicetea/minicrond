# 07 — Execution & Supervision

Status: Draft

## Spawning a run (local driver)

Order of operations, precisely:

1. **Admission** (scheduler-side; see `06`): overlap policy, queue, global
   concurrency, disk precheck (`min_free_disk`, default 100 MiB → fail fast
   with `start_error` and a `disk.low` event).
2. Persist `pending` run → allocate `run_id` (UUIDv7), open the LogSink.
3. Build environment, layered:
   `env_base` (`inherit` = daemon's env minus `minicron_*`; `clean` =
   `PATH=/usr/bin:/bin`, `HOME`, `TZ`) → `env_file` → `env` → `secret_env` →
   injected context vars:
   `minicron_JOB`, `minicron_RUN_ID`, `minicron_TRIGGER`,
   `minicron_ATTEMPT`, `minicron_LOG_PATH` (file backend), and for worker
   instances `minicron_INSTANCE` (1-based).
4. Resolve identity: `run_as` `user[:group]` (name or numeric; resolved via
   the system user database; `~` in `working_dir` resolves against that
   user's home). Root daemon required for dropping — otherwise validation
   error up front (D-5). In system mode, user-owned definitions are locked
   to the owner's uid/gid — any other `run_as` in a user scope is a
   validation error, never a silent override (`17`).
5. Create **process group** (`setsid`): every descendant is killable by the
   daemon, and nothing escapes the stop ladder.
6. Set `umask`, `rlimits` (`limits`: `cpu_seconds`, `memory_bytes`, `nproc`,
   `fsize`) [v0.2], `chdir(working_dir)` (default `/tmp`; missing dir =
   `start_error`).
7. Spawn:
   - single-line `command` → `shell -c command`;
   - multi-line `command` → written to a `0700` temp file under the data dir,
     executed as `shell file`, deleted after exit; commands fail-fast
     (script runs with `-e` semantics).
8. Pump stdout/stderr concurrently into the LogSink as **tagged lines**
   (`stdout` / `stderr` / `system`; see `09`). Oversized lines truncated at
   `logs.max_line` with a `system` marker noting the truncation.
9. Reap exit status → terminal transition + retry evaluation.

`start_error` covers: user/group unresolvable at spawn time, working dir
missing, exec permission, fork failure. It is a terminal, notifiable outcome.

## Stop ladder (uniform for every kill path)

Kill paths: operator stop (UI/API/CLI), timeout, overlap `replace`, reload
removal, daemon shutdown.

```
send stop_signal (default SIGTERM)
  → wait grace (default 10s, per-definition override)
  → send SIGKILL to the process group
  → wait 5s hard; report if unreapable (kernel-level, shouldn't happen)
```

- `grace = 0` means skip straight to SIGKILL.
- Timeout runs get a `system` log line at deadline ("timeout reached,
  stopping").
- Daemon shutdown: walk all active runs through the ladder concurrently,
  bounded by `shutdown_timeout` (default 15 s); unfinished → `interrupted`.

## Timeouts

`timeout` is wall-clock from process start (not queue admission). At
deadline: ladder → final status `timeout` (a flavor of `failed` unless
`success_codes` covers the post-kill exit code, which it normally shouldn't).
Retries apply to timeouts like any failure unless `retry_on_timeout = false`.

## Retries (jobs)

- `retries = N` additional attempts, each a **new run** linked via
  `parent_run_id`, `attempt = k+1`, `trigger = retry`.
- Delay: `retry_delay` scaled by `retry_backoff` — `constant` (d),
  `linear` (d·attempt), `exponential` (d·2^(attempt-1)), capped at 15 min.
- Runs only when the definition at failure time still allows it (a reload
  that lowers `retries` stops the chain).
- The chain's final attempt determines job-level outcome for notifications
  and "last status".

## Worker supervision

- Each `[[worker]]` keeps `instances` (1–64) slots. Each slot is an
  independent supervised lifetime = a run (`name#2` labeling for instances >
  1).
- **Health**: a start is healthy once it stays running `healthy_after`
  (default 30 s). Unhealthy exits are *failed starts*, not crashes.
- **Restart policy**:
  - `always` (worker default): restart on any exit.
  - `on-failure`: restart only on nonzero exit / signal-death.
  - `never`: one shot per explicit start (a service you must babysit).
- **Backoff**: `restart_delay` scaled by `restart_backoff` (same enums as
  retries), reset to zero after a healthy period. Manual restart (UI/API)
  always bypasses backoff and resets the failure counter.
- **Fatal**: after `max_restart_attempts` (default 5) consecutive starts that
  never reach healthy → slot goes `fatal`: no more restarts, `worker.fatal`
  notification, prominent UI state, one-click restart.
- **Boot**: ascending `priority`; `depends_on` gating per `06`. **Teardown**:
  reverse priority, ladder per instance, then dependent shutdown.
- Config change to a worker's command/env/identity restarts it through the
  ladder; cosmetic changes (labels, notify) don't.

## Docker driver [v0.2] (summary; details in `10`)

`driver = "docker"` replaces step 7: the executor shells out to `docker run
--rm --name minicron-<job>-<run_id> --init` with the image, streams
`docker logs -f` into the same LogSink tagged-line pipeline, and maps the
container's exit code. Stop ladder maps to `docker stop -t <grace>` /
`docker kill`.

## Crash recovery

On boot: every run still `pending`/`running` in SQLite → `interrupted`
(`crash_recovery`), exit code null, end timestamp = daemon start estimate.
In-flight **never resumes**. Missed schedule windows then go through
catch-up policy. Workers simply boot fresh (their interrupted lifetimes are
visible in history).

## What this spec intentionally does NOT include

- cgroup-based containment (beyond rlimits) — MAY post-1.0, Linux only.
- CPU/IO priority (`nice`, `ionice`) — trivially addable; not core.
- Arbitrary-argv mode (`command_argv`, no shell) — reserved key, v1.0+; the
  shell form with params (v0.2) covers the injection-safe path meanwhile.

## Open questions

- Should `retry_on_timeout` default be `true` (current draft) — opinions?
- `max_restart_attempts` vs `restart_attempts` naming — the `max_` prefix
  rule says former; confirm vocabulary freeze.
