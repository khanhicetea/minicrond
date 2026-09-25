# Getting started

This guide takes you from a source checkout (or release binary) to a running
daemon with a scheduled job, logs, and the web UI. Linux is the supported
platform.

## 1. Build (or download)

From a checkout:

```sh
go build -o minicrond ./cmd/minicrond
```

Release binaries (`minicrond-linux-amd64`, `minicrond-linux-arm64`) with
checksums and build provenance attestations are attached to each GitHub
release.

Check the build:

```sh
./minicrond version
```

## 2. Create the bootstrap config

```sh
./minicrond init          # writes ./minicron.toml (mode 0600)
./minicrond validate      # strict TOML: unknown fields are errors
```

`init` writes a minimal, fully commented-defaults config: loopback bind,
Unix socket enabled, UTC scheduler, file log backend. Every option is
documented in [configuration.md](configuration.md).

## 3. Start the daemon

```sh
./minicrond daemon
```

On first boot the daemon:

1. creates the data directory (`~/.local/share/minicron` by default) and
   requires it to be private (mode 0700),
2. creates `minicron.db` (definitions + run history) and
   `minicron-logs.db` (log archive), both SQLite with WAL,
3. writes the initial API bearer token **once** to the mode-0600 file
   `initial-token` in the data directory, then never again.

Read the token and remove the file:

```sh
DATA=~/.local/share/minicron
cat "$DATA/initial-token" && rm "$DATA/initial-token"
```

## 4. Create your first job

Definitions live in the SQLite registry — not in the config file. Create
them in the web UI, through the API, or by importing a TOML bundle. A
minimal bundle:

```toml
# hello.toml
[[job]]
name = "hello"
schedule = "@every 1m"          # or 5-field cron: "* * * * *"
command = 'echo "hello from minicrond"'
```

Import it (idempotent — re-importing updates the definition by name):

```sh
./minicrond import hello.toml
```

## 5. Watch it run

Local CLI commands talk to the daemon over the mode-0600 Unix socket and
need no token:

```sh
./minicrond list                 # all job & worker definitions
./minicrond run hello --wait     # trigger now, wait for the result
./minicrond logs <run_id>        # tagged stdout/stderr of a run
./minicrond logs <run_id> -f     # follow a running worker's output
./minicrond status               # daemon metadata
```

Every run gets a durable row: status, exit code, timing, trigger, attempt,
and log reference. Jobs receive `MINICRON_JOB`, `MINICRON_RUN_ID`,
`MINICRON_TRIGGER`, and `MINICRON_ATTEMPT` in their environment.

## 6. Open the web UI

Browse to <http://127.0.0.1:7423> and paste the bearer token from step 3
(lost it? `./minicrond token --rotate` and use the new one). The UI
provides:

- **Dashboard** — recent activity at a glance
- **Jobs** — list, create, and edit job/worker definitions with schema-
  validated forms; enable/disable; trigger; start/stop/restart workers
- **Runs** — searchable history with filters
- **Run detail** — metadata plus live SSE log streaming (stdout/stderr
  tagged, ANSI rendered)
- **Metrics** — run counts by outcome over 15m…30d windows
- **Settings** — token fingerprint, schema version, diagnostics

The UI shell and its bundled assets are public; every `/api/` endpoint
requires the bearer token.

## 7. Install as a service (optional)

Running as root, let minicrond provision config and systemd units:

```sh
sudo ./minicrond service install                     # root daemon, port 7423
sudo ./minicrond service install --user alice --port 7424   # per-user daemon
sudo ./minicrond service uninstall [--user alice]
```

The root daemon reads `/etc/minicrond/config.toml` and stores data under
`/var/lib/minicron`; per-user daemons read `~USER/.minicrond/config.toml`
and run as that user. See [operations.md](operations.md) for details.

To migrate existing cron jobs, run the interactive import against the desired
**local Unix socket** (the TCP port does not select the daemon):

```sh
minicrond crontab  # run as Alice: Alice's crontab -> Alice's user daemon
sudo env MINICRON_DATA=/var/lib/minicron minicrond crontab --user alice
# Alice's crontab -> root service, with every imported job set to run_as=alice
```

Check the displayed jobs before pressing Enter. They are commented out in the
source crontab **only after** the daemon imports them. Run `sudo` with
`--user alice` for Alice's crontab; plain `sudo minicrond crontab` reads
root's crontab. See [the CLI reference](cli.md)
for unsupported cron syntax and failure recovery.

## Next steps

- Full option reference: [configuration.md](configuration.md)
- Every CLI command: [cli.md](cli.md)
- HTTP API and SSE: [http-api.md](http-api.md)
- Runbook (backup, retention, security): [operations.md](operations.md)
- Try the end-to-end sandbox: [`examples/local-test`](../examples/local-test/README.md)
