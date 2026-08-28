# ADR-6: Hybrid log storage — file buffer plus separate SQLite archive

- Date: 2026-08-28
- Status: accepted

## Decision

Run logs use a two-tier design. Live runs keep writing compressed zstd chunk
files under `data/logs/<run_id>/` first (unchanged hot path, crash-safe). A
separate SQLite database, `data/minicron-logs.db` (never `minicron.db`), is the
long-term archive:

1. **Cron job logs** stay in the file buffer while the run executes; when the
   run finishes the executor's store close archives every chunk into the log
   database and removes the buffer directory. Output written before a crash is
   therefore already durable in files.
2. **Worker logs** (long-running, too large to buffer or hold in memory) are
   checkpointed: every `logs.worker_flush_interval` (default 15m) the daemon
   seals the current chunk and copies all sealed chunks into the log database,
   keeping only the active chunk on disk. Reads merge archive, buffer files,
   and the in-memory tail, so the flush boundary is invisible.
3. **Crash recovery**: buffer directories left without a writer at startup are
   swept into the archive; indexed chunks are archived verbatim and a torn
   in-progress chunk is salvaged up to the last intact frame.
4. **Pruning**: a daily sweep at `logs.db_prune_at` (default 03:30, scheduler
   timezone) deletes archived logs older than `logs.db_keep_for` (default
   720h). Per-run retention (`keep_runs`/`keep_for`) removes a run's logs from
   both tiers.

Chunk rows are upserted keyed by `(run_id, number)`, so a crash between the
database write and the file cleanup replays safely. `logs.backend = "file"`
remains the only backend value: the file buffer is still the only write path;
the archive is an additional durable tier, not a replacement.

## Consequences

- The data directory holds two databases; copying `minicron.db` alone no longer
  preserves run output. Rollback guidance (stop daemon, copy whole data dir)
  still applies.
- `log_max` continues to bound the writer's ring-buffer accounting per run;
  archive growth is bounded by `logs.db_keep_for` and per-run retention.
- Remote/S3 log backends remain later work and can reuse the same chunk
  framing and flush hooks.
