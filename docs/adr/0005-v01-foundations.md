# ADR-5: foundations

- Date: 2026-08-27 (status updated for 0.6.0, 2026-10-27)
- Status: accepted; partly superseded as noted

## Decision

minicrond uses Go (the version in `go.mod`), strict TOML, a handler-first HTTP API with a checked-in OpenAPI 3.1 contract, a React 19 SPA source (React Compiler, wouter, daisyUI 5, TanStack Query) with embedded generated assets, bearer-token TCP authentication, Unix-socket local authentication, port 7423, UTC defaults, and Apache-2.0. Execution is local; logs use the file backend.

SQLite is the sole definition authority. Browser, CLI, and API TOML imports explicitly copy into the registry; export provides portable TOML. Main-config `[[init]]`, `[[job]]` and `[[worker]]` entries are the one exception: they are file-owned and read-only in the UI. Runs use the non-overlapping terminal vocabulary recorded in [architecture](../architecture.md#run-lifecycle).

The original rule that lifecycle records commit before spawning is superseded by [ADR-16](0016-execution-first.md): history follows execution.

## Consequences

Retries, the durable queue ([ADR-9](0009-durable-execution-queue.md)), container operation ([ADR-16](0016-execution-first.md)), Telegram alerts ([ADR-13](0013-database-alert-channels.md)) and `service install/uninstall` were added after this record. Remote execution, S3 and built-in backups remain out of scope. Process groups are control for trusted local workloads, not containment.
