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
| [adr/](adr/) | Accepted architecture decision records, including [ADR-8](adr/0008-background-first-trade-offs.md) (background-first trade-offs and implementation status), [ADR-9](adr/0009-durable-execution-queue.md) (durable execution queue), [ADR-10](adr/0010-log-disk-budget.md) (log disk budget) and [ADR-11](adr/0011-read-admission.md) (read admission) [ADR-16](adr/0016-execution-first.md) (execution first: runs never wait for history storage) [ADR-17](adr/0017-gc-pacing.md) (GC pacing and the commit-cost findings) [ADR-18](adr/0018-relaxed-commits.md) (running transition and schedule state committed without their own fsync) [ADR-19](adr/0019-history-unavailable-policy.md) (per-job choice to skip a run whose row cannot be stored) and [ADR-20](adr/0020-log-archive-write-path.md) (log archive write path: file chunks, batched commits, deferred seal syncs) |
| [benchmarks/](benchmarks/) | Measured results; start with [workload tiers](benchmarks/2026-10-25-workload-tiers/README.md) (CPU, memory and I/O for tiny, small, normal and large setups) and [memory and commit costs](benchmarks/2026-10-25-memory-and-commits/README.md) |
| [releases/](releases/) | Release notes and supporting acceptance tables / measurements |

Older design drafts and audits were retired; they remain in git history.

The machine-readable contracts are checked in and CI-verified:

- [`schema/minicron.schema.json`](../schema/minicron.schema.json) — JSON
  Schema for the bootstrap config (also served by `minicrond schema` and
  embedded in the binary for the UI editor).
- [`cmd/minicrond/openapi.json`](../cmd/minicrond/openapi.json) — OpenAPI
  3.1 contract for the HTTP API (also served at `/openapi.json`).

A hands-on sandbox with real jobs, start/stop scripts, and an end-to-end
smoke test lives in [`examples/local-test`](../examples/local-test/README.md).
