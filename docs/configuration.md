# Configuration reference

minicrond has two configuration surfaces:

1. **Bootstrap config** — the strict-TOML file the daemon loads at startup
   (`minicron.toml` by default). It configures the *daemon*: server,
   scheduler, storage, logs, alert channels, job defaults, and optional
   config-owned `[[init]]`, `[[job]]`, and `[[worker]]` entries. Unknown fields are errors.
2. **Registry definitions** — jobs and workers created/edited in the web UI or via the API.
   Their source of truth is SQLite. They
   and can be round-tripped through TOML bundles with `minicrond import` /
   `minicrond export`. Import is an explicit copy; imported files are not
   linked or watched. Config-owned definitions are mirrored into SQLite for
   scheduling and run history; the main TOML file remains authoritative for them.

The `minicron`-prefixed identifiers (config filename, env vars, data paths,
API paths) are retained compatibility names.

---

## Bootstrap config

```toml
[server]
bind = "127.0.0.1:7423"        # host:port; loopback enforced unless opted out
tcp_enabled = true             # false: serve only the private Unix socket
unix_socket = true             # mode-0600 local socket, token-free CLI access
allow_insecure_remote = false  # required true for a non-loopback bind

[scheduler]
timezone = "UTC"               # IANA name; default schedule timezone
max_concurrent_runs = 32       # global gate across all definitions

[queue]                        # durable queue for triggers that find the gate full (ADR-9; restart to change)
enabled = true                 # false: over-capacity triggers are skipped (queue_full) as before
max_items = 100                # total queued runs (1..10000)
max_per_job = 25               # queued runs per definition (1..max_items)
max_bytes = 256                # persisted queue payload, KiB (1..65536)
max_age = 900                  # seconds a run may wait before it expires (1..604800)
drain_rate = 5                 # queued runs started per second at most (1..1000)
max_pending_retries = 500      # retries waiting for retry_delay (1..100000)

[storage]
keep_runs_default = 10000      # default per-definition run history length
keep_for_default = 7           # default per-definition run age retention, days
audit_keep = 10000             # audit-log rows kept
synchronous = "full"           # SQLite fsync policy: "full" or "normal" (restart)

[logs]
backend = "file"               # only "file" in v0.1
max_line = 256                 # single log line cap in KiB (1..16384)
worker_flush_interval = 15     # seal+archive cadence for running workers, minutes
db_keep_for = 30               # log-archive age budget, days (rolling)
db_prune_at = "03:30"          # daily prune sweep, local time HH:MM
db_max_size = 0                # log-archive size budget, MiB (0 = none, max 1073741824)
durability = "batch"           # log fsync policy: "batch" or "frame"
sync_interval = 2000           # batch: ms until dirty log buffers are fsynced (100..60000)
sync_max_dirty = 1024          # batch: KiB of unsynced output that forces an fsync (1..65536)
disk_min_free = 512            # keep this much free on the data-dir filesystem, MiB (0 = off)
disk_budget = 0                # cap on archive+WAL+buffers, MiB (0 = none, max 1073741824)
quarantine_keep_for = 0        # delete quarantined log data older than N days (0 = keep)
quarantine_max_size = 0        # cap quarantined log data, MiB (0 = no cap)

[reads]                        # expensive diagnostic reads (restart)
slots = 4                      # JSON log pages, raw downloads and metrics running at once (1..64)
budget = 32                    # MiB of estimated working set shared by those reads (8..4096)
work_timeout = 20              # seconds one request's read/aggregation may run (1..300)

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

[[init]]
name = "prepare"
command = "./prepare.sh"     # runs synchronously on every daemon start

[[worker]]
name = "api"
command = "./start-api.sh"   # supervised after init tasks succeed
restart = "always"

[[job]]
name = "cleanup"
schedule = "0 3 * * *"
command = "./cleanup.sh"
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

- `server.tcp_enabled` — defaults to `true`. Set `false` for a Unix-only
  daemon; `unix_socket` must then remain enabled. `bind` is unused in that
  mode. Changing either listener setting requires a daemon restart.
- `server.bind` — when TCP is enabled, a non-loopback host is rejected at load time unless
  `server.allow_insecure_remote = true`. TCP is plaintext HTTP; use the
  opt-in only behind a TLS-authenticated tunnel or reverse proxy.
- `BASE_PATH` — optional daemon environment variable for serving the API and
  web UI below a URL prefix, such as `/tools/minicron`. It must start with `/`
  and contain URL path segments made of letters, digits, `-`, `_`, `.`, or `~`.
  A trailing slash is accepted. The prefix applies to both TCP and Unix
  socket routes, including `/healthz`, `/readyz`, and `/openapi.json`.
  Set the same variable for local Unix-socket CLI commands, or include the
  prefix in `MINICRON_URL` for HTTP CLI commands. Restart the daemon after
  changing it.
- `MINICRON_ALLOW_IFRAME` — optional daemon environment variable to allow
  embedding the web UI in an iframe, for example `MINICRON_ALLOW_IFRAME=1`.
  Unset, empty, or whitespace-only values and `0`, `false`, `f`, `no`, `n`,
  or `off` keep embedding blocked (case-insensitive, with surrounding
  whitespace ignored). Any other value enables embedding by omitting
  `X-Frame-Options` and CSP's `frame-ancestors` directive. Restart the daemon
  after changing it.
- `scheduler.max_concurrent_runs` — 1..1024. When the cap is reached,
  job triggers (`schedule`, `manual`, `retry`) are queued durably, see
  `[queue]` below; `[[init]]` startup jobs are never queued. See `catch_up`
  below for how missed fires are handled.
- `[queue]` — the bounded durable execution queue ([ADR-9](adr/0009-durable-execution-queue.md)).
  A queued trigger is a run with status `queued`, persisted before the trigger is
  acknowledged and started later, oldest first with round-robin fairness across
  definitions, at most `drain_rate` per second and only into a free
  `max_concurrent_runs` slot. A run that waits longer than `max_age` becomes
  `skipped` with end reason `queue_expired`. When `max_items`, `max_per_job` or
  `max_bytes` is reached a manual trigger is refused (HTTP 429 `queue_full`) and a
  scheduled/retry trigger is recorded `skipped` / `queue_full`. Items survive a
  restart (a restart never re-queues a run that may have started); the current
  definition is used when an item is taken, and a deleted/disabled one is dropped
  (`skipped` / `definition_removed` or `definition_disabled`). `enabled = false`
  restores the old skip-on-full behavior (items already queued still drain or
  expire). `max_pending_retries` bounds retries that
  are waiting for `retry_delay`; an overflowing retry is dropped with a
  `skipped` / `retry_dropped` run. Workers and `[[init]]` jobs are never queued.
  A zero numeric value selects its default. Changing any `[queue]` value requires a
  daemon restart.
- `logs.db_keep_for` / `logs.db_prune_at` — the SQLite log archive has a
  rolling age budget; chunks older than `db_keep_for` are pruned once a day
  at `db_prune_at` (scheduler timezone). Pruning deletes in small batches,
  then returns the freed space to the filesystem and truncates the WAL.
  Job runs, logs included, are also deleted by run retention
  (`keep_for`/`storage.keep_for_default`), so a job's logs are kept for the
  shorter of the two budgets.
- `logs.db_max_size` — optional size budget for the log archive, in MiB. When
  the archive's used space exceeds it after the age prune, the oldest chunks
  are removed until it fits. `0` (the default) disables the budget; values
  above 1073741824 MiB (1 PiB) are rejected so the byte conversion cannot wrap.
- `logs.durability` — `"batch"` (the default) writes every log line to the
  buffer file before accepting the next one, so a daemon crash loses nothing,
  but fsyncs as a group (see `logs.sync_interval` and `logs.sync_max_dirty`).
  An OS crash or power loss can lose output newer than the last sync —
  nominally `sync_interval`, longer if the disk stalls. `"frame"` is the strict
  mode: it fsyncs every line; a child that prints faster than the disk can sync
  (a few hundred lines per second on many disks) is then slowed down, because
  it blocks writing to its full pipe. Reloadable; applies to runs started
  afterwards.
- `logs.sync_interval` — batch durability: milliseconds between the first
  unsynced log write and the group fsync of every dirty run buffer (default
  2000, range 100–60000). One timer is shared by all runs and exists only while
  some run is dirty; idle runs and an idle daemon schedule nothing. Reloadable.
  `0` selects the default and does not disable batching or syncing; values below
  100 are rejected. Use `logs.durability = "frame"` for per-line fsync.
- `logs.sync_max_dirty` — batch durability: KiB of unsynced output after which a
  run's buffer is fsynced immediately (default 1024, range 1–65536). Reloadable.
  `0` selects the default and does not mean "no limit".
- `logs.disk_min_free` — MiB of free space to keep on the filesystem holding the
  data directory (default 512). Below it the oldest completed runs' logs are
  deleted early, ahead of retention age, until free space reaches 125% of it; the
  value is clamped to a quarter of the filesystem. Omitted selects the default;
  an explicit `0` disables the rule (unlike `sync_interval`, where `0` means the
  default). Reloadable. See [operations](operations.md#log-disk-budget).
- `logs.disk_budget` — optional cap, in MiB, on everything the log tiers occupy:
  the archive database and its WAL, sealed buffers and live buffers (not the
  quarantine). Reclamation of the oldest completed runs' logs starts at 90% and
  stops at 80% of it. `0` (the default) sets no byte budget; values above
  1073741824 MiB are rejected. Unlike `logs.db_max_size` it is enforced whenever
  pressure may have changed, not once a day, and it never deletes live buffers,
  active workers' archived chunks, quarantine or metadata. Reloadable.
- `logs.quarantine_keep_for` / `logs.quarantine_max_size` — opt-in purge of the
  quarantine of corrupt log chunks, applied by the daily log prune: entries older
  than that many days, then the oldest entries until the rest fits that many MiB.
  Both default to `0`: keep everything (and only report its size). Every deletion
  is logged. Reloadable.
- `reads.slots`, `reads.budget`, `reads.work_timeout` — shared admission for
  the expensive on-demand reads: `GET .../log` pages, `GET .../log/raw`
  downloads and `GET /api/v1/metrics/runs` (the SSE follow stream has its own
  64-stream limit). A request needs one of `slots` and a share of `budget` (a
  log page or download is estimated at 4 MiB, a metrics computation at 8 MiB,
  so the default 32 MiB admits at most four of them). The budget is a second,
  independent limit: at the defaults it never binds before `slots` does, and it
  only matters when it is lowered or `slots` is raised (the per-request figures
  are fixed estimates, not measurements; one frame near `logs.max_line` can
  exceed a page's estimate); it queues for up to
  0.5 s, then is refused with `503` and `Retry-After: 1` (`read_busy`). Waiting
  requests are themselves capped at four per slot. `work_timeout` bounds the
  read or aggregation of one request (one page for a download), answering `503`
  `read_timeout` when exceeded; it is separate from the socket write deadline,
  which is renewed for the response. A raw download holds its slot until it
  finishes, so a slow client occupies one slot; a stalled one is cut off by the
  write deadline. At most `max(1, slots-1)` downloads run at once and a further
  one is refused immediately (`503 read_busy`), so downloads cannot lock out
  JSON pages and metrics (with `slots = 1` a download necessarily does). Defaults suit about 1 CPU / 512 MiB. `0` selects the default.
  Changing them requires a daemon restart (reload rejects the change).
- `storage.synchronous` — SQLite synchronous mode for `minicron.db` and
  `minicron-logs.db`. `"full"` (the default) fsyncs every commit. `"normal"`
  saves one fsync per commit and stays consistent after a crash, but can lose
  the most recent commits on power loss. Requires a daemon restart.
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
- `[[init]]` entries run once per daemon start, in file order, before workers
  start and before HTTP is ready. A failed init stops daemon startup. They do
  not rerun on reload. Disabled init entries are skipped.
- Config-owned `[[job]]` entries require a schedule; `[[worker]]` entries use normal supervision.
  Names must be unique across init, job, worker, and registry definitions.
- Reload: `SIGHUP` or `minicrond reload` re-reads the main config and syncs
  its definitions. Removed config-owned entries are disabled and hidden; their
  run history is retained. Listener changes still require a restart.
- Config-owned entries appear in the UI as view-only. API edits, deletion,
  enable/disable, import takeover, and manual trigger/start are rejected.

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
| `retries` | `0` | job | Number of additional attempts after a failed run (0..1000); each attempt is a new run. An explicit `retries = 0` overrides a nonzero `[defaults]` value; omitting the key inherits it |
| `retry_delay` | `5` | job | Seconds before each retry (1..86400; 0 uses default 5); constant delay; timeouts and stopped runs are not retried |
| `run_as` | daemon user | both | `user`, `uid`, or `user:group`. **Root daemon only** |
| `working_dir` | daemon cwd | both | Absolute working directory for the process |
| `env_base` | `clean` | both | `clean`: minimal fixed env; `inherit`: start from the daemon's env |
| `env` | `{}` | both | Literal environment map |
| `secret_env` | `{}` | both | Values must be `env:NAME` or `file:/abs/path`, resolved at spawn |
| `env_file` | — | both | Absolute path to a KEY=VALUE file (≤ 1 MiB) |
| `timeout` | `0` (none) | both | Maximum runtime in seconds; a timeout is reported even if the process then exits 0. An explicit `timeout = 0` (no timeout) overrides a nonzero `[defaults]` value; omitting the key inherits it |
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
use `[[init]]` in the main config for startup tasks; workers use `autostart`.

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
- Export includes registry-owned definitions. Main-config entries remain in
  the main TOML file.

### Runtime environment of a process

With `env_base = "clean"` (default) the process gets a minimal environment:
`PATH=/usr/bin:/bin`, the definition's `TZ`, a passwd-resolved `HOME` when
available, everything from `env`, `env_file`, and resolved `secret_env`, plus the metadata variables
`MINICRON_JOB`, `MINICRON_RUN_ID`, `MINICRON_TRIGGER` (`schedule`,
`manual`, `worker`), and `MINICRON_ATTEMPT`.

If the daemon UID has no passwd entry, jobs and workers still run as its
actual UID/GID. Set an absolute `HOME` in a definition's `env` or `env_file`
when a home directory is needed, and set `working_dir` independently when
the process needs a particular directory. Without either, the process uses
the daemon's working directory and no `HOME` is supplied. `~/...` in
`working_dir` requires a known or explicitly supplied `HOME`. In this case,
`env_base = "inherit"` does not carry the daemon's `HOME`, `USER`, or
`LOGNAME`, which could describe a different user. Its `TZ` is set from the
definition. Set `PATH` in `env` to include `/usr/local/bin` for shell commands;
bare `argv` executables are resolved only in `/usr/bin` and `/bin`, so use
an absolute `argv[0]` for executables elsewhere.

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
