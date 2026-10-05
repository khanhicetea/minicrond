# A06: archive cursor reads (`logdb.EachChunk`)

Benchmark: `BenchmarkEachChunkCursor` in `internal/logdb/logdb_bench_test.go`
(one run, 1-byte blobs so only index traversal is measured, one chunk per frame
sequence). `final_page` returns the last chunk, `empty_tail` is a follower's poll
with nothing new, `full_pagination` reads the whole run in pages of 8 chunks.

- Machine: AMD Ryzen AI 9 HX 370, 4 vCPU, Go 1.27.0, linux/amd64, SQLite via the
  project's `internal/sqlite` driver.
- Storage: **real disk** (`/dev/sda1`, ext4-style root filesystem, `TMPDIR=/home/kit/tmp-bench`), not tmpfs.
- Runs were serial. Small sizes: `-benchtime=200x -count=3`; 100k poll cases
  `-benchtime=50x -count=3`; 100k full pagination `1x` before (58 s) and `3x`
  after. Raw output sits next to this file (`before-*.txt`, `after-*.txt`).
- Before = `bbd55c3` (`WHERE run_id=? AND last_seq>? AND number>? ORDER BY number`,
  plan: walk `sqlite_autoindex_log_chunks_1`, filter `last_seq` per row).
  After = `ORDER BY last_seq LIMIT 1` with the cursor advanced to each chunk's last
  sequence (plan: `SEARCH ... USING INDEX idx_log_chunks_seq (run_id=? AND last_seq>?)`).

Median of the repetitions, per operation:

| chunks | final_page before → after | empty_tail before → after | full_pagination before → after |
|---:|---:|---:|---:|
| 100 | 34 µs → 41 µs (noisy, 17-51) | 44 µs → 8 µs | 1.40 ms → 1.06 ms |
| 1,000 | 110 µs → 28 µs | 100 µs → 17 µs | 19.7 ms → 12.2 ms |
| 10,000 | 859 µs → 44 µs | 878 µs → 11 µs | 726 ms → 125 ms |
| 100,000 | 9.7 ms → 32 µs | 9.1 ms → 11 µs | 58.6 s → 1.24 s |

Before, a cursor read grew linearly with the chunks before the cursor
(25 µs → 9.7 ms from 100 to 100k chunks) and full pagination grew
quadratically (58.6 s at 100k). After, the poll cost is flat (tens of µs, within
timer noise from 100 to 100k chunks) and full pagination is linear (about 12 µs
per chunk, 1.24 s at 100k).

Caveats: single machine, one SQLite file in the page cache (warm); the
numbers show the algorithmic change, not cold-disk latency. Blobs are tiny, so
real runs add blob read and decompression cost to both columns equally.
