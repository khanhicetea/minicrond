# minicron

A small local cron scheduler and process supervisor with durable run history and tagged logs. v0.1 targets user mode on Linux.

## Quickstart

```sh
go build -o minicron ./cmd/minicron
./minicron init
./minicron daemon
```

The daemon prints the initial bearer token **once**. Save it, open <http://127.0.0.1:7423>, and enter it. Local CLI commands use the mode-0600 Unix socket without a token:

```sh
./minicron list
./minicron run hello --wait
./minicron logs RUN_ID
```

Configuration is strict TOML. `[[job]]` supports exactly one of `command` (always `shell -c`) or `argv`. Jobs default to a clean environment, UTC, `catch_up = "none"`, and `on_overlap = "skip"`. See `schema/minicron.schema.json` and `specs/`.

## Operations

- Data defaults to `~/.local/share/minicron`; permissions are forced to 0700.
- SQLite uses WAL and forward-only migrations. Never directly write the DB.
- **Log storage is hybrid.** Live runs write compressed chunk files under `data/logs/<run_id>/` first (crash-safe). Finished runs are archived into the separate SQLite log database `data/minicron-logs.db` (not `minicron.db`) and the buffer files are removed. Still-running workers archive their sealed chunks every `logs.worker_flush_interval` (default 15m). Reads and streaming merge the database, buffer files, and the in-memory tail transparently. Buffers orphaned by a crash are salvaged into the archive at startup, up to the last intact frame.
- The log archive is pruned daily at `logs.db_prune_at` (scheduler timezone); archived logs older than `logs.db_keep_for` (default 720h) are deleted. Per-run retention (`keep_runs`/`keep_for`) removes a run's logs from both tiers.
- `/healthz` and `/readyz` are public and disclose no details. The SPA shell and bundled assets are public; every `/api/` endpoint requires a bearer token.
- Rotate a lost token locally with `minicron token --rotate`; the existing token cannot be recovered.
- Stop the daemon before copying its database for rollback. A binary that encounters a newer schema refuses to start. Restore by replacing `minicron.db` and restarting.
- The daemon signals process groups. Deliberately daemonized descendants can escape; v0.1 is not a hostile-workload sandbox.

## Development

```sh
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=0 go build ./cmd/minicron
```

Generated contracts are checked in: `schema/minicron.schema.json` and `cmd/minicron/openapi.json`.

A self-contained sandbox with real jobs, a `start.sh`/`.env.example` flow, and an end-to-end smoke test lives in [`examples/local-test`](examples/local-test/README.md).

Licensed under Apache-2.0.
