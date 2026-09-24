# minicrond

A small, trustworthy local job scheduler and process supervisor in a
single binary: durable cron jobs, supervised workers, compressed tagged
logs, a REST + SSE API, and an embedded web UI. Linux, user mode, v0.1.

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
- **SQLite is the single source of truth.** Definitions live in the
  registry; TOML bundles import/export explicitly (previewed and
  hash-checked). No config-file drift.
- **Real auth model.** Bearer-token TCP API, mode-0600 token-free Unix
  socket for the local CLI, loopback-only by default, secrets referenced
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
| CLI | `list`, `run --wait`, `logs -f`, `status`, `reload`, `import`, `export`, `token --rotate`, `schema`, `service install/uninstall` (systemd) |

## Quickstart

```sh
go build -o minicrond ./cmd/minicrond
./minicrond init
./minicrond daemon
```

On first boot the daemon writes the initial bearer token once to the
mode-0600 `initial-token` file in the data directory. Read and remove it,
then open <http://127.0.0.1:7423>. Local CLI commands use the Unix socket
without a token:

```sh
./minicrond list
./minicrond run hello --wait
./minicrond logs RUN_ID
```

Define jobs in the web UI, via the API, or by importing a TOML bundle:

```toml
[[alert_channel]]
name = "ops"
type = "telegram"
bot_token = "env:MINICRON_TELEGRAM_BOT_TOKEN"   # or file:/absolute/path
chat_id = "-1001234567890"

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

```sh
./minicrond import bundle.toml
```

Run as a systemd service (root daemon or per-user daemons):

```sh
sudo ./minicrond service install                    # unit minicrond, port 7423
sudo ./minicrond service install --user alice --port 7424   # unit minicrond@alice
```

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
