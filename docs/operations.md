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
are unaffected, and so is the final status: if closing the log fails when a run
ends, the run keeps its real status (for example `succeeded`) and is marked
`log_truncated`.

Corrupt buffers: an orphaned chunk that cannot be decoded at all is never
deleted. It is moved at once to `data/logs/.quarantine/<run>-<chunk>.zst.corrupt`,
the run's valid chunks are archived (so the run stays readable), and the sweep
reports the quarantine once. A chunk corrupt after a valid prefix has the prefix
archived and the original bytes copied to the same
`.quarantine/<run>-<chunk>.zst.corrupt` name. If the quarantine move itself
fails, the chunk stays in place and fails each sweep; after repeated failures the
whole buffer moves to `.quarantine/<run>/`. Torn tails left by a crash are still
salvaged up to the last intact frame. By default quarantined files are kept
forever and never pruned automatically: watch `quarantine_bytes`,
`quarantine_entries` and `quarantine_oldest_age_s` under
`diagnostics.log_storage.disk` (see below) and remove or repair them
deliberately. To bound them, opt in to `logs.quarantine_keep_for` (days) and/or
`logs.quarantine_max_size` (MiB): the daily log prune then deletes whole
top-level entries (a `<run>-<chunk>.zst.corrupt` file or a `<run>/` directory),
oldest first, and logs every deletion at warn level with its reason. Disk
pressure never touches the quarantine.

## Log disk budget

`log_max` bounds one run's hot buffer, not the disk; `logs.db_max_size` is a
once-a-day archive-only limit. The log **disk budget** (ADR-8 3A,
[ADR-10](adr/0010-log-disk-budget.md)) is the real bound and outranks retention
age: when it is exceeded, the oldest completed runs' logs are deleted early,
even if `keep_for` / `logs.db_keep_for` have not elapsed.

Two rules, both reloadable:

| Setting | Default | Meaning |
|---|---|---|
| `logs.disk_min_free` (MiB) | `512` | Keep this much free on the filesystem of the data directory. Below it, reclaim up to 125% of it. The value is clamped to a quarter of the filesystem. `0` disables the rule. |
| `logs.disk_budget` (MiB) | `0` (off) | Cap on the log tiers: archive database **plus WAL**, sealed buffers and live buffers (not the quarantine). Reclaim starts at 90% of it and stops at 80%. |

What is deleted, oldest first: archived chunks of completed runs, then sealed
buffers of completed runs still waiting for archival. What is **never**
deleted by this mechanism: live buffers, archived chunks of runs that still have
a writer (workers), `.quarantine/`, `minicron.db` metadata (definitions, run
records, audit, pending execution records) and anything outside the log tiers.
A run whose logs were reclaimed keeps its run record; its log reads empty.

A pass runs on the hourly retention tick, the worker-flush tick and the daily log
prune, at startup and on reload, and when the log store hints that pressure may
have changed (a chunk rotated, a run sealed, log capture failed), at most once per
10 seconds. An idle daemon runs nothing. Without pressure a pass is a single
`statfs`; the tiers are walked only when a watermark may be exceeded. If
`statfs` fails, headroom is skipped (reported as `statfs_error`) and only the byte
budget is enforced.

If everything eligible is gone and pressure remains, `diagnostics.log_storage.disk`
shows `insufficient: true` and the daemon logs one error per episode. Running
children then follow the capture-failure policy above (output is discarded, they keep
running). **Admission is not wired to disk pressure in this build:** jobs and
workers still start normally while `insufficient` is true, and the only effect on
them is that their log output is dropped (ADR-8 2A) until space returns. Refusing
or queueing new work under disk pressure belongs to the queue policy (ADR-8 4B),
which may consult `Store.DiskPressure()` once it exists. Fix the cause: free space, raise
`logs.disk_budget`, or lower retention. Expect occasional shorter history during
noisy periods; this is the accepted trade-off.

### Diagnostics

`GET /api/v1/daemon` returns, under `diagnostics`:

- `log_storage.disk` — `hot_bytes`/`hot_runs`, `sealed_bytes`/`sealed_runs`,
  `archive_bytes` (database + WAL, `archive_wal_bytes` separately), `log_bytes`
  (what the budget applies to), `quarantine_bytes`/`quarantine_entries`/
  `quarantine_oldest_age_s`, `quarantine_purged_*`, the policy (`budget_bytes`,
  `min_free_bytes` effective, `free_bytes`, `total_bytes`, `statfs_error`) and the
  outcome (`pressure`, `insufficient`, `passes`, `pressure_passes`,
  `insufficient_passes`, `pruned_runs`/`pruned_chunks`/`pruned_bytes`,
  `sealed_runs_deleted`/`sealed_bytes_deleted`). Tier sizes are measured on
  demand and cached for a few seconds; polling it does not scan continuously.
  Alert on `insufficient`, on `free_bytes` approaching `min_free_bytes`, and on
  growing `quarantine_bytes`.
