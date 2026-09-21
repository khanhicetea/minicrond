# 05 — Definition Registry, Import / Export

Status: implemented

## Single source of truth

SQLite is the only authoritative store for job and worker definitions. Every
create, edit, enable/disable, and delete operation changes the registry in one
transaction and records a revision and audit entry. The scheduler and worker
supervisor always reconcile from the registry.

The daemon TOML file contains daemon settings only: server, scheduler, storage,
logs, and alert channels. It cannot contain `[[job]]`, `[[worker]]`,
`[defaults]`, or `[include]`. Startup and SIGHUP reload daemon settings and
reconcile the runtime from the current registry; they never discover or watch
job files.

## TOML interchange

TOML is an explicit interchange format, not a live definition source. A bundle
may contain `[defaults]`, `[[job]]`, and `[[worker]]` tables.

```sh
minicrond import definitions.toml
minicrond export --format toml > definitions.toml
```

Import follows one path for CLI, API, and browser uploads:

1. Parse strictly and validate every definition.
2. Preview the exact uploaded bytes and return their SHA-256.
3. Apply only when the supplied content hash still matches.
4. Upsert the complete bundle in one transaction, incrementing revisions and
   writing audit records.
5. Reconcile the scheduler and worker supervisor from SQLite.

An existing name is updated; a new name is created. There is no link mode,
file authority, takeover, source tracking, or filesystem watcher. Changing or
deleting an imported file has no effect until the operator imports it again.

Export supports TOML bundles and canonical JSON. Runtime history and logs are
not part of definition export. Secrets are represented by their `env:` or
`file:` references; referenced secret values are never embedded.

## API surface

```
GET    /api/v1/export?format=toml|json
POST   /api/v1/import/preview
POST   /api/v1/import/apply
POST   /api/v1/daemon/reload
```

`POST /api/v1/daemon/reload` reloads daemon settings and reconciles all current
definitions from SQLite. It does not accept a source path.
