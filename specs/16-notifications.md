# 16 — Notifications & Events

Status: Draft · Deliberately minimal v1 [v0.2]

## Events (internal bus, also the SSE feed)

| Kind | Severity | Emitted when |
|---|---|---|
| `run.failed` | error | final attempt of a job failed |
| `run.succeeded_after_retries` | warn | succeeded but needed retries |
| `run.timeout` | error | timeout terminal |
| `worker.fatal` | error | worker slot gave up restarting |
| `worker.restarted` | info → warn at 3+ | crash restart |
| `disk.low` | warn | pre-run disk check tripped |
| `logs.unwritable` | error | active LogSink failing |
| `config.rejected` | error | reload validation failed (live set untouched) |
| `config.reload_failed` | warning | daemon settings reload failed validation |
| `docker.unavailable` | warn | docker driver runtime missing |

## Routing (v0.2)

- Per-definition `notify = ["on-failure", "on-timeout", "on-restart", …]`
  desugars to channel lists; `[notify]` global defaults; a `[[notify.route]]`
  table (match by kind/severity/label-glob → channels) exists in the schema
  for growth but v0.2 ships only the simple per-job form. Complexity budget
  spent elsewhere.
- **In-app inbox** (`events` table, spec `08`): always on, unread badge in
  UI, coalesced, capped retention. The zero-config channel.

## Channels (v0.2)

| Channel | Config | Notes |
|---|---|---|
| `inbox` | none | built-in |
| `webhook` | `url`, `headers`, `template_path?` | JSON POST: event, job, run_id, exit_code, duration, tail (last 3 log lines), deep-link URL if `server.external_url` set. 3 retries, exp backoff 1→30 s; 4xx (non-429) = drop + inbox note |

[v0.3+ candidates: smtp, slack-compatible webhook (the generic webhook +
a documented payload template covers it), telegram.] Channel additions are
additive config — no schema change.

## Coalescing

Fingerprint = (kind, job). Window 5 min: first occurrence delivers
immediately; subsequent occurrences increment a count; window close (or
every 10th) delivers a summary mentioning the count. Prevents the 3 AM pager
storm from a every-minute failing job. Outbound failures surface as
`inbox` entries (never a loop).

## Deep links

If `server.external_url` is set, payloads include
`{external_url}/runs/{id}` — click-through from Slack/email to the exact
failing run. (This is the reason `external_url` exists in config.)

## Open questions

- OQ-17: confirm v0.2 scope = inbox + generic webhook only (recommended);
  SMTP as first v0.3 channel.
- Event payload versioning: stamp payloads with `"v": 1` from day one so
  webhook consumers can depend on shape — agree?
