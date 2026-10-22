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

## Not done yet / next steps

Status at commit time (2026-10-22). Ordered by impact on "keep jobs and workers
running".

1. **Independent re-test of storage faults against the new code.** The first
   fault-injection pass (full disk, locked DB, dead filesystem, unwritable logs,
   faults at boot) ran on the old code and found every issue ADR-16 fixes; the
   re-run on the new code was stopped before it reported. Unit and integration
   tests cover each case, and a full-disk/locked/missing-table run passes, but
   repeat the real-filesystem scenarios (ext4 `-m 0` images, `EXT4_IOC_SHUTDOWN`).
2. **Still fatal at boot:** a read-only data directory (the lock file cannot be
   created) and a metadata database that cannot be opened or migrated. Both stop
   everything; consider a retry loop or an in-memory mode for config-owned
   definitions.
3. **Shutdown vs. `docker stop`.** The run-stop stage allows 15 s and each run's
   `grace` defaults to 10 s, so a worker that ignores SIGTERM can outlast
   Docker's default 10 s stop timeout. Docs recommend `stop_grace_period: 30s`;
   a `shutdown.timeout` setting that caps grace at shutdown would remove the
   need.
4. **History written behind execution is lost on restart.** Deferred history
   lives in memory (1024 runs); a daemon restart during an outage loses it, and
   `catch_up = "latest"` may re-run an occurrence whose row was never stored.
   Alert delivery records can fail (foreign key) while the run row is deferred.
5. **Worker crash loops.** `max_restart_attempts` (default 5) still retires a
   worker for good; config-owned workers can only be revived with `reload`
   (the API refuses `start` for them). For containers consider exponential
   backoff instead of giving up, and allowing lifecycle actions (not edits) on
   config-owned workers.
6. **5A benchmark** (`docs/benchmarks/2026-10-22-5a-baseline/`): baseline only,
   no profile and no A/B against ADR-16. Profile the ~1.7 MiB of heap per open
   run (likely per-writer zstd buffers).
7. **Container image** was verified with a scratch image in real Docker, but
   the shipped `Dockerfile` itself was not built (Docker Hub rate limit).
8. `groupAlive` treats zombie group members as alive, so a stop waits the full
   `grace` when no init reaps orphans (only outside the built-in init/tini).
9. Pre-existing: pending retries are in memory and lost on restart; the
   embedded web bundle was not rebuilt, so the UI does not show
   `run_history` yet.
