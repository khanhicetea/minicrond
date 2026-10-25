# ADR-20: Cheaper log archive writes — file chunks, batched commits, deferred seal syncs

- Status: accepted (owner decision, 2026-10-25)
- Date: 2026-10-25
- Amends: [ADR-6](0006-hybrid-log-storage.md) (archive tier),
  [ADR-8](0008-background-first-trade-offs.md) 1B (final sync on EOF and seal)
- Related: [ADR-10](0010-log-disk-budget.md), [ADR-17](0017-gc-pacing.md),
  [ADR-18](0018-relaxed-commits.md),
  [benchmark](../benchmarks/2026-10-25-log-archive-io/README.md)

## Context

Every run's log is written twice: to its chunk file in `logs/<run_id>/`, and
again into `minicron-logs.db` when the run is archived. Zero viewers read
either copy, so this is pure background cost. Measured per run on ext4
(`io_cost_bench_test.go`, archiver running as in the daemon):

| per run | short (3 lines) | silent (exit line only) | large (~4 MiB raw) |
|---|---|---|---|
| bytes written | 46 KiB | 41 KiB | 2.8 MiB |
| of which the archive copy | 34 KiB | 33 KiB | 1.75 MiB |
| block-device flushes | 1.44 | 1.34 | 16–17 |

A short run stores about 200 bytes of log, but the archive transaction
touches several 4 KiB pages: the run row, the chunk row, three indexes, all
written to the WAL and then checkpointed. A large run's compressed chunks are
written to the file, then to the WAL, then checkpointed into the database: three
writes of the same bytes. Each seal also fsyncs the chunk, `index.json` and the
directory, usually a few milliseconds before the archiver commits the run
(which is durable on its own) and deletes the files.

Replacing SQLite does not remove the copy. bbolt rewrites page paths on every
commit, and LSM engines (Badger, Pebble) add compaction or value-log
rewriting plus tens of MiB of memtables. Writing every byte once needs a custom
segment-file store with its own index, crash recovery and compaction for
per-run retention. That is a new storage engine, which AGENTS.md rules out
without stronger evidence. Keeping all logs as plain files instead would cost
at least a 4 KiB block per tiny run (about 12 KiB per run with its index and
directory). SQLite packs small runs much more tightly.

## Decision

Three changes on the archive write path. The file-first capture path, read
merging and every public contract stay as they are.

1. **Large chunks are archived as files.** A sealed chunk of at least 64 KiB
   compressed (`fileChunkMin`) gets a `log_chunks` row with `file_bytes > 0`
   and an empty blob. Its buffer file is renamed into
   `data/log-archive/<run_id>/<number>.zst` instead of being copied. Smaller
   chunks stay blobs. Log archive schema 4 adds the column and a partial
   index on file chunks.
   - **Order:** commit, then rename (like commit-before-unlink). Until the
     rename, readers fall back to the buffer path. After a crash the orphan
     sweep upserts the same row and renames again. If a rename fails (for
     example a data directory split across filesystems), the chunk is
     stored as a blob instead.
   - **Deletion:** every path that deletes rows (age prune, `db_max_size`,
     disk-pressure prune, run deletion) unlinks the files after its commit.
     A crash in between leaves unreferenced files, which a startup sweep
     (`SweepArchiveFiles`) removes.
   - **Accounting:** `logs.db_max_size`, the disk budget and the archive
     diagnostics count the files as archive bytes.
2. **Batched archive commits.** The archiver takes up to 32 queued runs at a
   time. Runs whose buffers are complete and small (a valid index listing
   exactly the chunk files on disk, all of them blobs) are committed in one
   transaction, within the existing 8 MiB / 64-chunk batch bounds. The others
   take the one-by-one path. A lone queued run is archived alone, at once, so
   nothing waits to fill a batch.
3. **Seal and pipe EOF join the group sync.** In batch durability
   (`logs.durability = "batch"`), sealing a chunk no longer fsyncs it, its
   `index.json` or the directory inline, and pipe EOF no longer syncs. The
   seal is queued for the shared group-sync timer (`logs.sync_interval`,
   default 2 s), which syncs the files and directories that still exist.
   - **Already archived:** a buffer the archiver committed and removed in the
     meantime is never fsynced.
   - **File chunks:** a file chunk still waiting for its sync is synced at its
     archive path. One that was already synced is not synced again. One left
     by an earlier process is synced before its row is committed.
   - **Bounds:** the queue holds at most 4096 seals; beyond that a seal syncs
     inline as before. One group-sync pass runs at a time, with at most 8
     concurrent fsyncs so the filesystem can share journal commits.
   - **Unchanged:** `"frame"` durability and orderly shutdown still sync
     everything before returning.

## Accepted downside

- **OS crash or power loss:** the last ~2 s of a *finished* run's output can
  now be lost, along with its `index.json`, even though the run's terminal
  state is durable. ADR-8 1B already accepts the same window for running runs.
  A stalled disk stretches it. An index that comes back empty is treated like
  a missing one, and its chunks are salvaged up to the last intact frame. The
  run's job/kind label and drop counters in the archive are lost in that case.
  A daemon crash still loses nothing.
- **Backups:** the archive is now two places, `minicron-logs.db` and
  `log-archive/`. Backups must copy the whole data directory, which was
  already the guidance.
- **`logs.db_max_size`** now bounds the database plus its archive files, not
  the database file alone.

## Bounds, lifecycle, ownership

There are no new goroutines while idle.
- The group-sync pass and its ≤ 8 fsync workers exist only during a pass.
  The timer is armed only while something is dirty or queued.
- Batching uses the existing two archive workers. A batch holds at most 32
  run locks, taken after the archive slot (same order as before), and at most
  8 MiB of blobs.
- The deferred-seal tracking holds one path per sealed chunk until it is
  synced (≤ 4096 queued).
- Archive files are owned by their rows. Writes and renames happen under the
  run's claim and exclusive lock. Readers hold the run's shared lock, and
  unlink happens after the row is gone.

## Measurements

Per run, `io_cost_bench_test.go`, archiver running, same host and filesystem,
2 repetitions each:

| per run | short (3 lines) | silent | large (~4 MiB raw) |
|---|---|---|---|
| bytes written, before → after | 46 → 10 KiB | 41 → 10 KiB | 2.8 → 1.08 MiB |
| block flushes, before → after | 1.44 → 0.08 | 1.34 → 0.08 | 16–17 → 2.3–2.7 |

With the archive unavailable (buffers synced, archived later), buffer flushes
per short run fall from 0.81 to 0.51. The deferred syncs run concurrently, so
they share journal commits better than the inline seals did.

The scenario seals 300 runs ten at a time, so batches are larger than a quiet
daemon sees.

Daemon tiers ([benchmark page](../benchmarks/2026-10-25-log-archive-io/README.md),
2 interleaved runs per cell, 120 s windows):
- System-wide block flushes fall 42% (5A small), 36% (normal) and 34% (large),
  and write IOs by about as much.
- Child exit → run ended falls from 16–32 ms to under 1 ms.
- CPU falls 10–15% on normal and large.
- Large-tier peak RSS is ~35 MiB higher (median unchanged); not investigated.

## Revisit when

- Power-loss tests show the deferred seal window is longer than the group-sync
  interval in practice.
- Archive files grow numerous enough (many large runs kept for a long time)
  that the per-directory walks for disk accounting or the startup file sweep
  show up in measurements.
- A filesystem that does not flush on clean-file fsync, or that reorders
  renames, becomes a supported target.
