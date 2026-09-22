# HTTP API

Base URL: the daemon's `server.bind` (default `http://127.0.0.1:7423`).
The full OpenAPI 3.1 contract is checked in at
[`cmd/minicrond/openapi.json`](../cmd/minicrond/openapi.json) and served
live at `GET /openapi.json`.

## Authentication

| Transport | Auth |
|---|---|
| TCP (`server.bind`) | `Authorization: Bearer <token>` required for **every** `/api/` path |
| Unix socket (`minicron.sock`) | Token-free; the peer's local credentials authorize the request |

Public without a token: `GET /healthz`, `GET /readyz` (no detail
disclosed), `GET /openapi.json`, and the web UI shell + bundled assets.
Everything else under `/api/` rejects unauthenticated requests.

The initial token is written once to `initial-token` in the data directory
(mode 0600) on first boot. Rotate with `POST /api/v1/token/rotate` or
`minicrond token --rotate`; the response contains the new token, and the
old one stops working immediately. The daemon status exposes only a token
fingerprint.

## Endpoints

### Daemon

| Method & path | Description |
|---|---|
| `GET /healthz` | Liveness — always `ok` |
| `GET /readyz` | Readiness — fails while shutting down |
| `GET /api/v1/daemon` | Version, schema, uptime, token fingerprint, capabilities |
| `POST /api/v1/daemon/reload` | Hot-reload the bootstrap config |

### Definitions

| Method & path | Description |
|---|---|
| `GET /api/v1/jobs` | List all job & worker definitions (with next fire time) |
| `POST /api/v1/jobs` | Create/replace a definition (schema-validated) |
| `GET /api/v1/jobs/{name}` | One definition |
| `PUT /api/v1/jobs/{name}` | Update a definition |
| `DELETE /api/v1/jobs/{name}` | Delete a definition |
| `POST /api/v1/jobs/{name}/trigger?wait=true` | Trigger a run now (`wait` blocks for the result) |
| `POST /api/v1/jobs/{name}/enable` | Enable |
| `POST /api/v1/jobs/{name}/disable` | Disable (scheduler skips it) |
| `POST /api/v1/workers/{name}/start` | Start a worker |
| `POST /api/v1/workers/{name}/stop` | Stop a worker (graceful, then kill) |
| `POST /api/v1/workers/{name}/restart` | Restart a worker |

### Runs

| Method & path | Description |
|---|---|
| `GET /api/v1/runs` | Search/filter run history (job, status, time window) |
| `GET /api/v1/runs/{id}` | Full run record |
| `POST /api/v1/runs/{id}/stop` | Operator stop (status `stopped`) |
| `GET /api/v1/runs/{id}/log?after=N&limit=N` | Tagged log frames (JSON, base64 payload, stream tag) |
| `GET /api/v1/runs/{id}/log/raw` | Raw merged bytes (download) |
| `GET /api/v1/runs/{id}/log/stream` | **SSE** live tail — frames as they are ingested |

`GET /api/v1/metrics/runs?range=15m|1h|24h|7d|30d&buckets=N` — run-count
time series by outcome.

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
across the hybrid tiers — SQLite archive, live buffer files, and the
byte-bounded in-memory tail are merged transparently, so a run looks the
same whether it finished an hour ago or is streaming right now. The SSE
stream replays from sequence 0 and then follows live output.

## Errors

Errors are structured JSON (`{"error": {"code", "message"}}`) with proper
status codes: `400` malformed, `401` unauthenticated, `404` unknown
resource, `409` state conflict (e.g. overlapping import), `422` validation
failure with field detail.
