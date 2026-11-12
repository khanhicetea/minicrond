# ADR-21: Cheaper log capture — one flush per pipe read, plain first chunks

- Status: accepted (owner decision, 2026-11-12)
- Date: 2026-11-12
- Amends: [ADR-6](0006-hybrid-log-storage.md) (buffer chunk format),
  [ADR-8](0008-background-first-trade-offs.md) 1B (write-through unit)
- Related: [ADR-17](0017-gc-pacing.md), [ADR-20](0020-log-archive-write-path.md),
  [benchmark](../benchmarks/2026-11-12-adaptive-capture/README.md)

## Context

With zero viewers, capture is the daemon's main background cost. A profile of
the large tier on `5058f20` showed:
- **CPU:** zstd `Flush` at 21% of daemon CPU (one flush per frame) and SQL
  parsing at 12.5% (`database/sql` with modernc re-prepares every statement).
- **Memory:** of a 147 MB live heap, about 87 MB was per-run zstd encoders and
  36 MB pipe buffers.

Most runs are small. In the benchmark databases, 60% of large-tier job runs
(79% for 5A) log under 1 KiB. Yet every run created its own encoder, about
632 KB and 110 µs, and kept it until close. A long-lived worker kept one for
its whole life, even after it went quiet.

Per-frame flushes also compress badly: a 100-line burst written as one pipe
read was stored as 2,084 bytes, because each line ended its own zstd block.

## Decision

Two changes to capture and one to archiving. The read API, sequence numbers,
group-sync durability and the archive commit order stay as they are.

1. **One flush per pipe read.** The pipe reader accumulates the frames of one
   read and writes them through to the chunk file together. It also flushes
   before it would block on the next read (`Buffered() == 0`) and at 16 KiB or
   `logs.sync_max_dirty`, whichever is smaller, so continuous output is still
   written through in bounded steps. EOF and close flush what is left. The
   pipe read buffer shrinks from 64 to 16 KiB, and the line buffer is allocated only when a
   line spans reads. Hot SQL statements (`CreateRun`, `FinishRun`, the
   archive run and chunk inserts) are prepared once and reused; the driver has no statement cache of its own.
2. **Plain first chunks.** A run's first chunk is *plain*: a valid zstd frame
   whose blocks are raw (uncompressed). It needs no encoder. Each flush
   appends one raw block, and sealing appends an empty last block. The
   stock decoder and the crash salvage read plain chunks, torn tails included,
   like any other chunk. `index.json` marks them `"plain": true`.
   - **Promotion:** when a run's output passes 16 KiB (`plainLimit`) in
     the current chunk, the plain chunk is sealed and the run continues in
     compressed chunks with its own encoder, as before. If the chunk is still
     empty (one very long first line), it switches in place.
   - **Release:** a worker checkpoint (`logs.worker_flush_interval`) resets
     promotion. The next chunk starts plain and the encoder is released, so a
     worker that went quiet stops holding one. A still-chatty worker is
     promoted again after 16 KiB.
3. **Compression at archive time.** The archiver compresses each plain chunk
   once, with one shared encoder (fastest level, 128 KiB window, concurrency
   = the 2 archive workers). It stores the result only if it is smaller;
   otherwise it stores the plain chunk as is. Plain chunks are always blobs,
   never ADR-20 file chunks.
   - **Handoff:** when a run closes with exactly one plain chunk, fully
     written and without errors, its in-memory image goes to the archiver
     with the queue entry. The archiver then does not read the buffer back.
     An image is kept only for a run actually in the queue. All images
     together are capped at 8 MiB; past that, and after an overflow or a
     stop, the archiver reads the file as before. Before committing from an
     image, the archiver checks that the run's buffer still exists, so a run
     deleted in the meantime is not resurrected.

## Accepted downside

- **Larger live buffers:** a plain chunk holds up to 16 KiB of uncompressed
  output on disk until it is archived. The benchmark shows 11–21% more
  live-buffer growth than with change 1 alone (still about half of base),
  offset by a smaller archive. The disk budget counts these bytes as before.
- **One extra chunk** for every run that is promoted, and again after each
  worker checkpoint that is followed by more than 16 KiB.
- **Wider sequence gaps on storage failure:** when a batched write fails, all
  frames of that pipe read (at most 16 KiB) consume sequence numbers without
  being stored, instead of one frame. Readers already handle such gaps
  (`gap` events, see `docs/http-api.md`).
- **Archive CPU** for plain chunks: about 4 µs for a short run and 14 µs for a
  100-line burst, on one of the two archive workers, off the completion path.
- **No backward compatibility work:** there is no production data yet. Older
  buffers are compressed chunks and still read normally.

## Bounds, lifecycle, ownership

- No new goroutines, timers or queues. Pipe batches are bounded by one read
  (16 KiB) or `logs.sync_max_dirty`. A plain chunk image is bounded by
  `plainLimit` plus one pipe read.
- The run's writer owns its image, under the writer lock, until close. At
  close the archiver queue takes it over (≤ 8 MiB in all). Taking a run
  from the queue removes its image. Stopping the archiver clears the rest.
- The shared archive encoder is created on first use and lives as long as the
  store. Per-run encoders exist only while a run is promoted.

## Durability

Unchanged. Accepted output is still written to the chunk file before the next
read from the child (daemon-crash-safe, ADR-8 1B), and fsyncs follow the group
sync (ADR-20). The image handed to the archiver is a copy of bytes already in
the file; it saves a read, not a write. Commit-before-unlink is unchanged.
After a daemon crash the orphan sweep salvages plain chunks from their files.

## Measurements

[Benchmark page](../benchmarks/2026-11-12-adaptive-capture/README.md):
interleaved base / change 1 / changes 1–3, ext4, zero viewers, 120 s windows,
2–3 repetitions per tier. Changes 1–3 against base:

| tier | daemon CPU | median RSS | write syscalls | disk growth |
|---|---|---|---|---|
| large | −17.6% | −61.8% | −46.5% | −40.2% |
| large, quiet | −18.3% | −55.0% | −56.5% | −30.1% |
| normal | −19.3% | −42.0% | −58.6% | −8.6% |
| 5A small | −16.2% | −27.8% | −53.5% | −11.3% |

- **Per run** (1-line job, capture only): 176 → 106 µs and 879 → 37 KB
  allocated.
- **Archive size:** smaller than per-read compression, since one pass covers
  the whole run (100-line burst: 547 vs 552 B; 4 single-line writes: 186 vs
  231 B).
- **Unchanged within noise:** bytes written, block flushes, runs completed,
  exit → ended latency and restart latency.
- **Slightly worse:** read syscalls rise 6–11% (the 16 KiB pipe read buffer).

Change 2's release step barely shows in 120 s windows, since checkpoints are
every 15 minutes. It is covered by `TestCheckpointReleasesEncoder`.

Tests: `adaptive_test.go` (plain run archived compressed from its image,
promotion at the limit and in place, checkpoint release, deleted run not
resurrected, plain/mixed/torn chunk decoding, malformed plain chunks),
`pipebatch_test.go`, `stmtcache_test.go`, and the archive queue overflow test
(no image kept for an overflowed run).

## Revisit when

- Many runs land just above 16 KiB, so promotion churn (an extra chunk per
  run) shows up in archive rows or CPU. A per-job choice of the starting
  mode from run history (the deferred "Phase 3") would address this.
- Small disks hit the disk budget because of uncompressed live buffers.
- `logs.worker_flush_interval` becomes much longer, so quiet workers hold
  encoders for longer.
- A reader or tool outside minicrond needs compressed buffer files.
