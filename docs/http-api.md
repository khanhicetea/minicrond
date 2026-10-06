# HTTP API

Base URL: the daemon's `server.bind` (default `http://127.0.0.1:7423`)
when TCP is enabled. The same routes are served on the private Unix socket.
The full OpenAPI 3.1 contract is checked in at
[`cmd/minicrond/openapi.json`](../cmd/minicrond/openapi.json) and served
live at `GET /openapi.json`. If `BASE_PATH=/tools/minicron` is set on the
daemon, prepend `/tools/minicron` to every route in this document and open the
web UI at `/tools/minicron/`.

## Authentication

| Transport | Auth |
|---|---|
| TCP (`server.bind`, enabled by default) | `Authorization: Bearer <token>` required for every `/api/` path and `/openapi.json` |
| Unix socket (`minicron.sock`) | Token-free; the daemon UID or root is authorized by peer credentials |

Public without a token over TCP: `GET /healthz`, `GET /readyz` (no detail
disclosed), and the web UI shell + bundled assets.
Everything else under `/api/` rejects unauthenticated requests.

The initial token is written once to `initial-token` in the data directory
(mode 0600) on first boot. Rotate with `POST /api/v1/token/rotate` or
`minicrond token --rotate`; the response contains the new token, and the
old one stops working immediately. The daemon status exposes only a token
fingerprint. In Unix-only mode no new token is created, rotation returns
404, and the daemon status omits the fingerprint. A hash from an earlier
TCP-enabled period is retained and becomes usable if TCP is re-enabled;
rotate it from the local CLI after re-enabling if the old token is unknown.

The embedded UI probes `GET /api/v1/daemon` without a bearer token before
showing login. A successful probe permits token-free UI requests only when
`tcp_enabled` is false. A 401 keeps the normal TCP token login; later 401s
clear browser data and end the session. The UI never sends a stored token
while probing or using proxy access.

An HTTP reverse proxy to the Unix socket must authenticate and authorize
**every** request, including the SPA, API, assets, downloads, and SSE. It must
protect state-changing requests against cross-site requests, isolate each
daemon on its own origin, and deny `/api/v1/token/rotate`. Proxy access is
full operator access: it can create commands that run as the daemon UID.
Use `tcp_enabled = false` for browser proxy deployments. The daemon denies
direct framing with `X-Frame-Options: DENY` and CSP `frame-ancestors 'none'`;
a trusted embedding proxy must replace the former and the latter directive
with a narrow framing policy, preserving the other CSP restrictions. Adding
a second CSP header cannot relax the daemon's framing restriction.

## Endpoints

### Daemon

| Method & path | Description |
|---|---|
| `GET /healthz` | Liveness — always `ok` |
| `GET /readyz` | Readiness — fails while shutting down |
| `GET /api/v1/daemon` | Version, schema, uptime, `tcp_enabled`, capabilities, token fingerprint when TCP is enabled |
| `POST /api/v1/daemon/reload` | Hot-reload the bootstrap config |

### Definitions

| Method & path | Description |
|---|---|
| `GET /api/v1/jobs` | List all job & worker definitions (with next fire time) |
| `POST /api/v1/jobs` | Create/replace a definition (schema-validated) |
| `GET /api/v1/jobs/{name}` | One definition |
| `PUT /api/v1/jobs/{name}` | Update a definition |
| `DELETE /api/v1/jobs/{name}` | Delete a definition |
| `POST /api/v1/jobs/{name}/trigger?wait=true` | Trigger a run now (`wait` blocks for the result). `202` with status `queued` when `max_concurrent_runs` is full and the run was durably queued (with `wait=true` a queued run is polled until it starts and finishes, within `timeout`; on timeout the non-final `202` run is returned, and `minicrond run --wait` keeps polling until it is terminal); `429 queue_full` (with `Retry-After`) when the bounded queue refuses it; `503 queue_unavailable` when it cannot be persisted |
| `POST /api/v1/jobs/{name}/enable` | Enable |
| `POST /api/v1/jobs/{name}/disable` | Disable (scheduler skips it) |
| `POST /api/v1/workers/{name}/start` | Start a worker |
| `POST /api/v1/workers/{name}/stop` | Stop a worker (graceful, then kill) |
| `POST /api/v1/workers/{name}/restart` | Restart a worker |

`POST /api/v1/jobs` and `PUT /api/v1/jobs/{name}` accept
`If-None-Match: *` for an atomic create-only operation. An existing or
previously deleted name returns 412 without replacing it. Updates may use
the existing `If-Match: <revision>` precondition; the two headers cannot
be combined.

### Runs

| Method & path | Description |
|---|---|
| `GET /api/v1/runs` | Search/filter run history (job, status, time window) |
| `GET /api/v1/runs/{id}` | Full run record |
| `GET /api/v1/runs/{id}/alerts` | Per-channel delivery status, attempts, last safe error |
| `POST /api/v1/runs/{id}/stop` | Operator stop (status `stopped`) |
| `GET /api/v1/runs/{id}/log?after=N&limit=N` | Tagged log frames (JSON, base64 payload, stream tag); `limit` is an upper bound (1–5000): a page also stops at about 1 MiB of payload (one larger line is returned alone), so fewer than `limit` items does **not** mean the end — continue with `after` = the last sequence until `items` is empty |
| `GET /api/v1/runs/{id}/log/raw` | Raw merged bytes (download), read and sent page by page |
| `GET /api/v1/runs/{id}/log/stream` | **SSE** follow — frames read from stored chunks by cursor, delivered in batches about every 2 s while the run is active |

