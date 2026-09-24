# 04 — Configuration

Status: Draft · Format decision: OQ-2 (TOML recommended) · Table style: OQ-4

## Philosophy

- **One small settings file.** `minicron.toml` contains daemon settings only.
- **One definition registry.** SQLite is authoritative. TOML bundles enter
  through explicit import and can be exported for review or version control
  (`05`). Imported files are never watched or linked.
- **Strict and positional.** Unknown keys, bad durations, invalid cron fail
  with file:line:column and a "did you mean" suggestion. A config that
  doesn't validate never touches the running set.

## File discovery (first match wins; all paths logged at startup)

1. `--config PATH` / `MINICRON_CONFIG` env
2. `./minicron.toml`
3. `$XDG_CONFIG_HOME/minicron/minicron.toml` (`~/.config/...`)
4. `/etc/minicron/minicron.toml` (typical when running as root)

If none exists: interactive `minicron` prompts to scaffold; `minicron
daemon` (headless) exits non-zero. Data dir resolution mirrors this:
`--data-dir` / `MINICRON_DATA` → `~/.local/share/minicron` (unprivileged)
or `/var/lib/minicron` (root).

## Full example (vocabulary reference)

```toml
# minicron.toml — daemon bootstrap
[server]
mode        = "user"                # user | system (root daemon + registered users, spec 17; restart-only)
bind        = "127.0.0.1:7423"      # OQ-8 default port
unix_socket = true                  # peer-auth CLI channel, data_dir/minicron.sock
tls         = "off"                 # off | auto (self-signed) | cert (with tls_cert/tls_key)

[scheduler]
timezone        = "UTC"             # default for jobs; per-job override
max_catchup     = 5                 # cap for catch_up = "all"
jitter_gate     = false             # v1.0+: serialize jittered starts

[storage]
keep_runs_default = 200             # applies when job omits keep_runs
keep_for_default  = 30              # days
audit_keep        = 5000

[logs]
backend   = "file"                  # "file" | "s3"         (see spec 09)
max_line  = 256                     # hard per-line cap in KiB (truncated + flagged)

# [logs.s3]                          # active only when backend = "s3"
# endpoint    = ""                   # empty = AWS default; set for MinIO/R2
# bucket      = "my-bucket"
# prefix      = "minicron"
# region      = "us-east-1"
# credentials = "env"                # env | file (~/.aws/credentials) | static
# force_path_style = false           # true for MinIO/R2-style endpoints

[notify]
on_failure = ["inbox"]              # default channels; see spec 16
```

```toml
# definitions.toml — an explicit import/export bundle
[[job]]
name        = "backup-db"
schedule    = "0 2 * * *"           # 5-field cron; see spec 06
timezone    = "Asia/Ho_Chi_Minh"
command     = '''
pg_dump mydb | gzip > /backups/db-$(date +%F).sql.gz
'''
run_as      = "postgres"
working_dir = "~"                   # ~ resolves against run_as user's home
env         = { PGOPTIONS = "-c statement-timeout=60s" }
env_file    = "backup.env"          # relative to the INCLUDING file
timeout     = 7200                  # seconds
grace       = 30                    # seconds; SIGTERM→SIGKILL window
on_overlap  = "skip"                # parallel | skip | queue | replace
max_queued  = 5
retries     = 2
retry_delay = "1m"
retry_backoff = "exponential"       # constant | linear | exponential
keep_runs   = 30
keep_for    = 90                    # days
log_max     = 50                    # per-run log budget in MiB (file backend)
notify      = ["on-failure"]
labels      = { tier = "prod" }
enabled     = true
```

```toml
# another entry in the same import bundle
[[worker]]
name        = "queue-worker"
command     = "node /app/worker.js"
run_as      = "nodeuser"
instances   = 2
env         = { NODE_ENV = "production" }
restart     = "on-failure"          # always (default for workers) | on-failure | never
restart_delay   = 5                 # seconds
restart_backoff = "exponential"
max_restart_attempts = 5            # consecutive unhealthy starts → fatal
healthy_after   = 30                # seconds
priority    = 10                    # boot order, lower first
depends_on  = ["db-migrate"]        # boot gating (worker names), reverse-order teardown
autostart   = true
stop_signal = "SIGTERM"
```

