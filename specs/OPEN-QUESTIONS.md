# OPEN QUESTIONS — Discussion Agenda

The decision backlog. Each item lists its options, a recommendation, impact,
and where the spec lives. When one settles, move it to `DECISIONS.md` as an
ADR and update the referencing spec's Status.

## Big rock decisions (settle first — they gate implementation)

### OQ-1 — Implementation language
- Options: **Go** (recommended, post-ADR-1) / Rust / compiled Bun.
- Why: ADR-1 removed the Rust-only SlateDB coupling. Remaining hard
  constraints — per-run setuid/setgid (disqualifies Bun), static single
  binary, SQLite, SSE, S3 client — are met natively by both Go and Rust.
  Go wins on iteration velocity and ecosystem depth for an ops tool
  (cron/docker/fsnotify batteries, embedded tzdata, trivial cross-compile);
  Rust stays the pick if a memory/binary floor below Go's becomes a hard
  requirement.
- Impact: everything. Specs: [02](02-technology-choices.md).

### OQ-2 — Config file format
- Options: **TOML** (recommended) / YAML / JSONC.
- Why TOML: no indentation traps, multiline script strings, clean UI
  round-trip.
- Specs: [04](04-configuration.md).

### OQ-3 — ~~Source-of-truth model for definitions~~ — RESOLVED
- Resolved 2026-08-26 by ADR-3: provenance-locked authority. File-imported
  definitions are file-authoritative (DB stores a reference + cache; UI/API
  edits rejected); db-native definitions are editable via UI/API. Design:
  [05](05-config-sync-import-export.md).

### OQ-4 — Config table style
- Options: **`[[job]]` / `[[worker]]` arrays** (recommended — include files
  append naturally) / `[jobs.<name>]` keyed tables.
- Specs: [04](04-configuration.md) [01](01-domain-model.md).

### OQ-5 — Cron seconds field
- Options: **exclude** (recommended; sub-minute only via `@every 30s`) /
  accept 6-field.
- Why: positional 5-vs-6 ambiguity is the classic cron foot-gun.
- Specs: [06](06-scheduler.md).

### OQ-6 — Catch-up default
- Options: **`none`** (recommended — quiet, crond parity; missed runs
  recorded only) / `latest` / `all`.
- Specs: [06](06-scheduler.md).

### OQ-7 — Auth model
- Options: **single bearer token + local socket peer-auth** (recommended) /
  password + cookie sessions / both.
- Trade-off: token is simpler; cookies add CSRF surface and session state.
- Note: system mode (`17`) layers OS-identity users on top of this — the
  open question is only the *network/admin* credential shape.
- Specs: [13](13-security.md) [11](11-http-api.md) [17](17-system-mode.md).

### OQ-8 — Default port
- Options: **7423** (proposed) / anything else.
- Impact: docs, docker examples, muscle memory. Decide once, never change.
- Specs: [04](04-configuration.md) [11](11-http-api.md).

### OQ-9 — Web UI framework
- Options: **Svelte 5 + Vite static** (recommended) / SolidJS / Preact.
- All meet the constraints (< 150 KB, SSE-friendly, embedded static).
- Specs: [12](12-web-ui.md) [02](02-technology-choices.md).

### OQ-10 — ~~SlateDB spike + version pin~~ — RESOLVED
- Resolved 2026-08-26 by ADR-1: SlateDB dropped; log offload is direct S3
  chunk storage (design: [09](09-log-pipeline-and-s3.md)). Only surviving
  item is chunk-size tuning (256 KiB vs 1 MiB), moved to the parking lot.

## Product & scope decisions

### OQ-11 — Product name
- Options: `minicrond` (repo working title) / something else.
- Recommendation: keep `minicrond` through v0.1; rename is cheap pre-release.
- Impact: binary name, config paths, ports branding, crate/repo. Specs: everywhere.

### OQ-12 — License
- Options: Apache-2.0 / MIT / dual MIT-Apache.
- Recommendation: Apache-2.0 (patent grant; ecosystem norm).
- Specs: [02](02-technology-choices.md) [13](13-security.md).

### OQ-13 — File-watch auto-reload
- Options: opt-in `watch = true` / manual-only / default-on.
- Recommendation: opt-in. Default-on surprises git-checkout-heavy users.
- Specs: [05](05-config-sync-import-export.md).

### OQ-14 — Docker phasing
- Options: job driver (Mode C) in v0.2 with Mode B/compose-import in v0.3 /
  all in v0.3 / more in v0.2.
- Recommendation: Mode C in v0.2 (high value, small surface).
- Specs: [10](10-docker-integration.md).

### OQ-15 — Multi-host as non-goal
- Options: confirm non-goal v1 / design hooks now.
- Recommendation: confirm non-goal; the per-daemon S3 prefix layout (one
  prefix per daemon) is the only forward-hook we keep.
- Specs: [00](00-product-vision.md) [09](09-log-pipeline-and-s3.md).

### OQ-16 — `on_overlap` default
- Options: `skip` / `parallel` (classic crond).
- Recommendation: `skip` — silently stacking runs is the classic cron
  foot-gun; parallel is one explicit line away.
- Specs: [06](06-scheduler.md) [04](04-configuration.md).

### OQ-17 — Notification scope v0.2
- Options: inbox + generic webhook / + SMTP / + Slack.
- Recommendation: inbox + webhook (generic webhook covers Slack-compatible
  endpoints with a payload template).
- Specs: [16](16-notifications.md).

### OQ-18 — `prune_missing` default
- Options: `false` (definitions from vanished include files get disabled) /
  `true` (removed).
- Recommendation: `false` — destructive defaults are wrong for a config sync.
- Specs: [05](05-config-sync-import-export.md).

### OQ-19 — Metrics endpoint placement
- Options: same port, auth'd / optional separate bind.
- Recommendation: same port v1; add separate bind only if scrape-network
  users ask.
- Specs: [11](11-http-api.md) [15](15-operations.md).

### OQ-20 — Export bundle default format
- Options: single TOML file / ZIP + manifest per definition.
- Recommendation: single TOML default (diffable, paste-able), ZIP behind
  `--bundle`.
- Specs: [05](05-config-sync-import-export.md).

## Parking lot (small, decide during implementation)

- `retry_on_timeout` default (`07`), `max_restart_attempts` naming (`07`).
- Monthly partitioning of `runs` at 10M+ rows (`08`).
- `spec` stored as canonical JSON vs TOML text (`08`).
- Log chunk size 256 KiB vs 1 MiB (`09`; S3 request economics vs tail
  latency — measure with a chatty worker).
- `log_on_full` per-definition vs global (`09`).
- `trigger` as hidden alias of `minicrond run` (`14`).
- npm/bun distribution shim in v0.2 (`15`).
- Webhook payload `"v": 1` stamping (`16`).
- Multiple tokens / read-only token (`13`, v2 reservation).

## Discussion log

- 2026-08-26 — Spec suite drafted; all items above open; nothing accepted
  beyond D-1..D-7 and ADR-0.
- 2026-08-26 — ADR-1 accepted: SlateDB dropped in favor of direct S3 chunk
  storage; OQ-10 resolved; OQ-1 recommendation updated to Go.
- 2026-08-26 — ADR-2 (no TUI), ADR-3 (provenance-locked single source of
  truth — resolves OQ-3), ADR-4 (system mode with registered users, spec
  `17`) accepted; `minicrond logs` promoted to v0.1.
