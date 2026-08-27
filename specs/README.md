# minicrond — Specification Suite

Working specs for **minicrond** (working title, see OQ-11): a single-binary cron
scheduler and process supervisor with a web UI, SQLite metadata, and optional
S3-backed log storage (direct chunk writes; SlateDB was considered and
dropped — ADR-1).

These documents are the product/design source of truth *before* code exists.
They are written to be argued with — every doc ends with **Open questions**
that are consolidated in [`OPEN-QUESTIONS.md`](OPEN-QUESTIONS.md).

## Clean-room note

`learn/` contains a reference product studied for feature inspiration
(RunWisp). It is gitignored and **no source, schema, text, or naming is copied**
from it. Everything in `specs/` is an original design that may deliberately
make different choices. Where a design diverges from the "obvious" approach
the reference product took, the doc says so and why.

## How to read / discuss

- Read in numeric order for a full pass; skim `00` → `05` for a product pass.
- Keyword levels: **MUST** (hard requirement), **SHOULD** (default expectation,
  may be traded off with a recorded reason), **MAY** (optional / later).
- Each spec has a `Status` header: `Draft` → `Proposed` → `Accepted`.
- To discuss: comment inline (`<!-- KS: ... -->` style is fine), or add/argue an
  item in `OPEN-QUESTIONS.md`. Once settled, record it in `DECISIONS.md` as an
  ADR and bump the spec status.
- Scope markers: `[v0.1]` core, `[v0.2]`, `[v0.3]`, `[v1.0+]` — see roadmap in
  `00-product-vision.md`.

## Document map

| Doc | Title | Status |
|---|---|---|
| [00](00-product-vision.md) | Product vision, goals, non-goals, roadmap | Draft |
| [01](01-domain-model.md) | Domain model & terminology | Draft |
| [02](02-technology-choices.md) | Language & stack choices | Draft |
| [03](03-system-architecture.md) | System architecture & process model | Draft |
| [04](04-configuration.md) | Configuration format & validation | Draft |
| [05](05-config-sync-import-export.md) | Config sync, import/export (UI + file include) | Draft |
| [06](06-scheduler.md) | Scheduler & cron engine | Draft |
| [07](07-execution-and-supervision.md) | Execution, supervision, run-as, stop/retry | Draft |
| [08](08-storage-sqlite.md) | SQLite storage & schema | Draft |
| [09](09-log-pipeline-and-s3.md) | Log pipeline, file backend, S3 backend | Draft |
| [10](10-docker-integration.md) | Docker pairing & container driver | Draft |
| [11](11-http-api.md) | HTTP API, SSE, OpenAPI | Draft |
| [12](12-web-ui.md) | Web UI | Draft |
| [13](13-security.md) | Auth, privileges, secrets, hardening | Draft |
| [14](14-cli.md) | CLI surface | Draft |
| [15](15-operations.md) | Install, service, backup, metrics | Draft |
| [16](16-notifications.md) | Notifications & events | Draft |
| [17](17-system-mode.md) | System mode: root daemon, registered users | Draft |
| [DECISIONS](DECISIONS.md) | Accepted decisions (ADR log) | — |
| [OPEN-QUESTIONS](OPEN-QUESTIONS.md) | Consolidated discussion agenda | — |

## Hard constraints (from product owner, 2026-08-26)

These are fixed inputs, not open for debate (recorded as D-1..D-7 in
`DECISIONS.md`):

1. One single compiled binary (Go, Rust, compiled Bun, … — choice is ours).
2. App data & metadata live in a SQLite database file.
3. Docker pairing as a process manager — nice to have.
4. Optional storage of cron/worker logs in SlateDB (LSM, S3-backed) —
   *revised by product owner 2026-08-26: direct S3 chunk storage instead
   (ADR-1)*.
5. Daemon can run as root; cron jobs and workers can run as a chosen user.
6. Web UI must be simple and developer-experience friendly.
7. Cron/worker settings importable/exportable via web UI **and** via a file
   include syntax.

Post-draft product-owner decisions, recorded as ADRs: **ADR-2** (no TUI),
**ADR-3** (provenance-locked single source of truth), **ADR-4** (system
mode — root daemon with registered unprivileged users).
