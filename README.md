# minicrond

A small, trustworthy local job scheduler and process supervisor in a
single binary: durable cron jobs, supervised workers, compressed tagged
logs, a REST + SSE API, and an embedded web UI. Linux, v0.2.

```
go build -o minicrond ./cmd/minicrond
./minicrond init && ./minicrond daemon
```

## Why minicrond

- **One binary, one data directory.** Scheduler, supervisor, SQLite
  storage, API, and web UI ship in ~18 MB with zero runtime dependencies.
- **Durable history.** Every run is recorded — status, exit code, timing,
  trigger, attempt — in SQLite with guarded state transitions.
- **Tagged, compressed, crash-safe logs.** Live runs write fsynced zstd
  chunk files; finished runs archive into a separate SQLite log database.
  Streams survive crashes; buffers orphaned mid-write are salvaged at boot.
- **Jobs *and* workers.** Cron jobs with catch-up and overlap policies,
  plus long-running supervised processes with restart policies.
- **Clear definition ownership.** UI and API definitions live in SQLite;
  TOML bundles import/export explicitly (previewed and hash-checked).
  Main-config `[[init]]`, `[[job]]`, and `[[worker]]` entries stay file-owned
  and appear read-only in the UI.
- **Real auth model.** Bearer-token TCP API, mode-0600 token-free Unix
  socket for the local CLI, loopback-only by default, optional Unix-only mode, secrets referenced
  as `env:`/`file:` — never inline.

## Features

| Area | Highlights |
|---|---|
| Scheduling | 5-field cron + descriptors, `@every 1s…`, per-job IANA timezones, DST-safe wall-clock firing, `catch_up = none\|latest`, `on_overlap = skip\|parallel`, `retries` + `retry_delay` for failed jobs, global concurrency gate |
| Execution | `command` (via shell) or `argv` (direct exec), clean/inherit env, `env`/`secret_env`/`env_file`, `working_dir`, `run_as` (root daemon), `timeout` + `grace` + configurable `stop_signal`, process-group control, `success_codes` |
| Workers | `autostart`, `restart = always\|on-failure\|never` with `restart_delay`, `max_restart_attempts`, `healthy_after`; start/stop/restart via API/UI/CLI |
| Logs | stdout/stderr tagged frames, per-run `log_max` budget with `drop_old`/`drop_new`, live SSE streaming, ANSI-aware web viewer, raw download |
| Storage | `minicron.db` (definitions, runs, audit) + `minicron-logs.db` (archive) with WAL; rolling age budget pruned daily; per-definition `keep_runs`/`keep_for` |
| API | REST `/api/v1/*` + SSE log stream, OpenAPI 3.1 contract served at `/openapi.json`, `/healthz` + `/readyz` |
| Web UI | Dashboard, definition editor (schema-validated), run history + live logs, run metrics (15m–30d), settings & diagnostics — embedded in the binary |
| Alerts | Telegram channels defined in bootstrap config; jobs/workers opt in with `alerts`; failed/timeout runs notified asynchronously with retries |
| CLI | `list`, `run --wait`, `logs -f`, `status`, `reload`, `import`, `crontab` (interactive migration), `export`, `token --rotate`, `schema`, `service install/uninstall` (systemd) |

## Usage

### 1. Provision a daemon

On Linux, build from source (or use a [release binary](docs/getting-started.md)):

```sh
go build -o minicrond ./cmd/minicrond
./minicrond init       # creates ./minicron.toml (mode 0600)
./minicrond validate   # check bootstrap settings before starting
./minicrond daemon     # foreground; leave running in this terminal
```

This is a **user-mode** instance. Its default data directory is
`~/.local/share/minicron` (private, mode 0700); config and data paths can be
changed with `MINICRON_CONFIG` / `MINICRON_DATA`. The bootstrap TOML controls
the server, scheduler, storage, logs, defaults, and alert channels — **not**
job definitions. Set `BASE_PATH=/tools/minicron` to serve the web UI and API
under that URL prefix (including health and OpenAPI routes). On first boot,
read the one-time web/API token from
`~/.local/share/minicron/initial-token` and remove that file:

```sh
cat ~/.local/share/minicron/initial-token
rm ~/.local/share/minicron/initial-token
```

Open <http://127.0.0.1:7423> and paste the token. The local CLI uses the
private Unix socket, so it does not need a token. If the token is lost, use
`./minicrond token --rotate` locally; this revokes the previous token.

