# local-test: real sandbox for hands-on testing of minicron

A self-contained sandbox that runs the real daemon against real job scripts
from this repository. Nothing touches your `~/.local/share/minicron`; all
state lives under `examples/local-test/.data/` (delete it to reset).

## Layout

```
examples/local-test/
├── .env.example   # environment template; start.sh copies it to .env
├── start.sh       # build + validate + start daemon (background), show token
├── stop.sh        # stop the sandbox daemon
├── smoke.sh       # end-to-end test: list/run/logs/failure/timeout/reload
├── mc             # CLI wrapper with sandbox env applied (uses unix socket)
├── common.sh      # shared helpers (sourced, not run directly)
├── minicron.toml  # sandbox bootstrap config (bind 127.0.0.1:7423)
├── jobs/          # example definitions included by minicron.toml
│   ├── hello.toml         # job: @every 1m, MINICRON_* metadata env
│   ├── heartbeat.toml     # job: @every 2m, multi-line stdout
│   ├── flaky.toml         # job: exits 42 -> recorded as failed
│   ├── slow.toml          # job: 3s timeout + 2s grace kill
│   └── worker-ticker.toml # worker: autostart, restart on-failure
└── scripts/       # the actual shell scripts the jobs execute
```

The jobs reference `scripts/` through `$EXAMPLE_DIR`, which comes from the
generated `env.local` via each definition's `env_file` (a real minicron
feature). No absolute paths are checked in.

## Run it

```sh
cd examples/local-test

./start.sh        # reads .env (created from .env.example on first run),
                  # builds ../../cmd/minicrond, validates, starts the daemon,
                  # waits for readiness, and prints the one-time bearer token

./smoke.sh        # end-to-end test against the running daemon
```

Everyday commands (unix socket, no token needed):

```sh
./mc list                        # definitions: 4 jobs + 1 worker
./mc status                      # daemon metadata
./mc run hello --wait            # trigger and wait for a run
./mc logs <run_id>               # tagged logs of that run
./mc run flaky --wait            # deliberate failure (exit 42)
./mc run slow --wait             # killed by its own timeout
./mc reload                      # hot-reload after editing minicron.toml/jobs/
./mc token --rotate              # new bearer token for the web UI
./stop.sh
```

Web UI: open <http://127.0.0.1:7423> and paste the bearer token printed by
`./start.sh` (first boot only). Lost it? `./mc token --rotate`.

## What the sandbox demonstrates

- `hello` — `@every` schedules, injected `MINICRON_JOB` /
  `MINICRON_RUN_ID` / `MINICRON_TRIGGER` env, tagged stdout logs
- `heartbeat` — multi-line output captured per run
- `flaky` — non-zero exit recorded as `failed` with `exit_code`
- `slow` — `timeout` + `grace` process-group kill -> terminal status `timeout`
- `ticker` — supervisor worker: autostart on boot, restart policy,
  start/stop/restart via `/api/v1/workers/ticker/*`
- clean environment defaults (`env_base = "clean"`), registry-based
  definitions (`start.sh` imports `jobs/*.toml` explicitly), retention
  (`keep_runs_default = 100`)

## HTTP mode (optional)

The CLI defaults to the unix socket at `.data/minicron.sock` (no token). To
exercise the TCP API instead, set in `.env`:

```
MINICRON_URL=http://127.0.0.1:7423
MINICRON_TOKEN=<token from start.sh or ./mc token --rotate>
```

## Reset / troubleshoot

- Full reset: `./stop.sh && rm -rf .data .env env.local`
- Port 7423 busy: edit `bind` in `minicron.toml` (requires daemon restart)
- "data directory is locked": another daemon owns `.data`; `./stop.sh` first
- No `go` toolchain: set `MINICRON_BIN` in `.env` or run `make build` at the
  repo root first
- Daemon log: `.data/daemon.log`
