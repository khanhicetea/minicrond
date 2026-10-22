# minicrond (next release): log disk budget

The log disk budget (ADR-8 3A, [ADR-10](../adr/0010-log-disk-budget.md)) is new and
its headroom rule is **on by default**.

- `logs.disk_min_free` defaults to **512 MiB** (clamped to a quarter of the
  filesystem). When free space on the data directory's filesystem falls below it,
  the oldest completed runs' logs are deleted early, ahead of `keep_for` /
  `logs.db_keep_for`, until free space reaches 125% of it. Deleted logs are not
  recoverable; run records and other metadata are never touched.
- To keep the previous behavior set `logs.disk_min_free = 0`. To bound log disk use
  directly set `logs.disk_budget` (MiB, default off).
- New opt-in quarantine purge: `logs.quarantine_keep_for`, `logs.quarantine_max_size`.
- `GET /api/v1/daemon` gains `diagnostics.log_storage` and
  `diagnostics.terminal_persistence`.
- The daemon logs a `log disk budget:` INFO line at start and reload with the
  effective settings.
- Admission of new work is not affected by disk pressure; when reclamation is
  insufficient, log output is dropped under the capture-failure policy.

## Execution first and containers ([ADR-16](../adr/0016-execution-first.md))

- Scheduled jobs and supervised workers keep running when the metadata
  database or log storage fails (full disk, read-only volume, locked database,
  stalled device). Previously a failed write stopped all scheduled jobs, killed
  just-spawned processes, kept crashed workers from restarting and could make
  startup exit. History is now written behind execution by a bounded background
  writer (1024 runs; beyond that history is dropped and counted).
- Completion alerts and retries no longer wait for the terminal write; no
  capacity slot is held for history any more (`diagnostics.finalizers.inline`
  is always 0). New `diagnostics.run_history`.
- A run whose log storage cannot be opened starts with output discarded instead
  of failing with `start_error`. A log archive database that cannot be opened at
  boot leaves the daemon on file buffers instead of exiting.
- A panicking maintenance loop is restarted instead of stopping the daemon.
- As PID 1, minicrond runs a built-in init (signal forwarding, orphan reaping).
  New `minicrond healthcheck`, a `Dockerfile` and `examples/docker/`. SIGQUIT
  shuts down cleanly. The binary embeds time zone data. An empty data directory
  created 0755 by a container runtime is tightened to 0700 instead of refusing
  to start; without HOME the default data directory is `/var/lib/minicron`.
