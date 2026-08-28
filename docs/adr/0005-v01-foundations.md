# ADR-5: v0.1 foundations

- Date: 2026-08-27
- Status: accepted

## Decision

v0.1 uses Go 1.24, strict TOML, a handler-first HTTP API with a checked-in OpenAPI 3.1 contract, a React 19 SPA source (React Compiler, wouter, daisyUI 5, TanStack Query) with embedded generated assets, bearer-token TCP authentication, Unix-socket local authentication, port 7423, UTC defaults, and Apache-2.0. Only user mode, local execution, file logs, `skip|parallel`, and `none|latest` catch-up ship.

Definitions have file or DB authority. Browser/`--copy` imports create DB-authority copies; `--link` adds a daemon-local include. Runs use the non-overlapping terminal vocabulary in `codex-decisions.md`. Lifecycle records commit before spawning or returning success.

## Consequences

System mode, retries, queue/replace, notifications, containers, S3, backups, and service installation remain later work. Process groups are control for trusted local workloads, not containment. The existing draft specs are interpreted through `codex-decisions.md` where they conflict.
