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
   `data/logs/<run_id>/`. Every accepted frame is written to the chunk file
   before the next line is read, so a daemon crash loses no accepted output.
   With `logs.durability = "batch"` (default, ADR-8 1B) the fsync is grouped:
   one shared timer, armed only while some run holds unsynced output, syncs all
   dirty buffers `logs.sync_interval` (default 2 s) after the first unsynced
   write, and a run syncs at once when it holds `logs.sync_max_dirty` (default
   1 MiB) of unsynced output. A sparse run is synced by the timer without
   another line arriving; pipe EOF, run completion and orderly shutdown always
   sync. An **OS crash or power loss can lose output newer than the last sync**
   (nominally the sync interval, longer if the disk stalls); a daemon crash loses
   nothing. `"frame"` is the strict mode: every line is fsynced before the next
   is read. `log_max` defaults to `100MiB` and bounds raw frame bytes
   in the file buffer (with `log_on_full` = `drop_old`/`drop_new`), not the
   archive. Successful archival frees buffer capacity for either policy.
2. **Archive tier** — finished runs are sealed on completion and archived
   into `data/minicron-logs.db` by a background archiver, which then removes
   their buffer files. Run completion never waits on the archive; the sealed
   buffer stays readable until it is moved. Still-running
   workers seal+archive their chunks every `logs.worker_flush_interval`
   (default 15m).

Crash-safety: buffers orphaned by a crash are salvaged into the archive at
startup, up to the last intact frame. Failed final archival leaves its buffer
on disk and is retried at the worker flush cadence, without a restart; the
same happens to sealed runs still queued at shutdown, which are archived at
the next start. At most two transfers run at once, each in batches of at most
8 MiB / 64 chunks (one oversized chunk is allowed); live writers can proceed
between batches. `GET /api/v1/daemon` reports the archive backlog under
`diagnostics.log_archive_backlog`, and connection waits for both databases
(`diagnostics.databases`, writer and read pool) to show whether SQLite is the
bottleneck.
Reads synchronize with migration and share one page budget across the
database and the buffer files (the active chunk included, since it is written
through on every frame). The daemon keeps **no in-memory payload tail or
per-viewer queue**: a log viewer reads stored chunks on demand, so an
unwatched run costs nothing extra and a stalled viewer holds no memory.

Accepted downside of following a run (ADR-8 / audit D03): each SSE poll (every
2 s per viewer) re-decodes the active chunk from its start — up to 1 MiB of raw
output, more only for oversized lines — and holds that run's shared read lock
for the page, so the run's writer waits for the decode, once per poll per
viewer. The cost exists only while someone watches, is bounded by the 64-stream
limit and the 1 MiB page budget, and ends when the viewer disconnects; display
lag of up to about two seconds is the price of paying nothing for unwatched runs.

On-demand reads other than SSE (JSON log pages, raw downloads, run metrics) pass
one admission gate with a small slot count, a byte budget and a per-request
work deadline (`[reads]` in `docs/configuration.md`; defaults 4 slots, 32 MiB,
20 s). JSON pages carry at most about 1 MiB of payload. Metrics are computed on
request only, shared between concurrent requests and kept for about 3 seconds;
there is no background refresh, and the aggregation stops when its request is
canceled or times out. Excess requests receive a retryable `503` rather than
competing with job execution. API metadata lookups (run, definition, listings)
use the SQLite read pool instead of the single writer connection; the execution
and supervisor paths keep using the writer connection. The read pool has four
connections, the same as the default `reads.slots`, so four concurrent metrics
computations (different `range`/`buckets`) can briefly queue those metadata
lookups behind them; each is bounded by `reads.work_timeout`, and none of it
touches the writer connection. Design record:
[ADR-11](adr/0011-read-admission.md).

