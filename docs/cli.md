# CLI reference

```
minicrond [command] [flags]
```

With no arguments the binary starts the daemon (same as `daemon`).

## Connection model

- **Default (local):** CLI commands talk to the daemon over its mode-0600
  Unix socket (`$MINICRON_DATA/minicron.sock`). No token needed — access is
  granted by filesystem permission plus local peer credentials.
- **Remote:** set `MINICRON_URL` (e.g. `http://host:7423`) and the CLI
  switches to HTTP, requiring `MINICRON_TOKEN` (a bearer token).

### Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `MINICRON_CONFIG` | `minicron.toml` | Bootstrap config path (`daemon`, `init`, `validate`) |
| `MINICRON_DATA` | `~/.local/share/minicron` | Data directory (`daemon`, `logs`, local CLI socket) |
| `MINICRON_URL` | unset | Switch CLI to TCP/HTTP mode |
| `MINICRON_TOKEN` | unset | Bearer token for `MINICRON_URL` and `export` |
| `MINICRON_BIN` | — | Sandbox only: binary override used by `examples/local-test` |

## Commands

### `daemon` — run the scheduler

```sh
minicrond daemon [--config PATH] [--data-dir PATH]
```

Starts the daemon (foreground). `SIGHUP` triggers a config reload. Takes
the instance lock in the data directory; a second daemon against the same
directory refuses to start.

### `init` — write a bootstrap config

```sh
minicrond init [--config PATH]
```

Creates a minimal `minicron.toml` (mode 0600); fails if the file exists.

### `validate` — check a bootstrap config

```sh
minicrond validate [PATH]
```

Loads the config with the same strict decoder and validators the daemon
uses.

### `list` — definitions

```sh
minicrond list
```

Prints every job and worker definition (JSON) with next fire time.

### `status` — daemon metadata

```sh
minicrond status
```

Version, schema, uptime, token fingerprint, capabilities.

### `reload` — hot-reload bootstrap config

```sh
minicrond reload
```

Same as `SIGHUP`. Server bind changes require a restart.

### `run` — trigger a job

```sh
minicrond run NAME [--wait]
```

Triggers an immediate run; `--wait` blocks until it finishes and prints the
final status.

### `logs` — read a run's logs

```sh
minicrond logs RUN_ID [--follow | -f]
```

Prints tagged frames (stderr lines are prefixed `[err] `); `--follow`
polls until the run ends. Reads merge the SQLite archive, live buffer
files, and the in-memory tail transparently.

### `export` / `import` — definition bundles

```sh
minicrond export [--format toml|json]
minicrond import PATH
```

`export` prints every definition as a portable bundle. `import` runs a
server-side preview (hash-checked) and then applies; re-importing updates
definitions by name. Workers and jobs are included; alert channels are
daemon config and stay in the bootstrap file.

### `crontab` — migrate your user crontab

```sh
minicrond crontab
# To import Alice's crontab into the root system service instead:
sudo env MINICRON_DATA=/var/lib/minicron minicrond crontab --user alice
```

Without `--user`, reads the calling user's crontab and imports into the daemon
at `$MINICRON_DATA/minicron.sock` (default: that user's data directory). With
`--user NAME`, the CLI must run as root: it reads and comments NAME's crontab
using `crontab -u NAME`, checks that the selected local daemon supports
`run_as`, and sets `run_as = "NAME"` on every imported job so they run as NAME,
not root. Set `MINICRON_DATA=/var/lib/minicron` to target the root system
service; without it, a root CLI would look for a socket under root's home.
The daemon's TCP port does not select a daemon for this command, and
`MINICRON_URL` is rejected. Do not run `sudo minicrond crontab` without
`--user` when intending to migrate a non-root user's crontab: that reads
root's crontab instead.

The command lists the jobs to import and waits for **Enter**; typing anything
else cancels. It uses the daemon's import preview and only after a successful
import comments out the migrated lines with `# minicrond imported: `. Other
lines, including environment settings, stay in the crontab. Names already
present at preview time cause an error rather than being overwritten. This
requires an interactive terminal and the system `crontab` command.

Cron's `%` stdin syntax, `@reboot`, and `CRON_TZ`/`TZ` settings need manual
migration; the command rejects these rather than silently changing their
behavior. Check shell, environment, timezone and command behavior before
confirming; minicrond doesn't send cron mail. If updating the crontab fails
after import, the command reports that both schedulers may run the jobs:
comment out the original entries manually. Concurrent edits made during the
final `crontab -` installation cannot be locked out; avoid editing the
crontab while migrating.

### `token` — rotate the API token

```sh
minicrond token --rotate
```

The current token cannot be recovered or displayed; rotation prints the
new one.

### `service` — install/uninstall systemd units (root)

```sh
sudo minicrond service install [--user NAME] [--port N] [--force]
sudo minicrond service uninstall [--user NAME]
```

- Root daemon: unit `minicrond`, config `/etc/minicrond/config.toml`, data
  `/var/lib/minicron`, port 7423.
- `--user NAME` (port required): unit `minicrond@NAME`, config
  `~NAME/.minicrond/config.toml`, runs as that user.

### `schema` — print the config JSON Schema

```sh
minicrond schema
```

Emits `schema/minicron.schema.json` (embedded in the binary) — usable to
validate configs or power editors.

### `version`

```sh
minicrond version   # minicrond VERSION (commit) os/arch
```