## Key reference (shared by jobs and workers unless noted)

| Key | Type | Default | Notes |
|---|---|---|---|
| `name` | string | required | unique across kinds; `^[a-z0-9][a-z0-9_.-]{0,99}$` |
| `command` / `argv` | string / [string] | exactly one required | command always runs as `shell -c`; argv bypasses the shell |
| `shell` | path | `/bin/sh` | used for `command` |
| `run_as` | `user` or `user:group` | daemon user | name or numeric; available only to a root daemon |
| `working_dir` | path | effective user's home | `~` = run_as home |
| `env` | map | — | visible in UI/API |
| `env_file` | path | — | dotenv; shown as path only |
| `secret_env` | map | — | explicit `file:/path` or `env:NAME` references; masked everywhere (spec 13) |
| `env_base` | `inherit`\|`clean` | `clean` | `clean` = minimal PATH/HOME/TZ only |
| `umask` | string | inherit | octal, e.g. `"027"` |
| `limits` | map | — | `cpu_seconds`, `memory_bytes`, `nproc`, `fsize` (setrlimit) [v0.2] |
| `timeout` | duration | `0` | wall clock; 0 = none |
| `grace` | duration | `10s` | stop-ladder window, all kill paths |
| `stop_signal` | signal | `SIGTERM` | |
| retries | — | — | deferred to v0.2 |
| `success_codes` | [int] | `[0]` | exit codes counted as success |
| `on_overlap` / `max_queued` | enum/int | see OQ-16 | jobs only |
| `keep_runs` / `keep_for` | int/duration | from `[storage]` | retention |
| `log_max` / `log_on_full` | dur/enum | `100MiB`/`drop_old` | `drop_old`\|`drop_new`\|`kill` |
| `notify` | [event] | inherit `[notify]` | e.g. `["on-failure", "on-timeout"]` |
| `labels` | map | — | UI/API filtering |
| `enabled` | bool | `true` | false prevents every new start, including manual starts |
| `driver` | enum | `local` | `local` \| `docker` (spec 10) |

Job-only: `schedule`, `timezone`, `jitter`, `catch_up` (`latest`\|`all`\|`none`),
`run_on_start` (bool). Worker-only: `instances` (1–64), `restart*`,
`healthy_after`, `priority`, `depends_on`, `autostart`.

## Reference rules

- Configuration values are literal; there is no global interpolation.
- `secret_env` alone accepts explicit `env:NAME` and absolute `file:/path`
  references. Resolved values are never persisted, returned, or audited.
- Definition paths are absolute so their meaning does not depend on an
  import file that may later move or disappear.

## Validation

`minicron validate [path]` runs the daemon-settings loader in dry-run mode
with strict decoding and timezone/log-setting validation. Definition bundles
are validated by `minicrond import PATH` before any transaction is applied,
including schedule grammar, command shape, paths, and duplicate names.

A JSON Schema is published (`minicron schema > schema.json`) for editor
autocomplete; the TOML itself remains authoritative.

## Reload semantics

SIGHUP / `minicron reload` / `POST /api/v1/daemon/reload` — validate first,
then apply atomically per `03` (reconciliation). Restart-only keys: anything
under `[server]` (except nothing), `[storage]` sqlite path, `[logs] backend`.

## Open questions

- OQ-2 TOML vs YAML (TOML recommended: no indentation traps, better
  multiline strings for scripts, round-trips cleanly from the UI editor).
- OQ-4 `[[job]]` array-of-tables remains the import/export bundle format.
- OQ-16 `on_overlap` default: `skip` recommended (cron jobs stacking
  silently is the classic foot-gun) vs classic `parallel`.
