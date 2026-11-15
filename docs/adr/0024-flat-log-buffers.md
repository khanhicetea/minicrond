# ADR-24: Flat log buffer files

- Status: accepted (owner decision 2026-11-15, after the measurements below,
  for the fsyncs it saves while the archiver is behind)
- Date: 2026-11-15
- Amends: [ADR-6](0006-hybrid-log-storage.md) (buffer layout),
  [ADR-20](0020-log-archive-write-path.md) (what the group sync fsyncs)
- Related: [ADR-8](0008-background-first-trade-offs.md) 1B,
  [ADR-10](0010-log-disk-budget.md), [ADR-22](0022-archive-on-group-sync.md)

## Context

Every run's live log buffer was a directory, `logs/<run_id>/`, holding
`index.json` and `NNNNNN.zst` chunks. Zero-viewer cost per run, on the
background path:

- a `mkdir` at open and a `rmdir` after archival, each allocating or freeing
  an inode and a directory block and journaling the change (ext4);
- a directory fsync per sealed run at the group sync, unless the archiver
  removed the buffer first (ADR-22);
- outside the short-run fast path (`removeBuffer` from an image), buffer
  removal went through `os.RemoveAll`: 14 syscalls for a two-file buffer, of
  which two are the useful unlinks (strace, 2026-11-15), and every orphan
  archive and read listed the run directory (`openat`, 2 `getdents64`,
  `close`).

The owner asked for this to be optimized for disk cost rather than CPU.

## Decision

A run's buffer is flat files directly under `logs/`:

```
logs/<run>.json          index
logs/<run>.json.tmp      index being replaced (crash leftover only)
logs/<run>.NNNNNN.zst    chunks
```

1. **No directory per run.** The first chunk is created with `O_EXCL`,
   which also refuses a run id whose buffer is still on disk. Run ids must
   not contain `.`, `/` or `\` (they are UUIDv7 in the daemon); `Open`
   refuses others.
2. **One directory fsync per group sync pass.** Every buffer file is in
   `logs/`, so the pass fsyncs that directory once for all sealed runs
   instead of once per run. Strict `"frame"` durability still fsyncs the
   directory on every index install.
3. **A run's files are found from its index, not a listing.** Chunk
   numbers are consecutive per run, so `bufferChunks` takes the index's
   chunks plus a new `open` field (the chunk being written when the index
   was written; 0 once closed), stats them, and probes one number below and
   above until a number is missing; without an index it probes from chunk 1.
   This covers a chunk newer than the index (crash before the next seal), a
   worker whose sealed chunks were all archived, and an abandoned chunk
   next to the indexed ones. Readers, the archiver and deletion use it.
4. **Sweeps list the root once** (orphan sweep, disk usage, pressure
   pruning, `ArchiveStats`), as they listed it before, group the files by
   run and pass the listed chunks along, so stray files are never missed.
5. **Partly archived index-less orphans get a stub index.** When the
   orphan archiver commits and removes the first batch of a buffer that has
   no usable index and then stops (database failure, cancellation), it
   writes an unsynced index that lists no sealed chunk and names the lowest
   chunk left, so readers keep finding the rest before the next sweep. It
   lists no sealed chunk, so the next sweep still salvages them.
6. **No migration.** No installation runs in production yet, so the
   per-run directory layout is not read: a `logs/<run>/` directory is
   ignored by every path.
7. **Quarantine.** A buffer quarantined as a whole moves to
   `.quarantine/<run>/` with its files keeping their names, so it stays one
   entry for the quarantine policy; a corrupt chunk's copy is
   `.quarantine/<run>.NNNNNN.zst.corrupt`. The archive directory
   `log-archive/<run>/` (ADR-20) is unchanged: it holds only large chunks
   and is not on the short-run path.

Rejected: `logs/<job>/<run>.*` (the layout first suggested). It saves the
same syscalls, but job names would have to be sanitized into paths, renaming
or deleting a definition would orphan or move directories, the store is
keyed by run id throughout (run locks, reads, orphan sweep, quarantine,
budget, archive) and a frequently firing job would concentrate every fsync
and directory lock on one directory.

## Guarantees and bounds

Unchanged: every accepted frame is written through before the next read;
batch fsync grouping and its power-loss window (ADR-8 1B, ADR-20); commit
before unlink and commit before rename; daemon-crash recovery salvages torn
chunks; reads see written-through output without waiting for archival.

Removal order is chunks, then the temporary index, then the index, so a
removal cut short leaves an index for the files left.

A stray file `bufferChunks` cannot reach (only an empty abandoned chunk
separated from the indexed ones by an archived one) is removed by the next
orphan sweep, which lists the root. Readers lose nothing: such a chunk is
empty.

## Downsides

- No measured disk gain in the normal case (below): the change is kept for
  the directory fsyncs it saves when sealed buffers wait for a slow archiver
  (one per group sync pass instead of one per run; for 20 jobs a minute up
  to about 27,000 a day while the archiver is behind, estimated, not
  measured).
- One `logs/` directory holds every buffer, a few files per live or
  unarchived run. ext4 and xfs index large directories; the sweeps already
  listed it in full.
- A read of a run with no buffer costs two failed lookups (index and
  chunk 1) instead of one failed `openat`.

## Measurements

[Benchmark 2026-11-15](../benchmarks/2026-11-15-flat-log-buffers/README.md),
real disk, 3 tiers, 2 interleaved runs per build:

- Bytes written, block write IOs, block flushes and disk growth: no change
  outside noise on any tier (bytes written 0.0%, +1.1%, +0.1%).
- Syscalls per run (strace, large tier): `mkdirat` 1 → 0, `unlinkat`
  3.8 → 2.0, fsyncs unchanged (2.1 → 2.0); about 3 of ~250 syscalls per run.
- Daemon CPU +0.9%, −3.0%, −3.7%, within the run-to-run range.

The short-run fast path already removed buffers with three unlinks, so the
saving there is one `mkdir`, one `rmdir` and the directory inode/block, which
the disk does not register at ~28 runs/s.

Tests: `go test ./...`, `go test -race ./...`, `go vet ./...`, new
`layout_test.go` (naming, refused run ids, flat files synced through the root,
index-driven chunk discovery, removal) and the existing
salvage, quarantine, budget, group-sync and archive tests on the flat layout.

## Revisit when

- Run rates grow far beyond the large tier, where per-run directory metadata
  may start to register on disk.
- Buffers commonly number in the tens of thousands (archive backlog under
  degraded storage) and the root listing of the sweeps becomes the cost.
