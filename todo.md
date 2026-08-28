# minicron implementation TODO

This plan follows `specs/` for product intent and uses `codex-decisions.md` to resolve scope and implementation conflicts. Keep the core small: each release must be operable on its own before adding the next layer.

## Rules for every release

- [ ] Update ADRs/specs before implementing behavior that changes a contract.
- [ ] Keep config, API, CLI, UI, and persisted state on the same vocabulary.
- [ ] Add migrations and upgrade tests for every storage change.
- [ ] Cover new state transitions, failure paths, authorization boundaries, and crash recovery with automated tests.
- [ ] Regenerate and verify config schema, OpenAPI, CLI help, and user documentation.
- [~] Measure startup, memory, binary size, scheduler cost, and log throughput; treat budgets as goals until proven. *(Binary size and log-frame throughput measured in `docs/v0.1-measurements.md`; startup/RSS/scheduler-cost measurements need a supported host.)*
- [~] Ship checksums, signed artifacts, release notes, and rollback/restore guidance. *(Checksum+cosign script, release notes, and rollback guidance exist; actual signing requires a release key.)*

## Before v0.1 — make the design implementation-ready

- [x] Record the settled choices as ADRs: Go, TOML, Huma, React 19, bearer-token auth, port 7423, Apache-2.0, UTC defaults, and reduced release scope.
- [x] Apply the consistency corrections from `codex-decisions.md` across the specs, especially run states, reload boundaries, import modes, scheduling persistence, process guarantees, logging, SQLite identity, and SSE authentication/resume. *(Spec diffs applied: non-overlapping run statuses, UTC default + persisted `@every` anchors, deferred `all`/`queue`/`replace`, `--link`/`--copy` import modes with hash-bound apply, versioned frame wire format, backlog-to-live broadcaster, 472 removal.)*
- [x] Write normative state-machine and acceptance-test tables for runs, workers, reloads, scheduling, logs, and retention.
- [x] Establish the repository layout, Go module, React build, embedded assets, build metadata, linting, race tests, vulnerability checks, and cross-platform CI.
- [x] Create migration, config-schema, and OpenAPI generation/checking workflows before feature code depends on them.
- [x] Check the `minicron` name for package, domain, and trademark conflicts. *(Findings in `docs/name-check.md`: binary name usable, module path must move to an owned namespace, domain/trademark checks remain manual pre-release.)*

## v0.1 — trustworthy local MVP

### Core daemon and persistence

- [x] Implement user-mode startup, data-directory locking, SQLite migrations/WAL setup, readiness, graceful shutdown, and crash recovery.
- [x] Build the registry with stable definition IDs, canonical revisions, audit records, schedule state, run history, idempotency storage, and retention.
- [x] Ensure lifecycle writes are committed before a process is spawned or an API operation reports success.

### Configuration and definitions

- [x] Implement strict TOML bootstrap/includes with positional validation and atomic reload of the complete user-mode desired set.
- [x] Enforce file versus DB authority, optimistic revision checks, and clear authority-conflict errors.
- [x] Support linked local files through CLI `--link`; make CLI `--copy` and all browser uploads DB-authority copies bound to previewed content hashes.
- [x] Use literal config values and explicit environment/file references rather than global interpolation.

### Scheduling and execution

- [x] Put a mature five-field cron evaluator behind an internal schedule interface; support descriptors and persisted `@every` anchors.
- [x] Implement UTC-by-default scheduling, schedule-revision-aware catch-up (`none` and `latest`), DST/clock-step handling, and overlap `skip`/`parallel`. *(DST gap/fold, persisted `@every` anchors, schedule-hash reset, and overlap skip are covered by tests; the wakeup loop recomputes next-fire from wall clock after clock steps.)*
- [x] Implement shell and argv commands, clean environments, effective-user home directories, local user/numeric-ID lookup, and safe privilege dropping.
- [x] Persist and verify PID/PGID/process identity; implement process-group stop, timeout escalation, daemon-restart cleanup, and honest limits around escaped descendants.
- [x] Implement one-instance local workers with restart policies, fixed backoff, health tracking, and an operator hold that prevents unwanted restart.
- [x] Keep active job capacity separate from worker capacity.

### Logs and observability

- [x] Implement a versioned tagged-frame format in daemon ingestion order, including sequence IDs, timestamps, partial lines, invalid UTF-8, truncation, and size caps.
- [x] Implement bounded local zstd chunk storage, indexing, crash recovery, retention deletion, and the in-process backlog-to-live broadcaster.
- [x] Provide authenticated windowed reads, raw download, and resumable streaming via `fetch()`-based SSE; safely render only permitted ANSI styling.
- [x] Archive logs into the separate `minicron-logs.db` SQLite database (ADR-6): jobs on completion, workers on `logs.worker_flush_interval` checkpoints, orphaned buffers salvaged at startup, daily prune at `logs.db_prune_at` bounded by `logs.db_keep_for`.

### Interfaces