Completed job and worker runs optionally include `resource_usage`:
`user_cpu_us`, `system_cpu_us`, and `peak_rss_bytes`. These are kernel exit
accounting, persisted with the terminal transition, not live samples. CPU may
include waited-for descendants; peak RSS is **not** the combined process-tree
peak. Historical, unspawned, or crash-interrupted runs omit this object;
measured zero is distinct from unavailable.

### Live resource monitoring

`GET /api/v1/monitor` returns `supported`, `sampled_at`, `daemon_pid`,
`daemon` (process sample or null), `heap_bytes` (Go heap objects), `goroutines`,
`active`, `truncated`, and `items` (at most 256 active job/worker identities
with nullable `stats`). A process sample contains `sampled_at`,
`process_start_id`, cumulative `cpu_us`, and current `rss_bytes`.

Live process stats require Linux procfs and cover the **direct child only**.
Missing/unreadable/exited processes and identity mismatches have null stats.
Compute interval CPU percentage as `100 × delta(cpu_us) / delta(time_us)`
only for the same process-start identity; 100% means one logical CPU. The first
sample has no percentage. Do not interpret null as zero.

The `/monitor` UI polls every 3 seconds only while visible; samples are never
saved. API snapshots are shared for 3 seconds and then released, with no
background sampler. Collection uses the shared read gate (1 MiB reservation,
no wait), one collector, and a 2-second cooperative work deadline. Busy reads
return `503` with `Retry-After`. A truncated response is a bounded subset, not
a stable page. See [ADR-12](adr/0012-resource-monitoring.md).

`GET /api/v1/metrics/runs?range=15m|1h|24h|7d|30d&buckets=N` — run-count
time series by outcome. Concurrent requests for the same `range`/`buckets`
share one computation, and a result is reused for about 3 seconds, so
`total` and the series can lag by that much; nothing is cached while idle.

### Expensive reads and `503`

JSON log pages, raw downloads and run metrics share one small admission gate
(`[reads]` in [configuration.md](configuration.md)). When it is full the
request waits briefly, then gets `503` with `Retry-After` (seconds) and error
code `read_busy`; a request whose read or aggregation exceeds its work budget
gets `503` `read_timeout`. Both are retryable and never mean the run or daemon
is unhealthy. A download that has already started is never cut off by
admission; if reading fails after the first bytes were sent, the connection is
aborted so the file cannot look complete. The SSE stream is limited separately
(`503` `stream_capacity`).

### Alerts

| Method & path | Description |
|---|---|
| `GET /api/v1/alert-channels` | Configured channel names, types, batch windows (never secrets) |
| `POST /api/v1/alert-channels/{name}/test` | Send a synthetic test message immediately |
| `GET /api/v1/metrics/alerts` | Counts by delivery status and outstanding delivery depth |

### Config portability

| Method & path | Description |
|---|---|
| `GET /api/v1/export?format=toml|json` | Export all definitions as a bundle |
| `POST /api/v1/import/preview` | Validate a bundle; returns a content hash |
| `POST /api/v1/import/apply` | Apply a bundle (content + hash from preview) |

### Tokens

| Method & path | Description |
|---|---|
| `POST /api/v1/token/rotate` | Issue a new token, revoke the old |

## Log frames

Frames carry `sequence` (per-run, gapless accounting), `stream` (`1` =
stdout, `2` = stderr), and base64-encoded `payload`. Reads are consistent
across the hybrid tiers — SQLite archive and live buffer files are merged
transparently, so a run looks the same whether it finished an hour ago or is
streaming right now. The SSE stream replays from the `after` cursor (or
`Last-Event-ID`; default sequence 0) and then follows the run by polling stored
output, so display lag is up to about two seconds. Events: `line` (`id` = frame
sequence), `backlog_done`, `done` (sent only after the run's final frames), and
`gap` (`{"after":N,"first":M}`, `id` = M-1) when frames between the cursor and
`M` were removed by retention, or — rarely — when the one frame being written at
the moment log storage failed consumed a sequence number but was not stored;
reconnecting with the `gap` id resumes without a gap. `dropped` is no longer sent by the server (no per-viewer queue exists
that could overflow); clients may still handle it. Sequences can skip when a
run's retained frames were evicted by `log_on_full = drop_old`.

## Errors

Errors are structured JSON (`{"error": {"code", "message"}}`) with proper
status codes: `400` malformed, `401` unauthenticated, `404` unknown
resource, `409` state conflict (e.g. overlapping import), `422` validation
failure with field detail, `429` the execution queue is full (`queue_full`,
with `Retry-After`), `503` the daemon is not ready or the queue cannot be
persisted (`queue_unavailable`).

Run `status` also includes `queued` (waiting for capacity, not yet started); a
queued run that never starts ends `skipped` with an `end_reason` such as
`queue_expired` (see the operations runbook). `GET /api/v1/daemon`
`diagnostics` reports `execution_queue`, `pending_retries`, `finalizers` and
`log_archive`.
