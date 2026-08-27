# 09 — Log Pipeline: File backend & S3 backend

Status: Draft · Revised by ADR-1 (SlateDB → direct S3 chunk objects) ·
Config keys: spec `04`

## Requirements

- Every run captures full stdout/stderr, distinguishable per stream,
  interleaved in one ordered sequence with absolute line numbers stable for
  the run's life.
- Live tail with resume (SSE) and windowed random access (jump to line N,
  last N lines) for UI/API — regardless of backend.
- Backend choice is per-daemon (one active sink), set in `[logs] backend`;
  runs record where their bytes went (`runs.log_ref`), so switching backends
  never orphans history.
- D-4 (as amended by ADR-1): S3-backed log storage is a first-class
  backend, not an afterthought — but the file backend remains the
  zero-config default.

## Wire format (backend-independent)

A run's log is a sequence of **frames**:

```
frame := stream_tag line
stream_tag := 0x01 (stdout) | 0x02 (stderr) | 0x03 (system)
```

- Frames are batched into **chunks** (default 256 KiB, zstd-compressed)
  — the unit of storage, transfer, and retention in both backends.
- A per-run **index** maps line number → chunk offset, rebuilt cheaply from
  chunk footers (each chunk footer records first/last line number and byte
  length).
- ANSI escape sequences pass through untouched (UI renders); `system` lines
  are minicron's own annotations (e.g. "timeout reached, stopping",
  "truncated 3 lines over 256KiB").
- `log_max` budgets bytes per run (default 100 MiB). On overflow:
  `log_on_full = drop_old` (default; ring-buffer style: oldest chunks
  deleted, line numbering continues upward, gap recorded via system line) |
  `drop_new` (capture stops; run continues) | `kill` (run stopped,
  `log_overflow` end reason).

## The LogSink trait

```
trait LogSink {
  fn open(run) -> StreamWriter;          // buffers + flushes chunks
  fn read(run, from_line, direction, limit) -> FrameStream;
  fn tail(run, after_line) -> FrameStream;   // live follow
  fn delete(run);                        // retention
  fn size(run) -> bytes;
}
```

The executor pumps pipes into `StreamWriter`; the HTTP layer reads through
`read`/`tail`. Backends differ only underneath this line.

## FileSink (default)

- Layout: `logs/<job>/<YYYY>/<MM>/<DD>/<run_id>.log` — gzipped chunks
  sidecar `<run_id>.idx` (chunk index) + `<run_id>.meta` (footer: status,
  truncation flags). Plain text is the union of decompressed chunks in order;
  a `zcat` one-liner works (documented).
- Writes: async pump task per run, chunk-buffered, fsync on chunk flush
  (cheap, few per run); crash mid-run leaves complete chunks only — the
  recovery pass marks the run `interrupted` and appends a closing system
  line.
- Reads: index seek → decompress one chunk → scan. Tail = poll at 250 ms +
  inotify/fsevents where available (polling fallback is fine locally).
- `minicron_LOG_PATH` env var is injected into runs (file backend only) so
  scripts can self-reference.

## S3Sink [v0.3] (D-4 as amended)

Plain object storage — S3-compatible endpoints (AWS, MinIO, Cloudflare R2,
Garage, …). **When to choose**: multi-gigabyte verbose workers, off-box log
retention, boxes you snapshot without dragging logs along, or central
aggregation across many daemons (each daemon owns a distinct prefix —
single-writer per prefix, no coordination needed).

### Object layout

```
<prefix>/runs/<run_id>/<seq:06d>.zst     # zstd chunks, frame format above
<prefix>/runs/<run_id>/index.json        # line→chunk map, byte counts,
                                         # truncation flags, final flag
```

- Chunks are byte-identical to FileSink chunks (same frame format, same
  footers carrying first/last line numbers) — `index.json` is a convenience
  cache, never a correctness requirement: any reader can `ListObjectsV2`
  the prefix and decode footers.