Log storage failures (ADR-8 2A): if writing or fsyncing a run's log fails (disk
full, I/O error), the run keeps executing and its stdout/stderr keep being
drained; output that cannot be stored is discarded. The run is flagged
`log_truncated`, the failure is logged once (not per line), and the buffer's
index records `dropped_frames`/`dropped_bytes`/`sync_failures`. Capture retries
on a fresh chunk with a growing delay (1 s up to 30 s, evaluated when the next
line arrives) and, once storage works again, writes a `system` line stating how
many lines were not stored. Lines discarded while capture is down do not
consume sequence numbers, but the single frame whose write failed does (it may
be partly on disk), so a failure shows up as one skipped sequence — which an
SSE `gap` event can report — plus the `system` line. Timeouts and stop requests
are unaffected.

Corrupt buffers: an orphaned chunk that cannot be decoded at all is never
deleted. It is moved at once to `data/logs/.quarantine/<run>-<chunk>.zst.corrupt`,
the run's valid chunks are archived (so the run stays readable), and the sweep
reports the quarantine once. A chunk corrupt after a valid prefix has the prefix
archived and the original bytes copied to the same
`.quarantine/<run>-<chunk>.zst.corrupt` name. If the quarantine move itself
fails, the chunk stays in place and fails each sweep; after repeated failures the
whole buffer moves to `.quarantine/<run>/`. Torn tails left by a crash are still
salvaged up to the last intact frame. Quarantined files are never pruned
automatically; remove them deliberately.

Retention:

- **Runs:** per-definition `keep_runs` (default `storage.keep_runs_default`
  = 10,000) and `keep_for` in days (default 7). Pruning a run removes its logs from
  both tiers.
- **Log archive:** chunks older than `logs.db_keep_for` in days (default 30) are
  pruned daily at `logs.db_prune_at` (default 03:30 local), in small batches.
  With `logs.db_max_size` set, the same sweep then removes the oldest chunks
  until the archive fits that many MiB. Afterwards freed pages are returned
  to the filesystem (incremental auto-vacuum) and the WAL is truncated.

Monitor data-directory free space. The first start after upgrading to log
archive schema 3 runs a one-time `VACUUM` of `minicron-logs.db` to enable
incremental auto-vacuum; it needs temporary free space about the size of the
file. The main `minicron.db` does not shrink after run retention deletes rows;
if physical reclamation is needed there, stop the daemon, back up the
complete directory, and use SQLite `VACUUM` before restarting.

## Alert delivery

Monitor `GET /api/v1/metrics/alerts` (`counts.failed`, `counts.dropped`,
`counts.interrupted`, and `queue_depth`). Inspect a failed run at
`GET /api/v1/runs/{id}/alerts` for per-channel attempts and the last safe
error message. These are observations, not a durable delivery queue: after a
crash, in-flight alerts are marked `interrupted` but not replayed. Test an
individual channel with `POST /api/v1/alert-channels/{name}/test`.

## Backup and restore

1. Stop the daemon (`systemctl stop minicrond` or SIGTERM; wait for exit).
2. Copy the **complete** data directory: `minicron.db`, `minicron-logs.db`,
   their WAL files, and `logs/` buffers together. Copying only
   `minicron.db` does not preserve log history.
3. Restore = stop the daemon, replace the directory, start the daemon.
   Never restore into a directory a daemon is using, and never run two
   daemons against one data directory.

## Tokens

- With TCP enabled, first boot writes `initial-token` (mode 0600) exactly once — read and
  remove it after provisioning.
- Lost token: rotate locally with `minicrond token --rotate` (uses the
  Unix socket, no token needed). The old token is revoked immediately.
- The daemon status exposes a token fingerprint only — tokens are never
  stored in the registry or logs.
- Unix-only mode creates no new token and disables rotation. A pre-existing
  token hash is retained for a later return to TCP mode; rotate after that
  return if the old token is unknown.

## Unix-only and proxied operation

Set `server.tcp_enabled = false` and keep `server.unix_socket = true`, then
restart the daemon. This is a general local service option: the CLI and all
HTTP routes use `MINICRON_DATA/minicron.sock`, while no TCP port is opened.
Keep the data directory mode 0700 and socket mode 0600. The socket accepts
the daemon UID and root; a proxy therefore needs a same-UID relay or process
identity, with access granted only to trusted operators.

