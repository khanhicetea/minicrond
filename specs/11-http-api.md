# 11 — HTTP API

Status: Draft · Auth details: `13` · Default port 7423 (OQ-8)

## Principles

- **The UI is just a client.** Everything the web UI does is a public API
  call — no private endpoints. If curl can't do it, the UI shouldn't either.
- REST-ish resource URLs under `/api/v1`, JSON bodies, SSE for streams.
- Machine-observable contract: OpenAPI 3.1 document served at
  `/openapi.json`, generated from the same source as the handlers
  (`utoipa` if Rust per `02`), publishable to clients without post-editing.
- Errors are one envelope, always:

```json
{ "error": { "code": "validation_failed",
             "message": "schedule: unknown field 'sec'",
             "details": [ { "path": "jobs[0].schedule", "line": 4, "col": 3 } ] } }
```

## Versioning & compatibility

- Path-versioned (`/api/v1`). Within v1: additive changes only (new optional
  fields, new endpoints); removals/renames require `/api/v2` and a migration
  note. The frozen vocabulary rule from `01` applies doubly here.
- `GET /api/v1/daemon` reports `{ version, schema_version, instance_id,
  capabilities: ["local","docker","s3logs"], uptime_s }`.

## Endpoint map (v0.1 unless marked)

| Method & path | Purpose |
|---|---|
| `GET /healthz`, `GET /readyz` | liveness/readiness (no auth) |
| `GET /metrics` | Prometheus text (auth'd; separate bind optional — OQ-19) |
| `GET /api/v1/daemon` | daemon info (above) |
| `POST /api/v1/daemon/reload` | config reload (validate-first; errors → 422 with details) |
| `GET /api/v1/config/state` | import sources, hashes, staleness, last reload |
| `GET /api/v1/jobs` / `GET /api/v1/jobs/{name}` | list (filter kind/label/enabled) / detail incl. `next_fire_at`, `next_5_firings`, active runs |
| `POST /api/v1/jobs` / `PATCH /api/v1/jobs/{name}` / `DELETE /api/v1/jobs/{name}` | registry editing (spec `05`); PATCH = partial spec update, full validation |
| `POST /api/v1/jobs/{name}/trigger` | run now; body = params (v0.2); `?wait=true&timeout=120` sync mode returns final status |
| `POST /api/v1/jobs/{name}/enable` / `/disable` | convenience toggles |
| `POST /api/v1/workers/{name}/start` / `/stop` / `/restart` | instance controls |
| `GET /api/v1/runs` | filter: job, status, trigger, exit codes (`>100`), time range, attempt; sort; cursor pagination |
| `GET /api/v1/runs/{id}` / `POST /api/v1/runs/{id}/stop` | detail / stop ladder |
| `GET /api/v1/runs/{id}/log*` | windowed / raw / SSE stream — see `09` |
| `GET /api/v1/jobs/{name}/log/search` | bounded search [v0.2] |
| `GET /api/v1/events/stream` | SSE: run lifecycle, config staleness, notifications (numbered, replayable) |
| `GET /api/v1/export` / `POST /api/v1/import/preview` / `POST /api/v1/import/apply` | spec `05` |
| `GET /api/v1/notifications` / `POST .../read` | inbox [v0.2] |

### Semantics worth pinning

- **Trigger idempotency**: `POST /trigger` accepts optional `Idempotency-Key`
  header; duplicate key within 24 h returns the original run instead of a
  second one. Default (no key) always starts fresh — cron semantics, not
  queue semantics.
- **Sync trigger** (`?wait=true`): holds the request, streams nothing, returns
  `{run_id, status, exit_code, duration_ms}`; cap 240 s; for CI ergonomics
  the HTTP status is 200 for every completed run (including application failure), while a wait
  limit reached with the run still active returns 202. The CLI maps final run
  status/exit code for scripts.
- **Pagination**: cursor tokens (`?cursor=`, `?limit=` default 50 max 500);
  runs default order newest-first.
- **Concurrency responses**: triggering a `skip` job that's active returns
  200 with the `skipped` run (not an error — it's a recorded outcome);
  `queue` returns 202 with the `pending` run.
- **Rate limiting**: token-auth'd API is unthrottled v1 (single-tenant tool);
  login attempts throttled (see `13`).

## SSE conventions

- One event type per stream endpoint; log events carry stable per-run sequence IDs and resume from
  `Last-Event-ID`. Browser clients use authenticated `fetch()` streaming,
  never native EventSource or query-string tokens. Global events use
  boot-aware IDs and emit `resync` when replay is unavailable.
- Heartbeat comment every 15 s keeps proxies from idling out.
- Limits: max 64 concurrent SSE connections, 8 per source IP — generous for
  a single-machine tool, protective against runaway tabs.

## Auth (summary — full spec `13`)

- Local Unix socket channel: OS peer credentials, no token needed.
- TCP: `Authorization: Bearer <token>` (or `X-minicron-Token`); web UI uses
  the same token via a login screen → `sessionStorage` only (no cookie persistence v1;
  OQ-7).
- All endpoints except `/healthz` require auth when a token is configured
  (default on).
- Authorization: admin (socket peer = root, or admin token) sees all
  scopes; in system mode a user identity (peer uid or user token) is
  scoped to their own definitions (`17`). Metadata mutations
  (`POST`/`PATCH`/`DELETE` on jobs) apply **only** to `db`-authority
  definitions — a `file`-authority target returns `409 authority_conflict`
  naming the managing file (ADR-3).

## Open questions

- OQ-19: metrics on main port (recommended; simpler) vs optional separate
  bind for scrape-only networks.
- Webhook **outbound** API for events (deliver run events to external
  listeners) — that's notifications (`16`), inbound triggers are covered
  here. Confirm no inbound webhook trigger type is wanted v1 (`trigger` via
  API + token covers cron-less use cases).
