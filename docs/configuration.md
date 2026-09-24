# Configuration reference

minicrond has exactly two configuration surfaces:

1. **Bootstrap config** — the strict-TOML file the daemon loads at startup
   (`minicron.toml` by default). It configures the *daemon*: server,
   scheduler, storage, logs, alert channels, and job defaults. Unknown fields are errors.
2. **Definitions** — jobs and workers. Their **sole source of truth is the
   SQLite registry**. They are created/edited in the web UI or via the API,
   and can be round-tripped through TOML bundles with `minicrond import` /
   `minicrond export`. Import is an explicit copy; imported files are not
   linked or watched.

The `minicron`-prefixed identifiers (config filename, env vars, data paths,
API paths) are retained compatibility names.

---

## Bootstrap config

```toml
[server]
bind = "127.0.0.1:7423"        # host:port; loopback enforced unless opted out
unix_socket = true             # mode-0600 local socket, token-free CLI access
allow_insecure_remote = false  # required true for a non-loopback bind

[scheduler]
timezone = "UTC"               # IANA name; default schedule timezone
max_concurrent_runs = 32       # global gate across all definitions

[storage]
keep_runs_default = 200        # default per-definition run history length
keep_for_default = 30          # default per-definition run age retention, days
audit_keep = 10000             # audit-log rows kept

[logs]
backend = "file"               # only "file" in v0.1
max_line = 256                 # single log line cap in KiB (1..16384)
worker_flush_interval = 15     # seal+archive cadence for running workers, minutes
db_keep_for = 30               # log-archive age budget, days (rolling)
db_prune_at = "03:30"          # daily prune sweep, local time HH:MM

[defaults]
shell = "/bin/bash"           # optional defaults for newly saved jobs
retry_delay = 15                # seconds

[[alert_channel]]
name = "ops"                   # referenced by definitions' alerts = [...]
type = "telegram"              # only "telegram" in v0.1
bot_token = "env:TELEGRAM_BOT_TOKEN"  # or file:/absolute/path (never inline)
chat_id = "-1001234567890"
disable_notification = false   # true = silent delivery
batch_window = 10              # group runs per channel, seconds (1..3600)
```

### Field notes

Numeric durations and sizes use fixed units, not strings such as `"10s"` or
`"100MiB"`: definition `timeout`, `grace`, job `retry_delay`, worker
`restart_delay` / `healthy_after`, and alert `batch_window` are seconds;
`logs.worker_flush_interval` is minutes; retention `keep_for`,
`storage.keep_for_default`, and `logs.db_keep_for` are days. `log_max` is MiB;
`logs.max_line` is KiB. Cron schedules and `@every 1m` are strings, as is
the `logs.db_prune_at` clock time (`"HH:MM"`). Counts such as `keep_runs`
and `max_concurrent_runs` are unitless.

- `server.bind` — a non-loopback host is rejected at load time unless
  `server.allow_insecure_remote = true`. TCP is plaintext HTTP; use the
  opt-in only behind a TLS-authenticated tunnel or reverse proxy.
- `scheduler.max_concurrent_runs` — 1..1024. When the cap is reached,
  scheduled fires wait; see `catch_up` below for how missed fires are
  handled.
- `logs.db_keep_for` / `logs.db_prune_at` — the SQLite log archive has a
  rolling age budget; chunks older than `db_keep_for` are pruned once a day
  at `db_prune_at` (scheduler timezone).
- `[[alert_channel]].bot_token` — must be `env:NAME` or
  `file:/absolute/path`. The daemon resolves it at startup/reload and fails
  to start if the reference cannot be resolved. Tokens are never stored in
  config or the registry.
- `[defaults]` fills omitted fields when a job is created/updated through the API
  or imported. Explicit job values take precedence; bundle `[defaults]` takes
  precedence over bootstrap `[defaults]`. Values are persisted in the registry;
  changing bootstrap defaults does not change existing jobs until they are
  saved or imported again. Worker definitions do not use bootstrap defaults.
  Zero-valued numeric fields use their defaults (as with bundle defaults).
- Reload: `SIGHUP` or `minicrond reload` re-reads the bootstrap config
  (server bind changes need a restart). Definitions are not reloaded from
  disk — they live in the registry.

Validate any file with `minicrond validate PATH`. The JSON Schema is
`schema/minicron.schema.json` (also: `minicrond schema`).

---

## Job and worker definitions

Jobs have a schedule and run to completion; workers are supervised
long-running processes with restart policies. Both share the definition
schema below.

