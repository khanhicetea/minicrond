# Operations runbook

## Data directory

Default `~/.local/share/minicron` (`--data-dir` / `MINICRON_DATA` to
change). The daemon **requires** it to be private (group/other bits zero)
and refuses to start otherwise.

```
data/
├── minicron.db          # definitions + run history + audit (SQLite, WAL)
├── minicron-logs.db     # log archive (SQLite, WAL) — separate database
├── logs/<run_id>/       # live run log buffers (compressed zstd chunks)
├── minicron.sock        # mode-0600 Unix socket (local CLI)
├── minicron.lock        # instance lock (flock + pid)
└── initial-token        # one-time initial bearer token (mode 0600)
```

Rules of thumb:

- Never write the databases directly. SQLite runs in WAL mode; a binary
  that encounters a newer schema refuses to start (no silent downgrades).
- One daemon per data directory, enforced by `minicron.lock`.
- This development build rejects databases from the former file-authority
  schema era — remove the old database and restart.

## Hybrid log storage

Log storage is two-tier (ADR-6):

1. **Live tier** — running runs write compressed chunk files under
   `data/logs/<run_id>/`. Every accepted frame is flushed and synced before
   `Write` returns. `log_max` defaults to `100MiB` and bounds raw frame bytes
   in the file buffer (with `log_on_full` = `drop_old`/`drop_new`), not the
   archive. Successful archival frees buffer capacity for either policy.
2. **Archive tier** — finished runs are archived into
   `data/minicron-logs.db` and their buffer files removed. Still-running
   workers seal+archive their chunks every `logs.worker_flush_interval`
   (default 15m).

Crash-safety: buffers orphaned by a crash are salvaged into the archive at
startup, up to the last intact frame. Failed final archival leaves its buffer
on disk and is retried at the worker flush cadence, without a restart.
Transfers are serialized and use batches of at most 8 MiB / 64 chunks (one
oversized chunk is allowed); live writers can proceed between batches.
Reads synchronize with migration and share one page budget across the
database, files, and memory tail.

Retention:

- **Runs:** per-definition `keep_runs` (default `storage.keep_runs_default`
  = 200) and `keep_for` (default 720h). Pruning a run removes its logs from
  both tiers.
- **Log archive:** chunks older than `logs.db_keep_for` (default 720h) are
  pruned daily at `logs.db_prune_at` (default 03:30 local).

Monitor data-directory free space and SQLite WAL/freelist growth. Age
retention is not a disk-space quota: SQL deletion makes pages reusable but
does not normally shrink the database. If physical reclamation is needed,
stop the daemon, back up the complete directory, and use SQLite `VACUUM` on
`minicron-logs.db` with sufficient temporary free space before restarting.

## Backup and restore

1. Stop the daemon (`systemctl stop minicrond` or SIGTERM; wait for exit).
2. Copy the **complete** data directory: `minicron.db`, `minicron-logs.db`,
   their WAL files, and `logs/` buffers together. Copying only
   `minicron.db` does not preserve log history.
3. Restore = stop the daemon, replace the directory, start the daemon.
   Never restore into a directory a daemon is using, and never run two
   daemons against one data directory.

## Tokens

- First boot writes `initial-token` (mode 0600) exactly once — read and
  remove it after provisioning.
- Lost token: rotate locally with `minicrond token --rotate` (uses the
  Unix socket, no token needed). The old token is revoked immediately.
- The daemon status exposes a token fingerprint only — tokens are never
  stored in the registry or logs.

## Security model

- TCP is plaintext HTTP and defaults to loopback. A non-loopback bind is
  rejected at load unless `server.allow_insecure_remote = true`; use that
  opt-in only behind a TLS-authenticated tunnel or reverse proxy.
- Every `/api/` endpoint requires the bearer token over TCP;
  `/healthz`, `/readyz`, `/openapi.json`, and UI shell assets are public
  and disclose nothing.
- The Unix socket is mode 0600 in a 0700 data directory; local access is
  authorized by peer credentials.
- Secrets (`secret_env`, alert `bot_token`) are `env:NAME` or
  `file:/absolute/path` references resolved at spawn/startup — never
  inline values in config or the registry.
- `run_as` is available only to a root daemon; in user mode jobs and
  workers always run as the daemon user.
- The daemon signals process **groups** for stop/timeout. Deliberately
  daemonized descendants can escape; v0.1 is not a hostile-workload
  sandbox.

## systemd installation

```sh
sudo minicrond service install                    # root daemon
sudo minicrond service install --user alice --port 7424
sudo minicrond service uninstall [--user alice]
```

| Mode | Unit | Config | Data dir | Port |
|---|---|---|---|---|
| root daemon | `minicrond` | `/etc/minicrond/config.toml` | `/var/lib/minicron` | 7423 |
| per-user | `minicrond@USER` | `~USER/.minicrond/config.toml` | default (user's) | required, unique |

A hand-written sample unit is checked in at
[`examples/systemd/minicron.service`](../examples/systemd/minicron.service).
`--port` is required for per-user daemons so every daemon on the host binds
a unique TCP port. Service mode logs to the journal.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `data directory must be a private directory` | `chmod 700` the data dir (or recreate it) |
| `data directory is locked by another daemon` | Another daemon owns it; stop it first |
| Daemon won't start with alert channels | A `bot_token` reference could not be resolved (unset env var / unreadable file) — fix the reference or disable the channel |
| `non-loopback plaintext HTTP requires allow_insecure_remote=true` | Intentional; bind loopback or set the opt-in behind TLS |
| Web UI 401 | Wrong/rotated token — rotate again or re-check `initial-token` |
| Runs recorded as `interrupted` | Daemon was killed; recovery marks unobservable active runs `interrupted` at boot |
| Jobs didn't fire while daemon was down | By design (`catch_up = "none"` default); use `catch_up = "latest"` to fire the most recent miss |

## Upgrade / rollback

Stop the daemon, back up the data directory (above), replace the binary,
start. Rollback = stop, restore the directory snapshot, restore the old
binary, start. Never downgrade a live newer-schema database.
