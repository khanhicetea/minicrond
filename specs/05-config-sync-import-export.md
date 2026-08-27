# 05 — Config Sync: Authority, Import / Export

Status: Draft (revised by ADR-3) · Model: provenance-locked authority

This spec satisfies hard constraint D-7 (settings flow in and out via file
include syntax and the web UI) under the single-source-of-truth rule:
**every definition has exactly one authority, and only that authority can
change it.** OQ-3 is resolved by ADR-3.

## Authority model

| | `file` authority | `db` authority |
|---|---|---|
| Created by | include / import from a TOML file | web UI / API / CLI create |
| Source of truth | **the file** | **the SQLite registry** |
| What the DB stores | a reference (path, sha256, provides-list) + a cached parse kept only for run-history linkage | the definition + its revisions (authoritative) |
| Metadata edits via UI / API / direct DB | **rejected** — `409 authority_conflict`, error names the managing file | allowed → new revision + audit entry |
| How you edit it | edit the file, then `reload` | UI editor / API `PATCH` |
| `enabled` toggle | the `enabled` key in the file governs | UI / API toggle |
| Delete | remove from the file (see prune rules) | UI / API delete (soft) |
| Operational actions (trigger, stop, restart instance) | allowed for any authorized actor | same |

The cached parse of a file-authority definition is never allowed to become
authoritative: every boot and reload re-reads the files first, and the
cache is overwritten unconditionally. If the file is gone, the cache does
not resurrect the definition (prune rules below).

### Sync rules

| Situation | Behavior |
|---|---|
| File defines X; registry has X (file authority, same hash) | no-op |
| File defines X; registry X (file authority) differs | **file wins** — this *is* the edit path; workers restart only if the spec actually changed |
| File defines X; registry X is db-authority | conflict — import preview flags it; applying requires an explicit `takeover` (converts db → file authority) or `skip`. Never silent |
| File that provided X disappears | X disabled + warning (default) or removed if `prune_missing = true` (OQ-18) |
| UI/API metadata edit on file-authority X | 409 — "managed by `<path>`: edit the file and reload" |
| Two files define the same name (same owner scope) | validation error; only that source is rejected (per-source isolation, `17`) |

Rationale (vs. the earlier hybrid draft): a definition whose bytes live in
a reviewed file must not be quietly mutable through a second channel —
that's how file and reality drift apart. The UI stays fully capable for
db-authority definitions, and `export` always produces files, so nothing
is locked in.

## File include syntax

```toml
[include]
paths = [
  "jobs/*.toml",              # glob, relative to the including file
  "/etc/minicrond/shared/*.toml",
]
prune_missing = false
```

- Include files contain `[[job]]` / `[[worker]]` tables only (plus optional
  per-file `[defaults]` overlay scoped to that file). Daemon-level blocks in
  an include = validation error pointing at the file.
- Nested includes — **not supported** v1; one level, one bootstrap file.
- Each import records: path, mtime, sha256. The API/UI exposes a
  **staleness flag**: new files matching a glob, or hash drift vs last
  import, → "config stale, reload?" banner.
- File watch (OQ-13): opt-in `watch = true`; debounced 500 ms; the standard
  validate-first reload; a validation failure keeps the live set and
  surfaces a scoped error.
- In **system mode**, each registered user owns additional file sources via
  `minicrond import` — same rules, scoped to their namespace (spec `17`).

## Web UI import/export

**Export** (Settings → Import/Export):

- Scope: everything / kind filter (`jobs`|`workers`) / label filter /
  multi-select by name.
- Formats:
  - **TOML bundle** (default): single file of `[[job]]`/`[[worker]]` blocks
    with source comments (`# from jobs/backup.toml`), or a ZIP with one
    file per definition + `manifest.json` (name, hash, revision, authority,
    minicrond version) — OQ-20.
  - **JSON**: canonical definition JSON (same shape as the API), for
    scripts and diffing.
- Secrets are **never exported** — `secret_env` values export as
  `${file:/run/secrets/...}` references with a header comment listing what
  must be provisioned manually.
- Run history is not part of settings export; separate `minicrond export
  --runs` (JSONL/CSV) [v0.2].

**Import** (same page, or `minicrond import <path>`):

1. Paste or upload file(s).
2. Parse + validate. Errors (with positions) block that item only.
3. **Dry-run diff table**: per name → `new` / `changed` (field-level diff
   view) / `unchanged` / `conflict` (registry entry is db-authority).
4. Operator resolves conflicts: `skip` (default) / `takeover` (converts
   db-authority → file authority; the conversion is audited) /
   `rename-import` (`backup-db → backup-db-2`). Bulk-apply per column.
5. Apply → single transaction → file-authority references upserted,
   revisions/audit written where applicable → live reconciliation (same
   diff classes as reload) → summary with "trigger now" links.

## API surface (details in `11`)

```
GET    /api/v1/export?kind=&label=&format=toml|json
POST   /api/v1/import/preview   (multipart or JSON body → diff, no changes)
POST   /api/v1/import/apply     (items + conflict resolutions → result)
GET    /api/v1/config/state     (sources, hashes, staleness, last reload)
POST   /api/v1/daemon/reload
```

`POST/PATCH/DELETE /api/v1/jobs…` succeed **only** for db-authority
definitions; file-authority targets return `409 authority_conflict` with
the managing path (`11`).

## Git workflow (the blessed path for file-managed definitions)

```
repo/
  minicrond.toml            # bootstrap: [server], [include]
  jobs/*.toml               # reviewed in PRs — the only edit channel
  workers/*.toml
```

`minicrond import --dry-run jobs/proposed.toml` validates in CI; on merge,
a reload picks it up. Ad-hoc db-authority definitions made in the UI can be
materialized back via `minicrond export --format toml` for review.

## Migration aids

- `minicrond import --from crontab <file|user>` [v0.2]: converts user/system
  crontabs to `[[job]]` TOML with `# TODO` comments for anything ambiguous
  (`%` semantics, redirections). Jobs import **disabled** until reviewed.
  No masking/disabling of system cron — the operator's explicit call.
- `minicrond import --from compose <file>` [v0.3]: compose services →
  `[[worker]]` definitions using the docker driver (spec `10`).

## Open questions

- OQ-18: `prune_missing` default false (disable, keep history) — confirm.
- OQ-20: bundle format default (single TOML vs ZIP+manifest).
- On `takeover`, keep prior db revisions visible in history? Draft: yes —
  audit records the authority conversion.