If an HTTP reverse proxy serves the UI, use Unix-only mode. The proxy must
authenticate and authorize every request to the app origin, including SPA
assets, API mutations, downloads, and SSE. Deny `/api/v1/token/rotate`,
enforce CSRF/Origin checks on writes, prevent credential forwarding to other
origins, and give each daemon its own browser origin. API access permits
arbitrary job commands as the daemon UID, so treat it as full operator access.
Direct daemon responses deny framing; an embedding proxy may replace
`X-Frame-Options: DENY` and the existing CSP `frame-ancestors 'none'` directive
with a narrow framing policy, preserving the other CSP restrictions. Adding
a second CSP header does not override the original restriction.
Never grant access by trusting a caller-supplied identity header.

For an unprivileged numeric UID without a passwd entry, set an absolute
`MINICRON_DATA` and a usable `HOME` for the daemon process. Set each job's
`working_dir` and, if needed, `env.HOME` explicitly. Jobs inherit the actual
daemon UID/GID; they do not inherit an ambient `HOME`, `USER`, or `LOGNAME`
when that UID has no passwd entry. Set `env.PATH` for shell commands and
use absolute `argv[0]` paths for programs outside `/usr/bin` or `/bin`.

## Security model

- TCP is plaintext HTTP, enabled by default, and defaults to loopback. A non-loopback bind is
  rejected at load unless `server.allow_insecure_remote = true`; use that
  opt-in only behind a TLS-authenticated tunnel or reverse proxy.
- Every `/api/` endpoint and `/openapi.json` require the bearer token over TCP;
  `/healthz`, `/readyz`, and UI shell assets are public
  and disclose nothing.
- The Unix socket is mode 0600 in a 0700 data directory; daemon-UID and root
  peers are authorized by kernel credentials.
- Secrets (`secret_env`, alert `bot_token`) are `env:NAME` or
  `file:/absolute/path` references resolved at spawn/startup — never
  inline values in config or the registry.
- `run_as` is available only to a root daemon; in user mode jobs and
  workers always run as the daemon user.
- The daemon signals process **groups** for stop/timeout. Deliberately
  daemonized descendants can escape; v0.1 is not a hostile-workload
  sandbox.

### Browser XSS defenses

The UI renders job definitions, API errors, and decoded log output as React
text. ANSI log formatting uses fixed CSS classes, not generated HTML or
clickable terminal hyperlinks. The chart dependency generates SVG markup;
its text and attribute escaping must remain intact when upgrading it. Keep
untrusted values out of raw HTML, script, and URL insertion paths. CSP
supplements this escaping. In standalone mode the bearer token is held in
`sessionStorage`, which executing scripts can read; XSS could gain full
operator access, including job command execution.

The daemon sends CSP on UI, asset, API, and error responses. Scripts and
stylesheets are limited to the same origin; inline scripts, event-handler
attributes, eval, inline stylesheet blocks, objects, frames, workers, and
native form submissions are blocked. Forms use authenticated fetch calls.
Style attributes remain allowed for chart SVG and dynamic UI styling, and
data images remain allowed. `base-uri 'self'` permits the SPA's validated
`BASE_PATH` tag while blocking an external base URL.

This is an origin allowlist policy. For internet-facing deployments or
deployments with less-trusted log producers, consider a nonce- or hash-based
script policy, especially if the origin also serves other applications or
user-controlled files. Such a policy must authorize the Vite entry script
and any future imports; adding `'strict-dynamic'` without a nonce or hash
will block the app. Keep each daemon on its own origin: stricter CSP cannot
isolate applications that already share browser storage and origin access.
Test a stronger policy in report-only mode before enforcement. Trusted Types
enforcement needs compatibility work on the chart dependency's HTML sinks.
See the [OWASP CSP guidance](https://cheatsheetseries.owasp.org/cheatsheets/Content_Security_Policy_Cheat_Sheet.html).

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