| Field | Default | Applies to | Description |
|---|---|---|---|
| `name` | — (required) | both | `^[a-z0-9][a-z0-9_.-]{0,99}$`; unique across jobs *and* workers |
| `enabled` | `true` | both | Disabled definitions are skipped by the scheduler |
| `command` | — | both | Shell string, run via `shell -c`. Exactly one of `command`/`argv` |
| `argv` | — | both | Direct exec array (no shell) |
| `shell` | `/bin/sh` | both | Shell used for `command` |
| `schedule` | — | job | 5-field cron (`m h dom mon dow`, `@daily` etc. supported) or `@every 30s` (min 1s). Required for jobs |
| `timezone` | scheduler's | both | Per-definition IANA timezone; DST-safe (wall-clock schedules keep local time across transitions) |
| `catch_up` | `none` | job | `none`: skip missed fires; `latest`: fire only the most recent miss and mark intermediate ones `missed` |
| `on_overlap` | `skip` | job | `skip`: a new fire is recorded `skipped` while one runs; `parallel`: allow concurrent runs |
| `retries` | `0` | job | Number of additional attempts after a failed run (0..1000); each attempt is a new run |
| `retry_delay` | `5` | job | Seconds before each retry (1..86400; 0 uses default 5); constant delay; timeouts and stopped runs are not retried |
| `run_as` | daemon user | both | `user`, `uid`, or `user:group`. **Root daemon only** |
| `working_dir` | daemon cwd | both | Absolute working directory for the process |
| `env_base` | `clean` | both | `clean`: minimal fixed env; `inherit`: start from the daemon's env |
| `env` | `{}` | both | Literal environment map |
| `secret_env` | `{}` | both | Values must be `env:NAME` or `file:/abs/path`, resolved at spawn |
| `env_file` | — | both | Absolute path to a KEY=VALUE file (≤ 1 MiB) |
| `timeout` | `0` (none) | both | Maximum runtime in seconds; a timeout is reported even if the process then exits 0 |
| `grace` | `10` | both | Seconds to wait after `stop_signal` before SIGKILL to the process group |
| `stop_signal` | `SIGTERM` | both | INT, HUP, QUIT, USR1, USR2, TERM, or KILL |
| `success_codes` | `[0]` | both | Exit codes counted as success |
| `keep_runs` | storage default | both | Per-definition run-history length (0 = use `storage.keep_runs_default`) |
| `keep_for` | storage default | both | Per-definition run age retention in days (0 uses `storage.keep_for_default`) |
| `log_max` | `100` | both | Per-run file-buffer budget in MiB (0 = default, 1..1048576); successful archival frees capacity; not an archive quota |
| `log_on_full` | `drop_old` | both | Hot buffer full: `drop_old` or `drop_new` frames |
| `labels` | `{}` | both | Free-form metadata map |
| `alerts` | `[]` | both | Alert channel names to notify on `failed`/`timeout` |
| `autostart` | `true` | worker | Start on daemon boot |
| `restart` | `always` | worker | `always`, `on-failure` (non-zero exit), or `never` |
| `restart_delay` | `5` | worker | Delay between restart attempts, in seconds |
| `max_restart_attempts` | `5` | worker | 1..1000; supervisor gives up after this many consecutive failures |
| `healthy_after` | `30` | worker | Seconds a worker must run to be considered healthy (restart counter resets) |

Pending retry timers are in memory and are canceled on daemon shutdown; retries do not resume after a daemon crash. A retry uses the current enabled definition and retry budget. If it overlaps a running job under `on_overlap = "skip"`, it is recorded as skipped.

Not supported (validation rejects): `priority`, `run_on_start` for jobs —
use `@every` schedules or trigger manually; workers use `autostart`.

### Import bundle format

```toml
[defaults]              # optional: values applied to every entry below
grace = 30              # seconds

[[job]]
name = "backup"
command = "./backup.sh"
schedule = "0 2 * * *"
timezone = "Europe/Berlin"
catch_up = "latest"
alerts = ["ops"]

[[worker]]
name = "bridge"
argv = ["/usr/local/bin/bridge", "--config", "/etc/bridge.conf"]
restart = "on-failure"
```

- One `[[job]]`/`[[worker]]` table per definition; `defaults` fills omitted
  scalar fields.
- Exactly one of `command` or `argv` per definition.
- Workers must not have `schedule`; jobs must.
- Unknown fields are errors; `minicrond import` previews server-side before
  applying, and is idempotent by `name`.

### Runtime environment of a process

With `env_base = "clean"` (default) the process gets a minimal environment
plus: `PATH`, `HOME` (resolved for `run_as`), everything from `env`,
`env_file`, and resolved `secret_env`, plus the metadata variables
`MINICRON_JOB`, `MINICRON_RUN_ID`, `MINICRON_TRIGGER` (`schedule`,
`manual`, `worker`), and `MINICRON_ATTEMPT`.

### Alerts

Failed and timed-out runs are delivered asynchronously to the named
channels; successful, skipped, and manually stopped runs never alert.
Delivery is best-effort: each channel groups failures arriving within its
`batch_window` (measured from the first alert) into one Telegram message.
Long batches are split to fit the provider limit. An in-memory bounded queue
and limited parallelism mean queued alerts do not survive a daemon crash.
Delivery attempts, drops and failures can be inspected under a run's Alerts
section or via `GET /api/v1/runs/{id}/alerts`; `GET /api/v1/metrics/alerts`
reports status counts and outstanding delivery depth. Interrupted deliveries are
marked on daemon restart; they are **not** resent. Test a channel with the
web editor or `POST /api/v1/alert-channels/{name}/test`. Definitions must
reference configured channel names; reload refuses to remove a referenced
channel. Additional providers implement the alert channel interface without
touching run execution.