For a socket-only service, set `server.tcp_enabled = false` in `minicron.toml`
and restart. The CLI, full API, and UI remain available on the private Unix
socket; no TCP port or initial bearer token is created. See
[proxy and Unix-only operations](docs/operations.md#unix-only-and-proxied-operation)
before publishing the UI through a reverse proxy.

**Production alternative:** install the binary at a permanent path and use
`sudo minicrond service install` for a root systemd service (config
`/etc/minicrond/config.toml`, data `/var/lib/minicron`), or
`sudo minicrond service install --user alice --port 7424` for a daemon running
as Alice. Installation enables and starts the unit. These are separate
instances from the foreground user daemon; stop the foreground daemon before
installing another on the same port, and target the right data directory for
CLI commands (e.g. `sudo env MINICRON_DATA=/var/lib/minicron minicrond status`
for the root service). Its first token is in `/var/lib/minicron/initial-token`;
read and remove it as root. See [service operations](docs/operations.md#systemd-installation).

### 2. Add jobs and workers

Create editable definitions in the web UI, through the API, or import a TOML **bundle**.
For container startup, put `[[init]]` tasks and `[[worker]]` services in the
main config. Init tasks finish in order before the daemon serves requests;
their failure stops startup.
For example, save this as `bundle.toml` (replace the example program paths
with programs installed on your host):

```toml
[[job]]
name = "backup"
argv = ["/usr/local/sbin/backup.sh"]
schedule = "0 2 * * *"
timezone = "Europe/Berlin"
catch_up = "latest"       # run the most recent missed fire after downtime
on_overlap = "skip"       # don't run two backups at once

[[worker]]
name = "bridge"
argv = ["/usr/local/bin/bridge", "--config", "/etc/bridge.conf"]
restart = "on-failure"
```

```sh
./minicrond import bundle.toml    # preview/validate, then apply by name
./minicrond list                  # definitions and next fire times (JSON)
./minicrond export --format toml  # snapshot portable definitions
```

Imports explicitly update the SQLite registry; editing `bundle.toml` alone
does nothing until it is imported again. Time and size settings are **numbers
in fixed units**, not quoted duration strings: `timeout`, `grace`,
`retry_delay`, `restart_delay`, `healthy_after`, and alert `batch_window` are
seconds; `logs.worker_flush_interval` is minutes; `keep_for`,
`storage.keep_for_default`, and `logs.db_keep_for` are days; `log_max` is MiB
and `logs.max_line` is KiB. Schedules such as `"@every 1m"` and daily
`logs.db_prune_at = "03:30"` are string-valued exceptions.

Jobs run on a schedule or on demand; workers are supervised long-running
processes (autostart by default). To notify on failures, add a channel to
**`minicron.toml`** (not the bundle):

```toml
[[alert_channel]]
name = "ops"
type = "telegram"
bot_token = "file:/path/to/telegram-token"  # replace with a daemon-readable absolute path
chat_id = "-1001234567890"
```

Set `alerts = ["ops"]` on the desired job or worker in `bundle.toml`, reload
the bootstrap config with `./minicrond reload`, then re-import the bundle.
An `env:NAME` token reference is also supported if the variable is set in
the daemon's environment. Bind changes need a restart. See the
[configuration reference](docs/configuration.md) for retries, timeouts,
secrets, retention, and worker restart controls.

### 3. Operate and inspect

In a second terminal, with the same user's data directory (for a root
service, run these as root with `MINICRON_DATA=/var/lib/minicron`):

```sh
./minicrond status                 # uptime, schema, token fingerprint
./minicrond run backup --wait      # manual run; prints a JSON result with run ID
./minicrond logs RUN_ID            # tagged stdout/stderr; ID from the result
./minicrond logs RUN_ID -f         # follow a live run/worker
```

Use the web UI for run history, metrics, live logs, enabling/disabling
schedules, and starting/stopping/restarting workers. For automation, use the
[HTTP API](docs/http-api.md) with a bearer token. TCP is plaintext and bound
to loopback by default; keep remote access behind TLS rather than exposing
the daemon directly.

### 4. Migrate cron and maintain the instance

To migrate a user's crontab, select the **local socket of the destination
daemon**. The command previews entries, waits for Enter, imports, and only
then comments out migrated cron lines:

```sh
minicrond crontab   # current user's crontab -> current user's daemon
sudo env MINICRON_DATA=/var/lib/minicron minicrond crontab --user alice
# Alice's crontab -> root service; imported jobs get run_as=alice
```

Use the second command only for a root service; plain `sudo minicrond crontab`
would read root's crontab. Review unsupported cron syntax and recovery steps
in the [CLI reference](docs/cli.md#crontab--migrate-your-user-crontab).
Back up the **whole data directory** with the daemon stopped (both SQLite
DBs, WAL files, and live `logs/` buffers); check disk space and retention.
See the [operations runbook](docs/operations.md) for backup/restore, upgrades,
security, and troubleshooting.

## Documentation

- [Getting started](docs/getting-started.md) — build to first scheduled job
- [Configuration reference](docs/configuration.md) — every daemon and definition option
- [CLI reference](docs/cli.md) — commands, flags, environment variables
- [HTTP API](docs/http-api.md) — endpoints, auth, SSE streaming
- [Operations runbook](docs/operations.md) — storage, backup/restore, tokens, security, systemd, troubleshooting
- [Architecture](docs/architecture.md) — components, run lifecycle, storage design
- [Design specs](specs/) and [ADRs](docs/adr/) — how the design was argued out

Machine-readable contracts, CI-verified: `schema/minicron.schema.json`
(config; also `minicrond schema`) and `cmd/minicrond/openapi.json`.

## Operations notes

- Data defaults to `~/.local/share/minicron` and must stay mode 0700;
  SQLite runs WAL — never write the DBs directly.
- Back up and restore the **complete** data directory together
  (`minicron.db`, `minicron-logs.db`, WAL files, `logs/` buffers), daemon
  stopped. Copying only `minicron.db` loses log history.
- Rotate a lost token locally: `minicrond token --rotate`.
- TCP is plaintext HTTP, loopback by default; non-loopback binds require
  the explicit `allow_insecure_remote` opt-in (use only behind TLS).
- v0.1 is a trusted-workload supervisor, not a sandbox: the daemon signals
  process groups, but deliberately daemonized descendants can escape.

## Development

```sh
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...   # pure-Go SQLite; CGO not required to build
make build                          # stripped release-style build
make check                          # tests + vet + generated-contracts diff
make contracts                      # verify schema/OpenAPI against generators
```

Generated contracts are checked in: `schema/minicron.schema.json`,
`cmd/minicrond/openapi.json`, and `web/src/generated/model.ts` (Tygo;
`make generate-types` after changing `internal/model`). Release CI builds
linux/amd64 + linux/arm64, runs `govulncheck`, and attaches checksums and
build-provenance attestations.

A self-contained sandbox with real jobs, `start.sh`/`smoke.sh`, and an
end-to-end test lives in [`examples/local-test`](examples/local-test/README.md).

Licensed under Apache-2.0.