- Recommended `<prefix>` includes the daemon `instance_id` for
  multi-machine aggregation.

### Write path

- Async pump per run buffers a chunk (default 256 KiB) → `PutObject`.
- `index.json` rewritten on rotation/truncation events and finalized at run
  end (a `final: true` flag).
- S3 errors: 3 retries, exponential backoff + jitter; persistent failure
  applies the `log_on_full` policy (default `drop_new`: stop shipping, never
  stall the worker's stdout pipe — the run itself keeps going). Escalates to
  `logs.unwritable` events.
- S3 has been strongly consistent since 2020 — list-after-put reads are
  safe; no read-your-writes caveats.

### Read path

- Windowed read: `GET index.json` → ranged `GET`s for exactly the chunks
  covering the requested lines (no full-download ever).
- Live tail of **active** runs serves from the daemon's in-memory buffer —
  S3 is never on the hot path; only historical reads hit the bucket.
- Historical read of a crashed run (no final index): list + footer-scan
  fallback; the recovery pass appends the closing `system` line and writes
  the final index on boot.

### Retention

- The existing sweeper issues batched `DeleteObjects` (≤ 1000 keys per call,
  including `index.json`) per expired run — the same code path as a file
  unlink. Retention stays per-job because the registry drives it.
- Ops belt-and-suspenders: a bucket lifecycle rule on the whole prefix is
  documented as a coarse fallback; our sweeper remains the source of truth.

### Config (`[logs.s3]`, spec `04`)

`endpoint` (empty = AWS default), `region`, `bucket`, `prefix`,
`credentials = env|file|static`, `force_path_style` (MinIO/R2). Credentials
never logged; connection failures → `logs.unwritable` events.

### Cost shape

~1 PUT per 256 KiB of compressed log + 1 index rewrite per run; reads = 1
index GET + 1 GET per chunk. Request pricing dominates for very chatty
fleets — the chunk-size knob (parking lot: 256 KiB vs 1 MiB) is the tuning
lever.

## Why not an embedded LSM (SlateDB) — considered, rejected (ADR-1)

The original constraint named SlateDB (an embedded async-Rust LSM over
object storage). Our workload is append-only chunks, sequential range
reads, and whole-run deletes — no mutations, no point lookups. An LSM's
machinery (memtables, WAL, compaction, bloom filters) serves workloads we
don't have, and compaction would rewrite write-once data for no benefit,
while costing a young pre-1.0 dependency, resident memory, and
version-pinning risk. The one capability we'd actually use — durable S3
I/O — is exactly what this design does with a plain S3 client. Revisit only
if a future need emerges for indexed point lookups into log bytes (none is
on any roadmap).

## Streaming API (both backends)

- `GET /api/v1/runs/{id}/log?from_line=&limit=` — windowed frames.
- `GET /api/v1/runs/{id}/log?tail=100` — last N lines.
- `GET /api/v1/runs/{id}/log/raw` — text download (stdout+stderr inline,
  stderr visually tagged `[err] ` prefix in raw form only).
- `GET /api/v1/runs/{id}/log/stream` — SSE: `line` events (batched frames),
  `backlog_done`, `done`, `dropped`; `Last-Event-ID` = line number for
  resume. One shared SSE bus per client connection (see `11`).
- Search [v0.2]: `GET /api/v1/jobs/{name}/log/search?q=&regex=` — bounded
  parallel scan over recent runs, cursor-paginated.

## Open questions

- Chunk size 256 KiB vs 1 MiB (S3 request economics vs tail latency) —
  parking lot; measure with a chatty worker.
- `log_on_full` per-definition vs global — draft: per-definition with
  `[defaults]` fallback.
- `index.json` rewrite cadence under heavy `drop_old` rotation churn
  (each rotation bumps the object) — verify PUT costs are acceptable or
  batch rotations.
