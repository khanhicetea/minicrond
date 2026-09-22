# minicrond

A small local cron scheduler and process supervisor with durable run history and tagged logs. v0.1 targets user mode on Linux.

## Quickstart

```sh
go build -o minicrond ./cmd/minicrond
./minicrond init
./minicrond daemon
```

On first boot the daemon writes the initial bearer token once to the mode-0600 `initial-token` file in the data directory. Read and remove that file after provisioning, then open <http://127.0.0.1:7423>. Local CLI commands use the mode-0600 Unix socket without a token:

```sh
./minicrond list
./minicrond run hello --wait
./minicrond logs RUN_ID
```

Daemon settings use strict TOML. Job and worker definitions have one source of truth: the SQLite registry. Create them in the UI/API or explicitly import a TOML bundle with `minicrond import PATH`; export them with `minicrond export`. Imported `[[job]]` entries support exactly one of `command` (always `shell -c`) or `argv`. Jobs default to a clean environment, UTC, `catch_up = "none"`, and `on_overlap = "skip"`. See `schema/minicron.schema.json` and `specs/`. The `minicron`-prefixed configuration, environment, storage, and API identifiers are retained for compatibility.

## Operations

- Data defaults to `~/.local/share/minicron`; permissions are forced to 0700.
- `run_as` is available only to a root daemon; in user mode, jobs and workers always run as the daemon user.
- SQLite uses WAL. Never directly write the DB. This development build intentionally rejects databases from the former file-authority schema; remove the development database and restart.
- **Log storage is hybrid.** Live runs write compressed chunk files under `data/logs/<run_id>/` first. Every accepted frame is flushed and synced before `Write` succeeds. Finished runs are archived into the separate SQLite log database `data/minicron-logs.db` (not `minicron.db`) and the buffer files are removed. Still-running workers archive their sealed chunks every `logs.worker_flush_interval` (default 15m). Reads and streaming merge the database, buffer files, and a byte-bounded in-memory tail transparently. Buffers orphaned by a crash are salvaged into the archive at startup, up to the last intact frame.
- `log_max` is the per-run hot-buffer accounting budget; it is not an archive-size quota. The archive has a rolling age budget: chunks older than `logs.db_keep_for` (default 720h) are pruned daily at `logs.db_prune_at`. Per-run retention (`keep_runs`/`keep_for`) removes a run's logs from both tiers. Monitor data-directory free space and SQLite WAL/freelist growth.
- `/healthz` and `/readyz` are public and disclose no details. The SPA shell and bundled assets are public; every `/api/` endpoint requires a bearer token.
- Rotate a lost token locally with `minicrond token --rotate`; the existing token cannot be recovered.
- Stop the daemon before backup or restore. Back up and restore the complete data directory together: `minicron.db`, `minicron-logs.db`, their WAL files, and `logs/` buffers. Copying only `minicron.db` does not preserve log history. A binary that encounters a newer schema refuses to start.
- TCP is plaintext HTTP and defaults to loopback. A non-loopback bind is rejected unless `server.allow_insecure_remote = true`; use that opt-in only behind a TLS-authenticated tunnel or reverse proxy.
- The daemon signals process groups. Deliberately daemonized descendants can escape; v0.1 is not a hostile-workload sandbox.

## Telegram alerts

Define named alert channels in the bootstrap config, then opt jobs or workers in with `alerts`. Failed and timed-out runs are delivered asynchronously; successful, skipped, and manually stopped runs do not alert.

```toml
[[alert_channel]]
name = "ops"
type = "telegram"
bot_token = "env:MINICRON_TELEGRAM_BOT_TOKEN" # or file:/absolute/path
chat_id = "-1001234567890"
disable_notification = false

[[job]]
name = "backup"
command = "./backup.sh"
schedule = "0 2 * * *"
alerts = ["ops"]
```

`bot_token` must be an environment-variable or absolute-file reference, so the token is not embedded directly in configuration. Alert delivery is best-effort: an in-memory bounded queue uses limited parallelism and retries transient failures, but queued alerts do not survive a daemon crash. Additional providers can implement the alert channel interface without changing run execution.

## Development

```sh
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=0 go build ./cmd/minicrond
```

Generated contracts are checked in: `schema/minicron.schema.json` and `cmd/minicrond/openapi.json`. Frontend model types are generated from Go with Tygo into `web/src/generated/model.ts`; run `make generate-types` after changing `internal/model`.

A self-contained sandbox with real jobs, a `start.sh`/`.env.example` flow, and an end-to-end smoke test lives in [`examples/local-test`](examples/local-test/README.md).

Licensed under Apache-2.0.
