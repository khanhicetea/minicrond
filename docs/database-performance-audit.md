# Database performance and reliability audit

Measured on 2026-09-30 against commit `8c578ee`, followed by the changes in
this working tree. The current SQLite and hybrid log design is a sound base
for a small daemon. This audit supports focused changes to database access:
reuse statements within archive batches, preserve connection settings after
replacement, and make archive deletion atomic. The measurements do not
justify replacing the database engine or rewriting the whole service.

## Current storage strategy

| Area | Current behavior | Assessment |
|---|---|---|
| Metadata | Separate SQLite database for definitions, runs, schedules, alerts and audit | Clear ownership and explicit SQL |
| Logs | Compressed file buffer plus a separate SQLite archive | Keeps archive work separate from metadata writes |
| Connections | One warm connection per database | Bounds connections and their caches; reads and writes queue within each database |
| Durability | WAL, `synchronous=FULL`, file and directory syncs | Preserve these commit boundaries |
| Archive transfer | At most 64 chunks and 8 MiB per batch, allowing one oversized chunk | Bounds ordinary transfer memory and transaction size |
| Archive reads | Fetch one blob, release the database connection, then decode | Prevents decoding from monopolizing the sole connection |
| Run retention | 128-run pages, terminal-state and idempotency guards | Already avoids a commit per deleted run |
| Metrics | Indexed window selection and exact percentiles in Go | Old history is excluded efficiently; large selected windows still cost CPU and memory |

