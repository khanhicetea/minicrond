# DECISIONS — ADR Log

Decisions move here once argued in `OPEN-QUESTIONS.md` and accepted by the
product owner. Never delete an entry; supersede with a new ADR that links back.

Template:

```
## ADR-N: <title>
- Date: YYYY-MM-DD
- Status: accepted | superseded by ADR-M
- Context: <why this needed deciding>
- Decision: <what we chose>
- Consequences: <what this costs/enables; who it affects>
```

---

## D-1..D-7: Product-owner constraints (accepted 2026-08-26)

Not debated — fixed inputs to every other decision.

| # | Constraint |
|---|---|
| D-1 | Single compiled binary (language of our choice) |
| D-2 | App data & metadata in a SQLite db file |
| D-3 | Docker pairing as process manager (nice to have) |
| D-4 | Optional log storage in SlateDB (LSM, S3-backed) — **amended by ADR-1**: optional S3-backed log storage via direct chunk writes |
| D-5 | Daemon may run as root; jobs/workers run as a chosen user |
| D-6 | Web UI simple & DX-friendly |
| D-7 | Settings import/export via web UI **and** file include syntax |

## ADR-0: Clean-room stance

- Date: 2026-08-26
- Status: accepted
- Context: `learn/` contains a reference product (RunWisp) studied for feature
  inspiration; its daemon/UI are GPL-3.0.
- Decision: minicron is designed and implemented from these original specs
  only. No source, schema, text, or identifier is copied from the reference;
  `learn/` stays gitignored and is never build input. Where designs resemble
  the reference (category-best-practice behavior like stop ladders), the spec
  text is our own and divergences are intentional.
- Consequences: no license contamination; we may choose any license (OQ-12);
  feature parity with the reference is not a goal — the specs are the goal.

## ADR-1: Log offload via direct S3 chunk storage; SlateDB dropped

- Date: 2026-08-26
- Status: accepted (product owner)
- Context: D-4 originally named SlateDB. Analysis: our log workload is
  append-only chunks, sequential range reads, and whole-run deletes — no
  mutations, no point lookups. An embedded LSM (memtables, WAL, compaction,
  bloom filters) serves workloads we don't have; compaction would rewrite
  write-once data for no benefit, while adding a young pre-1.0 dependency,
  resident memory, and version-pinning risk. The only SlateDB capability we
  would use is durable S3 I/O.
- Decision: store log chunks as plain S3 objects behind the same `LogSink`
  trait (`09-log-pipeline-and-s3.md`). SlateDB is removed from the stack;
  D-4 is amended to "optional S3-backed log storage (direct chunk writes)".
- Consequences: smaller binary and dependency surface; retention stays
  explicit (our sweeper + batched `DeleteObjects`, optional bucket lifecycle
  rule); the Rust-only coupling from SlateDB is released — OQ-1 reopens with
  Go as the standing recommendation (`02`).

## ADR-2: No TUI

- Date: 2026-08-26
- Status: accepted (product owner)
- Context: terminal UIs are a large surface (rendering, input handling,
  virtualization) serving a niche that SSH port-forwarding to the web UI
  largely solves.
- Decision: minicron ships exactly two interfaces — the web UI and the
  CLI. No TUI, ever.
- Consequences: smaller scope; the CLI gains run-log viewing (`minicron
  logs`) to keep headless/SSH workflows first-class.

## ADR-3: Provenance-locked single source of truth

- Date: 2026-08-26
- Status: accepted (product owner) — resolves OQ-3
- Context: a definition whose bytes live in a reviewed file must not be
  quietly mutable through a second channel (UI/API/DB) — that is how a
  file and reality drift apart. The earlier hybrid draft allowed UI edits
  of file-imported definitions with conflict flags; rejected as drift-prone.
- Decision: every definition has exactly one authority. `file` authority:
  the TOML file is the single source of truth; the DB stores only a
  reference (path, hash) plus a non-authoritative cached parse; metadata
  edits via UI/API/DB are rejected (409). `db` authority: created via
  UI/API/CLI, registry-authoritative, freely editable there. Converting
  db→file is an explicit, audited `takeover` at import time.
- Consequences: git-managed definitions are immutable outside git; the UI
  editor applies only to db-native definitions (read-only view + managing
  path otherwise); no conflict-resolution state machine at reload. Spec
  `05`.

## ADR-4: System mode — root daemon with registered users

- Date: 2026-08-26
- Status: accepted (product owner)
- Context: on shared hosts the daemon should run once, as root, while
  individual Unix users manage their own jobs/workers safely — extending
  D-5 rather than running N per-user daemons.
- Decision: add `[server] mode = "system"` (restart-only). Unix users run
  `minicron user register` over the system socket (kernel peer
  credentials prove identity — no passwords); each registered user gets a
  scoped namespace, may import file sources and create db-authority
  definitions (locked to `run_as` = self), and uses the CLI for listing
  (status, next fire, last run status), viewing run logs, triggering, and
  scoped reload. Reload validates sources independently so one broken file
  never blocks others.
- Consequences: multi-tenant authorization matrix, `users` table,
  owner-scoped definitions (`08`, `17`); users are CLI-first (UI namespace
  scoping v1.1); lands v0.2 with schema columns present (NULL) from v0.1.
  Spec `17`.

## ADR-5…n: reserved

The following are **pending** in `OPEN-QUESTIONS.md` and become ADRs when
accepted: OQ-1 (language), OQ-2 (config format), OQ-7 (auth), OQ-9 (UI
framework), OQ-12 (license).
