# minicrond (next release): log disk budget, execution first

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
- New `scheduler.shutdown_grace` (seconds, default 0 = off): caps each run's
  grace when the daemon shuts down, workers included. The image's example
  config sets 5 so `docker stop` (10 s) finishes cleanly (measured 5.2 s,
  exit 0; previously 10.2 s and SIGKILL).
- Stops no longer wait the full `grace` for zombie members of a process group
  (outside the built-in init or tini).
- A data directory that cannot be written (read-only volume, full disk at
  boot) still starts when its metadata database is readable and its schema is
  current: the database is opened read-only, jobs and workers run, nothing is
  stored, the Unix socket may be unavailable and `minicrond healthcheck` then
  fails. A missing, unreadable or outdated database is still fatal.
- A database write lock held by another process no longer slows scheduled
  jobs: schedule-state writes are skipped while storage is degraded.
- Alert delivery records of runs whose history was deferred are written after
  the run row instead of failing (and rolling back other records).

## Workers

- New worker field `restart_backoff_max` (seconds, default 0 = off): the
  worker is never retired; each consecutive unhealthy exit doubles
  `restart_delay` up to the cap. Without it `max_restart_attempts` still
  retires a crash-looping worker.
- `POST /api/v1/workers/{name}/start|stop|restart` (and `minicrond start|stop|
  restart`) now accept config-owned workers; edits stay refused.

## Log archive writes ([ADR-20](../adr/0020-log-archive-write-path.md))

- Log chunks of 64 KiB or more (compressed) are no longer copied into
  `minicron-logs.db`: the database keeps a row that points to the file, which
  moves to the new `data/log-archive/<run_id>/` directory. Small chunks are
  still stored in the database. Log archive schema 4; the migration only adds
  a column. Back up the whole data directory, `log-archive/` included.
- Under load the archiver stores up to 32 finished short runs in one
  transaction.
- In `logs.durability = "batch"`, end of output and run completion no longer
  fsync on their own; the next group sync (`logs.sync_interval`, 2 s) does, and
  skips runs already archived by then. An OS crash or power loss can now lose
  the last ~2 s of a finished run's output, as it already could for a running
  one. `"frame"` and orderly shutdown still sync at once.
- `logs.db_max_size`, the disk budget and the archive diagnostics count the
  `log-archive/` files.
- Measured per run on ext4: a short run writes about 10 KiB instead of 46 KiB
  with 0.08 instead of 1.4 disk flushes; a 4 MiB run writes 1.1 MiB instead of
  2.8 MiB.

## Memory

- Each open run's log encoder uses a 128 KiB zstd window instead of 1 MiB (8
  MiB with a large `max_line`): about 0.6 MiB instead of 1.46 MiB of heap per
  running job or worker, for 1–2% lower compression. Large lines still work.

## Not done yet

Status 2026-10-24, ordered by impact on "keep jobs and workers running".

1. **Degraded-storage log noise.** While storage fails, each run logs its
   log-open failure and each history retry; bounded by backoff, not rate
   limited. In read-only mode the history writer retries writes that cannot
   succeed until its 1024-run budget fills.
2. **History written behind execution is lost on restart** (ADR-16 accepted
   cost; a journal was rejected). After a crash during an outage,
   `catch_up = "latest"` may re-run an occurrence whose row was never stored.
3. **First writes of a locked-database episode** each wait SQLite's 5 s busy
   handler before the circuit breaker opens (one delayed fire per job, one
   delayed restart per worker).
4. Pending retries are in memory and lost on restart (documented design).
5. Not measured: power-loss behavior of batched fsync, `EIO` in the middle of
   log archival, numerical log-volume targets.