- `log_storage.capture` — `failures_total` (runs that entered degraded capture
  since start), `dropped_frames`/`dropped_bytes`, `sync_failures`, `active_writers`,
  `degraded` and up to ten `degraded_runs` with their error. It never waits for a
  busy writer (`unavailable` counts those skipped).
- `log_storage.maintenance` — per task (`retention`, `worker_flush`, `log_prune`,
  `disk_budget`, `quarantine_purge`): `runs`, `last_ms`, `max_ms`, `total_ms`,
  `last_at`.
- `log_storage.writer_lock_waits` — `count`, `total_ms`, `max_ms` of log writes
  that blocked on the run lock or the writer lock (behind an archive batch, a
  read page or a flush). Uncontended writes are not counted, so it costs nothing
  on the happy path.
- `terminal_persistence` — time from a run's process end to its terminal state
  being committed (`persisted`, `last_ms`, `max_ms`, `total_ms`) and
  `pending`/`pending_oldest_age_ms` for runs still retried in the background
  (should be 0).

Archive cursor reads (log pages, downloads, SSE catch-up) seek by frame sequence
(A06): the cost of a read depends on the chunks it returns, not on how many
chunks of the run precede it.

Retention:

- **Runs:** per-definition `keep_runs` (default `storage.keep_runs_default`
  = 10,000) and `keep_for` in days (default 7). Pruning a run removes its logs from
  both tiers.
- **Log archive:** chunks older than `logs.db_keep_for` in days (default 30) are
  pruned daily at `logs.db_prune_at` (default 03:30 local), in small batches.
  With `logs.db_max_size` set, the same sweep then removes the oldest chunks
  until the archive fits that many MiB. The log disk budget (below) applies
  independently and earlier, whenever its watermarks are crossed. Afterwards freed pages are returned
  to the filesystem (incremental auto-vacuum) and the WAL is truncated.

Monitor data-directory free space (the log disk budget above reacts to
low headroom, but only by deleting logs). The first start after upgrading to log
archive schema 3 runs a one-time `VACUUM` of `minicron-logs.db` to enable
incremental auto-vacuum; it needs temporary free space about the size of the
file. The main `minicron.db` does not shrink after run retention deletes rows;
if physical reclamation is needed there, stop the daemon, back up the
complete directory, and use SQLite `VACUUM` before restarting.

## Execution queue and pending work

Design and limits: [ADR-9](adr/0009-durable-execution-queue.md); settings: the
`[queue]` section of [configuration](configuration.md). A job trigger
(`schedule`, `manual`, `retry`) that finds `scheduler.max_concurrent_runs` full
is persisted as a run with status `queued` and started later, oldest first with
round-robin fairness across definitions, at most `queue.drain_rate` per second.
Workers and `[[init]]` jobs are never queued.

- **Outcomes you can see.** `queued` (waiting), then the usual run lifecycle with
  `started_at - queued_at` as the wait; or a terminal `skipped` run whose
  `end_reason` says why it never ran: `queue_expired` (waited longer than
  `queue.max_age`), `queue_full` (a limit was hit; scheduled/retry triggers only),
  `definition_removed` / `definition_disabled` (deleted, recreated, or disabled
  while queued), `retry_budget` (the definition now allows fewer retries),
  `overlap_skip`, `retry_dropped` (see below). Deleting or disabling a definition drops its queued runs at once. `POST /api/v1/runs/{id}/stop` on a queued run cancels it: status `stopped`, end reason `queue_cancelled`. A manual trigger that meets a full
  queue gets HTTP 429 `queue_full` with `Retry-After` and creates no run; if the
  queue cannot be persisted it gets 503 `queue_unavailable`. `wait=true` follows a queued run until it
  is terminal or the wait `timeout` ends (then `202` with the current run).
- **Restart.** Queued runs survive a restart; `pending`/`running` runs are marked
  `interrupted` as before and never re-queued, so a command that may have started
  is not replayed. A backlog drains at `drain_rate` into free slots and expires
  after `max_age`, so a long outage ends with expired records, not a start storm.
  There is no exactly-once guarantee. The current definition is used when an item
  is taken (an edit applies; the run records the revision that actually ran).
- **Retries.** Failed runs wait for `retry_delay` in one in-memory scheduler,
  capped by `queue.max_pending_retries`; they are still lost on restart. Beyond
  the cap a retry is dropped with evidence: an error log, the `dropped` counter
  and a `skipped` / `retry_dropped` run (attempt N+1, linked to the failed run).