SQLite documents durable WAL commits with `synchronous=FULL`; reducing it
to `NORMAL` changes power-loss durability. That tradeoff is unsuitable for
the requested reliability target. See the
[SQLite synchronous documentation](https://www.sqlite.org/pragma.html#pragma_synchronous).

## Findings and changes

**Fixed: connection replacement lost foreign-key enforcement and the busy
timeout.** Both stores configured connection-local settings during initial
migration only. Forcing retirement of that connection reproduced
`foreign_keys=0` and `busy_timeout=0`. The shared
[`internal/sqlite`](../internal/sqlite/sqlite.go) opener now applies
`foreign_keys(ON)`, `synchronous(FULL)` and `busy_timeout(5000)` through the
driver DSN on every connection. It escapes literal paths and explicitly
retains one idle connection. WAL remains configured by each store's
migration; WAL mode persists across connections. The driver supports these
[DSN settings](https://pkg.go.dev/modernc.org/sqlite#Driver.Open), and SQLite
documents [WAL persistence](https://www.sqlite.org/pragma.html#pragma_journal_mode).

**Fixed: failed single-run archive deletion could permanently lose chunks.**
The chunk and parent deletions previously committed separately. Injecting a
failure on the parent deletion left one run with zero chunks, despite an
error returned to the caller. `LogDB.DeleteRun` now uses the existing atomic
batch implementation. The regression test verifies both records and blobs
survive a failure.

**Improved: archive batches repeatedly parsed the same upsert.** A CPU profile
of 64 small chunks attributed about 43% of sampled CPU cumulatively to the
SQLite parser. `PutChunks` now prepares its chunk upsert once for a batch
larger than one chunk. The statement closes before commit, with rollback on
any error. Single-chunk writes keep the direct execution path. Statement
reuse preserves replay metadata, original archive age, bounded memory, and
the existing commit-before-file-removal sequence.

**Improved: archive statistics used three independent queries.** `Stats` now
returns counts and total blob size from one statement and one consistent
snapshot. This cuts Go allocations substantially; its measured latency did
not change significantly.

## Benchmark method and results

The host reports Go 1.27.0, Linux amd64, QEMU Virtual CPU version 2.5+, eight
available CPUs, and an ext2/ext3 filesystem type. All benchmarks use
`GOMAXPROCS=2`. This is a shared virtual host, so the results describe this
environment and workload. They are not production capacity limits.

The archive comparison uses separately compiled before and after test
binaries, alternated serially for ten samples per version, with 200 ms per
benchmark. No other tests or benchmarks ran alongside it. Both versions use
the same Go toolchain, WAL/FULL settings and default checkpoint cadence.
Fixture setup is excluded from timing. Insert cases use fresh run IDs;
replay cases update the same archived run. Blobs are synthetic, already
compressed-storage inputs, and do not measure zstd compression.

| Archive operation | Before median | After median | Change |
|---|---:|---:|---:|
| Insert 16 chunks of 128 bytes | 218.6 µs | 126.4 µs | 42.2% less time |
| Replay 16 chunks of 128 bytes | 175.0 µs | 74.1 µs | 57.7% less time |
| Insert 64 chunks of 128 bytes | 735.6 µs | 339.2 µs | 53.9% less time |
| Replay 64 chunks of 128 bytes | 636.0 µs | 223.3 µs | 64.9% less time |
| Insert 64 chunks of 64 KiB | 4.271 ms | 3.625 ms | 15.1% less time |
| Replay 64 chunks of 64 KiB | 1.591 ms | 1.026 ms | 35.5% less time |

All six timing comparisons above have `p<0.001`, with ten samples per version.
Single-chunk insert latency had no statistically significant change. Small
single-chunk replay showed an 8.3% difference despite retaining the same
execution path; this is not evidence of a statement-reuse benefit.

| Resource measure | Before | After | Interpretation |
|---|---:|---:|---|
| 64 small inserted chunks, allocated Go bytes | 43.24 KiB/op | 42.01 KiB/op | 2.9% reduction |
| 64 small inserted chunks, allocations | 991/op | 932/op | 6.0% reduction |
| Archive statistics, allocated Go bytes | 2.524 KiB/op | 1.017 KiB/op | 59.7% reduction |
| Archive statistics, allocations | 64/op | 26/op | 59.4% reduction |
| Atomic single-run deletion, allocated Go bytes | 751 B/op | 1479 B/op | Transaction safety adds 728 bytes per call |

Statistics latency had no significant change. Single deletion took 44.67 µs
after the change versus 48.33 µs before (`p=0.011`), but the reason to retain
the change is atomicity. The normal retention path already batches deletions.

Existing metadata and log-read benchmarks were also measured serially with
six samples. These are baseline snapshots, not before/after comparisons:

| Workload | Median time | Allocated Go bytes | Allocations |
|---|---:|---:|---:|
| Metrics with 30,000 old and 500 recent runs | 578 µs | 92.7 KiB | 6,058 |
| Metrics with 30,000 runs in the window | 31.9 ms | 6.66 MiB | 360,100 |
| Delete 128 metadata runs individually | 11.38 ms | 51.0 KiB | 1,536 |
| Delete the same size metadata page in a batch | 792 µs | 12.5 KiB | 149 |
| Read one archived stream page | 944 µs | 6.33 MiB | 410 |
| Read a 5,000-frame archived stream backlog | 10.61 ms | 25.98 MiB | 7,705 |
| Download 20 MiB of incompressible archived output | 16.82 ms | 72.83 MiB | 6,383 |

The metrics query plan uses `MULTI-INDEX OR`, with the existing time,
end-time and active-status indexes. Metadata deletion batching also reduced
observed WAL growth from 29.65 KiB to 933 bytes per run in the benchmark
that disables auto-checkpointing to measure WAL growth.

`B/op` is cumulative Go allocation per operation, not peak resident memory.
The raw-download figure therefore does not mean 73 MiB is retained at once.
SQLite native allocations, process RSS, and file-cache memory require
separate measurements. WAL file growth is not total physical disk writes:
checkpoints can reuse the file without increasing its size.

## Daemon resource measurements

Six repetitions exercised the real Unix API, executor, archive and dashboard
reads while eight clients submitted 128 jobs producing 32 lines of output
each. The history case first adds 30,000 older and 500 recent runs.

| Workload | Throughput | Run admission p95 | Metrics p95 | Process RSS high-water |
|---|---:|---:|---:|---:|
| Loaded daemon | 680 runs/s | 16.16 ms | 2.67 ms | 29.94 MiB |
| Loaded daemon with history | 583 runs/s | 19.77 ms | 2.66 ms | 35.19 MiB |

The timed intervals used about 1.75 and 1.67 CPU cores respectively, out of
the two available to the Go scheduler. These are burst workloads, not idle
CPU estimates. Throughput includes submission and completion checks; it is
not a benchmark for arbitrary user commands. RSS is the test process's
high-water mark and includes startup and fixtures. Loaded repetitions share
a process, so later high-water marks can include earlier cases. CPU figures
cover the test process and exclude child job processes.

Retention snapshots were 30.19 ms for 2,000 runs with archived chunks and
19.26 ms for 128 runs with 1 MiB chunks. The latter allocated about 125 KiB
during the sweep, demonstrating that this path does not load the archived
128 MiB into Go memory. These measurements do not bound daily age pruning,
which uses a different query and transaction.

Idle measurements run each definition-count case in a fresh process to
prevent an earlier loaded case from contaminating its RSS high-water mark.
Each observation spans 31 seconds, including a scheduler clock check.
Six observations per case produced the following medians:

| Idle definitions | CPU time per second | Go heap after GC | Process RSS high-water | WAL growth |
|---|---:|---:|---:|---:|
| 0 | 0.0236 ms/s | 0.406 MiB | 17.33 MiB | 0 bytes |
| 100 | 0.0772 ms/s | 0.784 MiB | 22.95 MiB | 0 bytes |

CPU measurements varied substantially at these small absolute values
(confidence intervals of ±72% and ±25%). All twelve observations showed
zero WAL growth. The observation window does not include daily pruning or a
15-minute worker flush and does not establish a long-term resource ceiling.

## Remaining work and rewrite priorities

1. **Propagate maintenance cancellation.** Archive transfer and log-deletion
   calls still use `context.Background()`. A maintenance operation waiting
   for the sole connection can hold archive and run locks after daemon
   cancellation. Introduce context-aware maintenance methods, pass daemon
   contexts, and give final archival a bounded completion context. Test
   cancellation while a connection is held, checking that uncommitted buffer
   files remain available for retry.
2. **Make read deadlines effective while waiting for locks.** Log reads acquire
   a run `RWMutex` before checking their context. Cancellation cannot
   interrupt that wait. Review coordination together with the maintenance
   changes; avoid introducing a goroutine per waiting read.
3. **Reduce allocations in large dashboard windows and log reads.** The
   30,000-run metrics case and archive-read cases are measured sources of
   allocation churn. Profile row scanning and decoder/frame lifetimes next;
   retain exact percentile and frame-pagination semantics. Consider a small
   read pool only if measured connection wait time and write latency justify
   its extra cache memory.
4. **Bound daily archive age pruning.** `LogDB.Prune` deletes all expired
   chunks in one transaction and then removes empty parents. Unlike run
   retention, it has no page limit. Measure realistic multi-day archives
   before changing it, then use bounded transactions if lock duration or WAL
   writes become excessive.
5. **Improve scale and crash evidence.** The index-cost benchmark with only
   100 transitions was too small and noisy to support an index-removal
   decision. Its reported page usage was identical across cases. Benchmark
   steady state near the default 10,000 retained runs per definition, include
   scheduled-run lookup and tie-heavy pagination, and add subprocess crash
   tests at commit/file-cleanup boundaries. Use pool wait metrics and actual
   process write accounting alongside WAL size.

Keep schema/index changes separate for review with realistic data volumes,
consistent with the database skill's schema guidance. Keep the pure Go
driver, explicit SQL, bounded transfers, single warm connections and durable
commit boundaries as the starting design. This audit changes no schemas,
dependencies, API contracts or retention defaults.

## Validation and reproduction

The following passed after the implementation changes:

- `go test -race ./... -count=1`
- `make check contracts build` (tests, generated contracts, vet, and the
  `CGO_ENABLED=0` production build)
- `git diff --check`

Added tests cover literal database paths, replacement connection settings,
foreign-key rejection and alert cascades after replacement, rollback of
single-run archive deletion, and rollback of metadata and earlier chunks
when a prepared batch fails. Existing replay, archive age, partial-batch
recovery, retention and concurrency tests also pass. This is evidence for
the tested failure modes; hardware power-loss and long-duration production
soak tests were not performed.

Raw results are in [`benchmarks/2026-09-30-db`](benchmarks/2026-09-30-db/).
`logdb-comparison.txt` is the full ten-sample benchstat comparison;
`store-baseline.txt` includes query plans. Profile files and compiled test
binaries remain under `/tmp/minicrond-db-audit` for this session.

To measure the current implementation again:

```bash
GOMAXPROCS=2 go test ./internal/logdb -run '^$' \
  -bench 'Benchmark(PutChunks|Stats|DeleteRunWithChunk)$' \
  -benchmem -benchtime=200ms -count=10 > logdb.txt
GOMAXPROCS=2 go test ./internal/store -run '^$' \
  -bench 'BenchmarkRunMetrics(History|Window)$' \
  -benchmem -benchtime=200ms -count=6 > store.txt
GOMAXPROCS=2 go test ./internal/daemon -run '^$' \
  -bench 'Benchmark(DaemonLoaded|DaemonLoadedHistory|SweepRetentionArchived|SweepRetentionLargeArchive)$' \
  -benchmem -benchtime=1x -count=6 > daemon.txt
benchstat logdb.txt
```

For a comparison, compile test binaries before and after the change and
alternate their runs serially on the same host and toolchain. Do not compare
these saved measurements with a different machine as evidence of a code
improvement. Run idle cases in separate processes with `-benchtime=1x`; their
31-second observation window makes six samples for both cases take about
six minutes.
