# 04 — Configuration

Status: Draft · Format decision: OQ-2 (TOML recommended) · Table style: OQ-4

## Philosophy

- **One small bootstrap file + includes.** `minicrond.toml` holds daemon-level
  settings and points at include globs that carry job/worker definitions.
- **One authority per definition** (ADR-3, details in `05`): a definition
  imported from a file is file-authoritative — the UI, API, and DB cannot
  edit its metadata (the DB stores a reference + cache). Definitions created
  in the UI/API are registry-authoritative and freely editable there.
  Files + reload is the git-friendly path; the UI is the interactive path.
- **Strict and positional.** Unknown keys, bad durations, invalid cron fail
  with file:line:column and a "did you mean" suggestion. A config that
  doesn't validate never touches the running set.

## File discovery (first match wins; all paths logged at startup)

1. `--config PATH` / `MINICROND_CONFIG` env
2. `./minicrond.toml`
3. `$XDG_CONFIG_HOME/minicrond/minicrond.toml` (`~/.config/...`)
4. `/etc/minicrond/minicrond.toml` (typical when running as root)

If none exists: interactive `minicrond` prompts to scaffold; `minicrond
daemon` (headless) exits non-zero. Data dir resolution mirrors this:
`--data-dir` / `MINICROND_DATA` → `~/.local/share/minicrond` (unprivileged)
or `/var/lib/minicrond` (root).

## Full example (vocabulary reference)

```toml
# minicrond.toml — daemon bootstrap
[server]
mode        = "user"                # user | system (root daemon + registered users, spec 17; restart-only)
bind        = "127.0.0.1:7423"      # OQ-8 default port
unix_socket = true                  # peer-auth CLI channel, data_dir/minicrond.sock
tls         = "off"                 # off | auto (self-signed) | cert (with tls_cert/tls_key)

[include]
paths = ["jobs/*.toml", "workers/*.toml"]   # globs, relative to THIS file
prune_missing = false               # OQ-18: true = registry entries from a missing
                                    # file are removed, not just disabled

[scheduler]
timezone        = "UTC"             # default for jobs; per-job override
max_catchup     = 5                 # cap for catch_up = "all"
jitter_gate     = false             # v1.0+: serialize jittered starts

[storage]
keep_runs_default = 200             # applies when job omits keep_runs
keep_for_default  = "30d"
audit_keep        = 5000

[logs]
backend   = "file"                  # "file" | "s3"         (see spec 09)
max_line  = "256KiB"                # hard per-line cap (truncated + flagged)

# [logs.s3]                          # active only when backend = "s3"
# endpoint    = ""                   # empty = AWS default; set for MinIO/R2
# bucket      = "my-bucket"
# prefix      = "minicrond"
# region      = "us-east-1"
# credentials = "env"                # env | file (~/.aws/credentials) | static
# force_path_style = false           # true for MinIO/R2-style endpoints

[defaults]                          # fallbacks for any [[job]]/[[worker]] key
shell       = "/bin/sh"
grace       = "10s"
timeout     = "0"                   # 0 = no timeout
success_codes = [0]

[notify]
on_failure = ["inbox"]              # default channels; see spec 16
```

```toml
# jobs/backup.toml — an include file
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
timeout     = "2h"
grace       = "30s"                 # SIGTERM→SIGKILL window
on_overlap  = "skip"                # parallel | skip | queue | replace
max_queued  = 5
retries     = 2
retry_delay = "1m"
retry_backoff = "exponential"       # constant | linear | exponential
keep_runs   = 30
keep_for    = "90d"
log_max     = "50MiB"               # per-run log budget (file backend)
notify      = ["on-failure"]
labels      = { tier = "prod" }
enabled     = true
```

```toml
# workers/queue.toml
[[worker]]
name        = "queue-worker"
command     = "node /app/worker.js"
run_as      = "nodeuser"
instances   = 2
env         = { NODE_ENV = "production" }
restart     = "on-failure"          # always (default for workers) | on-failure | never
restart_delay   = "5s"
restart_backoff = "exponential"
max_restart_attempts = 5            # consecutive unhealthy starts → fatal
healthy_after   = "30s"
priority    = 10                    # boot order, lower first
depends_on  = ["db-migrate"]        # boot gating (worker names), reverse-order teardown
autostart   = true
stop_signal = "SIGTERM"
```