- **Terminal-state writes.** If a run's terminal write fails, one background
  finalizer retries it, each item with its own 1s to 30s backoff (a permanently
  failing item never starves the others; three consecutive failures pause the
  pass). Up to 256 runs wait there. Beyond that a job retries inline for at most
  two minutes, holding its concurrency slot (new runs are throttled instead of
  memory growing), then is handed to the background map up to four times the
  cap; past that it keeps holding its slot, and `diagnostics.finalizers.inline`
  shows it. Workers always use the background map. Nothing is discarded, and
  anything unwritten at shutdown is marked `interrupted` by recovery.
- **Archive discovery.** The archiver keeps at most 4096 sealed run ids in memory;
  beyond that buffers stay on disk and the idle archiver sweeps the directory to
  find them (the periodic worker-flush sweep does too).

`GET /api/v1/daemon` shows all of it under `diagnostics`: `execution_queue`
(`depth`, `bytes`, `oldest_age_s`, limits, and `expired`, `rejected`,
`unavailable`, `dropped` counters since the daemon started), `pending_retries`
(`count`, `max`, `dropped`), `finalizers` (`background`, `inline`, `max`) and
`log_archive` (`queued`, `overflow`, `overflow_total`, `sealed_runs`,
`sealed_bytes`, computed per request from a bounded directory walk). Counters
reset at restart; the `skipped` run records are the durable evidence. Run
metrics (`/api/v1/metrics/runs`) count `queued` runs under `queued`. Alert when
`depth` stays near `max_items`, `expired` or `rejected` grow, or `sealed_bytes`
keeps rising while the archiver is up.

## Alert delivery

Monitor `GET /api/v1/metrics/alerts` (`counts.failed`, `counts.dropped`,
`counts.interrupted`, and `queue_depth`). Inspect a failed run at
`GET /api/v1/runs/{id}/alerts` for per-channel attempts and the last safe
error message. These are observations, not a durable delivery queue: after a
crash, in-flight alerts are marked `interrupted` but not replayed. Test an
individual channel with `POST /api/v1/alert-channels/{name}/test`.

`counts.dropped` (queue full or channel unavailable) is recorded off the
run-completion path: pending drop observations are held in a bounded list
(256) and written by the alert batch goroutine, so a slow database delays the
record, never the run. Under sustained overload drops beyond that list are
counted in the daemon log (`alert drop observations discarded`) but not
stored.

## Shutdown

On SIGTERM the daemon stops accepting work, cancels log maintenance, signals
worker supervision and the executor, stops the scheduler (5s), then stops runs
(15s grace before a forced kill), joins worker supervision (5s) and
maintenance (10s), drains alerts, and shuts down HTTP (5s). The stages before
alert drain share a 45s target; alert delivery always gets at least 5s and HTTP
and the log archiver keep small reserved slices, so the worst case is somewhat
over 45s. This is a target, not a hard guarantee: a run that survives a forced
kill, or file/fsync work that cannot be interrupted, can still overrun it. A
reload waiting on a worker's long `grace` is abandoned rather than waited for.

Log maintenance and archive calls honor the shutdown context. If a maintenance
loop still has not stopped after its 10s join, the daemon logs `maintenance
loops did not stop ... databases will be left open` and deliberately does **not**
close the SQLite databases under it; SQLite recovers from the unclosed WAL at
the next start. A trigger that races with shutdown can be refused with
`ErrShutdown` after its run row exists; that row is marked `failed`
(`start_error`) and, for API idempotency keys or scheduled occurrences, is not
replayed.

A worker whose terminal state cannot be written (storage fault) is finalized in
the background; its restart policy is applied once that succeeds. Queued runs
stay queued across shutdown and are not started by it.

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
| Runs stay `queued` / end `queue_expired` | `max_concurrent_runs` stays full for longer than `queue.max_age`; raise the limit, shorten jobs, or raise `max_age` (see Execution queue) |
| Jobs didn't fire while daemon was down | By design (`catch_up = "none"` default); use `catch_up = "latest"` to fire the most recent miss |

## Upgrade / rollback

Stop the daemon, back up the data directory (above), replace the binary,
start. Rollback = stop, restore the directory snapshot, restore the old
binary, start. Never downgrade a live newer-schema database.

**Upgrading to the log disk budget.** `logs.disk_min_free` is **on by default**
(512 MiB, clamped to a quarter of the filesystem). After upgrading, a host with
less than that free on the data directory's filesystem deletes the oldest
completed runs' archived logs (and, if that is not enough, sealed buffers) on the
first pass at startup; deleted logs are not recoverable. Before upgrading a small
or nearly full disk, check `df` for the data directory, and either free space,
size `logs.disk_budget` deliberately, or set `logs.disk_min_free = 0` to keep the
previous behavior. The daemon logs one `log disk budget:` INFO line at every
start and reload with the configured and effective values (`min_free_mib`,
`effective_min_free_mib`, `filesystem_mib`, `budget_mib`, quarantine policy); read
it after upgrading, and see [Log disk budget](#log-disk-budget).
