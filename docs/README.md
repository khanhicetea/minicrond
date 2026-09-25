# minicrond documentation

minicrond is a small, single-binary cron scheduler and process supervisor
with durable run history, tagged compressed logs, a REST + SSE API, and an
embedded web UI. Linux is the supported platform.

| Doc | Contents |
|---|---|
| [getting-started.md](getting-started.md) | Build, first daemon, first job, web UI login, systemd install |
| [configuration.md](configuration.md) | Bootstrap TOML reference + full job/worker definition reference |
| [cli.md](cli.md) | Every CLI command, flags, and environment variable |
| [http-api.md](http-api.md) | HTTP API surface, auth model, SSE log streaming, OpenAPI |
| [operations.md](operations.md) | Runbook: data layout, hybrid log storage, backup/restore, retention, tokens, security, troubleshooting |
| [architecture.md](architecture.md) | Components, run lifecycle, storage design, design decisions |
| [adr/](adr/) | Accepted architecture decision records |
| [releases/](releases/) | Release notes and supporting acceptance tables / measurements |

Design-time specifications (drafted before implementation) live in
[`specs/`](../specs/). Where a draft conflicts with the documents here or
with an accepted ADR, these documents and the ADRs govern.

The machine-readable contracts are checked in and CI-verified:

- [`schema/minicron.schema.json`](../schema/minicron.schema.json) — JSON
  Schema for the bootstrap config (also served by `minicrond schema` and
  embedded in the binary for the UI editor).
- [`cmd/minicrond/openapi.json`](../cmd/minicrond/openapi.json) — OpenAPI
  3.1 contract for the HTTP API (also served at `/openapi.json`).

A hands-on sandbox with real jobs, start/stop scripts, and an end-to-end
smoke test lives in [`examples/local-test`](../examples/local-test/README.md).