## Key reference (shared by jobs and workers unless noted)

| Key | Type | Default | Notes |
|---|---|---|---|
| `name` | string | required | unique across kinds; `^[a-z0-9][a-z0-9_.-]{0,99}$` |
| `command` | string | required | shell string; multiline = script written to temp file, fail-fast |
| `shell` | path | `/bin/sh` | used for `command` |
| `run_as` | `user` or `user:group` | daemon user | name or numeric; root daemon only |
| `working_dir` | path | `/tmp` | `~` = run_as home |
| `env` | map | — | visible in UI/API |
| `env_file` | path | — | dotenv; shown as path only |
| `secret_env` | map | — | values via `${file:path}` or env refs; masked everywhere (spec 13) |
| `env_base` | `inherit`\|`clean` | `inherit` | `clean` = minimal PATH/HOME/TZ only |
| `umask` | string | inherit | octal, e.g. `"027"` |
| `limits` | map | — | `cpu_seconds`, `memory_bytes`, `nproc`, `fsize` (setrlimit) [v0.2] |
| `timeout` | duration | `0` | wall clock; 0 = none |
| `grace` | duration | `10s` | stop-ladder window, all kill paths |
| `stop_signal` | signal | `SIGTERM` | |
| `retries`* / `retry_delay` / `retry_backoff` | int/dur/enum | 0/`10s`/`exponential` | jobs only |
| `success_codes` | [int] | `[0]` | exit codes counted as success |
| `on_overlap` / `max_queued` | enum/int | see OQ-16 | jobs only |
| `keep_runs` / `keep_for` | int/duration | from `[storage]` | retention |
| `log_max` / `log_on_full` | dur/enum | `100MiB`/`drop_old` | `drop_old`\|`drop_new`\|`kill` |
| `notify` | [event] | inherit `[notify]` | e.g. `["on-failure", "on-timeout"]` |
| `labels` | map | — | UI/API filtering |
| `enabled` | bool | `true` | false = scheduled but never fires; manual trigger still allowed |
| `driver` | enum | `local` | `local` \| `docker` (spec 10) |

Job-only: `schedule`, `timezone`, `jitter`, `catch_up` (`latest`\|`all`\|`none`),
`run_on_start` (bool). Worker-only: `instances` (1–64), `restart*`,
`healthy_after`, `priority`, `depends_on`, `autostart`.

## Substitution rules

- `${VAR}` and `${file:/path}` expand in **all** string values **except**
  `command` (left to the shell — prevents double-expansion surprises) and
  inside `env_file`/`secret_env` file contents (read literally).
- Missing `${VAR}` at load time = validation error (fail loudly, no empty
  strings sneaking through). `${VAR:-default}` supported.
- `secret_env` values are stored hashed-in-audit, masked in UI/API/logs.

## Validation

`minicrond validate [path]` runs the daemon's real loader in dry-run mode:

- strict decode (unknown key → error with suggestion),
- schedule grammar + timezone existence,
- `run_as` resolvability (warn if not, error at spawn time as `start_error`),
- path existence for `env_file`/`working_dir` (warning, not error),
- cross-file duplicate `name` detection with both source paths listed,
- output: human (file:line:col) or `--json` for editors/CI.

A JSON Schema is published (`minicrond schema > schema.json`) for editor
autocomplete; the TOML itself remains authoritative.

## Reload semantics

SIGHUP / `minicrond reload` / `POST /api/v1/daemon/reload` — validate first,
then apply atomically per `03` (reconciliation). Restart-only keys: anything
under `[server]` (except nothing), `[storage]` sqlite path, `[logs] backend`.

## Open questions

- OQ-2 TOML vs YAML (TOML recommended: no indentation traps, better
  multiline strings for scripts, round-trips cleanly from the UI editor).
- OQ-4 `[[job]]` array-of-tables (recommended: include files append
  naturally, name collisions caught by validation) vs `[jobs.<name>]`.
- OQ-16 `on_overlap` default: `skip` recommended (cron jobs stacking
  silently is the classic foot-gun) vs classic `parallel`.
