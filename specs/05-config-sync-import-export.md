# 05 — Definition Registry, Import / Export

Status: implemented

## Single source of truth

SQLite is authoritative for definitions created through the API or import.
The main TOML file is authoritative for its `[[init]]`, `[[job]]`, and
`[[worker]]` entries. Those entries are mirrored into SQLite in one
transaction at startup and reload so runs retain stable IDs and history.
The scheduler and worker supervisor reconcile from the registry.

The daemon TOML file contains server, scheduler, storage, logs, alert channels,
optional `[defaults]`, and optional config-owned definitions. It cannot
contain `[include]`. Bootstrap defaults
are copied into definitions when saved, not applied retroactively to registry
entries. Startup and SIGHUP reload the main file and reconcile the runtime.
Init entries run in order before serving on startup; reload does not rerun them.

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

An existing registry-owned name is updated; a new name is created. Import
cannot take over a config-owned name. There is no filesystem watcher. Changing
or deleting an imported file has no effect until the operator imports it again.

Export includes registry-owned entries and supports TOML bundles and canonical JSON. Runtime history and logs are
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