- [x] Implement the minimal REST API and Unix-socket peer authentication, token hashing/rotation, request limits, security headers, disabled CORS, and stable error envelopes.
- [x] Implement essential CLI commands: daemon, init, validate, list, run, logs, reload, import, export, status, token, and version.
- [x] Build the minimal React 19 SPA (React Compiler, wouter, daisyUI 5, TanStack Query): login, dashboard, definition list/detail, structured definition form, run detail, and live log viewer.
- [x] Show token fingerprints and rotation controls only; never recover or display the current token.

### v0.1 release gate

- [x] Pass the run-transition, reload, scheduler/DST, process-control, log/SSE, auth, retention, and migration acceptance suites. *(Automated: `go test ./...` — model, config, store, logstore, executor, scheduler, supervisor, api packages.)*
- [x] Verify Linux user mode from fresh install through first visible run in under five minutes. *(Verified end-to-end here.)*
- [x] Test kill/restart recovery, upgrades from the previous schema fixture, and export/import reconstruction. *(kill -9 recovery verified end-to-end; upgrade fixture and TOML round-trip are unit-tested.)*
- [x] Publish static service templates as examples, but do not generate or install them yet. *(examples/systemd; not generated or installed.)*

## v0.2 — advanced local operation

- [ ] Add persistent overlap queues and replace semantics, including cancellation, restart recovery, and capacity behavior.
- [ ] Add job retries with unambiguous retry-root/attempt linkage and no timeout retry by default.
- [ ] Add multiple worker instances and stronger backoff controls; add dependency ordering only with a validated use case and tests.
- [ ] Add typed trigger parameters with validation and shell/argv-safe delivery.
- [ ] Add an internal launcher protocol before enabling concurrency-sensitive child settings such as per-run umask or rlimits.
- [ ] Add optional file watching using the same atomic validation/reload path.
- [ ] Add authenticated Prometheus metrics and explicit degraded-health reporting.
- [ ] Add SQLite backup/restore, optional local-log archive, and restore verification.
- [ ] Add the in-app inbox and versioned fixed-schema webhooks backed by a durable delivery outbox; do not include logs or arbitrary templates.
- [ ] Add service install/uninstall/status for systemd user/system services, with drift and purge safeguards.
- [ ] Add bounded local log search if it meets resource budgets.
- [ ] Complete failure-injection tests for queues, retries, webhook restarts, active backups, and file-watch rename/error cases.

## v0.3 — optional integrations

- [ ] Add one explicitly configured Docker/Podman run driver for new, daemon-owned jobs and foreground workers.
- [ ] Label and reconcile only containers owned by this daemon; support logs, exit mapping, stop escalation, and runtime-unavailable errors.
- [ ] Ship/document an appropriate container image and optional CLI-equipped variant; do not promise Compose reconciliation or import.
- [ ] Add S3-compatible log storage using the same frame/chunk contract and per-daemon prefix.
- [ ] Put a bounded local spool in front of S3 so remote failures never block child output; persist upload progress and expose degraded state.
- [ ] Support indexed historical reads, crash finalization, retention deletion, standard credential chains/secret references, and endpoint hardening.
- [ ] Prove file and S3 backends against identical conformance tests, including prolonged outage, full spool, retry, resume, and cleanup.
- [ ] Add integration health/configuration and retention controls to the UI without expanding the core dashboard.

## v0.4 — security-gated system mode

Do not start this milestone until the local product is stable and an explicit root-daemon security review is complete.

- [ ] Finalize a Linux-first threat model and authorization design for owner-scoped definitions, runs, sources, tokens, and events.
- [ ] Replace self-registration with admin-controlled user enrollment; require a correctly configured group/ACL socket and never fall back to a world-writable socket.
- [ ] Implement owner-permission file/secret access with symlink and path-race defenses so the root daemon cannot be used to read privileged files for a user.
- [ ] Add per-owner quotas for definitions, active jobs, workers, queues, imports, logs/rates, and SSE clients.
- [ ] Add owner-aware database keys, explicit owner API fields/paths, scoped CLI operations, independent owner-set reloads, and admin-only global settings.
- [ ] Keep storage endpoints and notification channels admin-owned to prevent SSRF; definitions may select only predeclared channels.
- [ ] Keep system-mode users CLI-first until a separately reviewed scoped UI is ready.
- [ ] Pass privilege-escalation, namespace-isolation, quota, token, linked-file, and root-daemon crash/recovery tests before release.

## v1.0 — stable contract

- [ ] Freeze v1 config and API compatibility rules; document the supported schema/migration window.
- [ ] Complete security, dependency, fuzzing, race, load, recovery, and platform hardening passes.
- [ ] Resolve all pre-v1 placeholders and remove unsupported/dead configuration keys.
- [ ] Publish versioned quickstart, config reference, API documentation, operational runbooks, security guidance, and recipes.
- [ ] Finalize supported packages/images and reproducible signed release automation.
- [ ] Prove upgrade, backup/restore, and rollback procedures from every supported pre-v1 release.
- [ ] Declare measured operating envelopes and document graceful limits.

## Post-v1 candidates — evidence required

- [ ] Scoped web UI for system-mode users.
- [ ] Additional notification channels or token roles.
- [ ] Compose import/service management.
- [ ] Stronger containment such as Linux cgroups.
- [ ] Multi-host behavior remains out of scope unless the product direction explicitly changes.
