# Storage-fault re-test on a real filesystem (2026-10-24, ADR-16)

Re-run of the fault scenarios against the execution-first code, with
`scripts/storage-faults/run.sh <binary> <out-dir>` (sudo, ext4 loop image).

- Binary: this branch after the fixes listed below, plain `make build`.
- Host: 4-vCPU KVM guest, Linux 6.12; data directory on a 96 MiB ext4 image
  made with `-m 0`. Liveness markers are written outside the image.
- Workload: a `@every 1s` job printing ~2.7 KiB, and a worker that lives 2 s,
  exits 1 and restarts after 1 s (`healthy_after = 1`, so it never retires).
  20 s per phase, so the expected counts are 20 job runs and 6–7 worker starts.

```
healthy                 20s  job runs  20  worker starts   7  daemon alive  deferred=0 dropped=0 pending=0 read_only=False storage_degraded=False
free after fill:  1024 B
disk-full               20s  job runs  20  worker starts   7  daemon alive  deferred=27 dropped=0 pending=27 read_only=False storage_degraded=True
space-freed             20s  job runs  20  worker starts   7  daemon alive  deferred=28 dropped=0 pending=0 read_only=False storage_degraded=False
rows after recovery: flaky:22 tick:63
db-write-locked         20s  job runs  16  worker starts   4  daemon alive  deferred=48 dropped=0 pending=21 read_only=False storage_degraded=True
lock-released           20s  job runs  20  worker starts   7  daemon alive  deferred=49 dropped=0 pending=0 read_only=False storage_degraded=False
shutdown (healthy) 0.01 s
boot-on-full-disk       20s  job runs  20  worker starts   7  daemon alive  deferred=31 dropped=0 pending=31 read_only=True storage_degraded=True
fs-shutdown             20s  job runs  20  worker starts   7  daemon alive  deferred=58 dropped=0 pending=58 read_only=True storage_degraded=True
shutdown (dead fs) 0.0 s
boot on dead fs: exit 1 (expected: the metadata database is unreadable)
```

Every phase keeps the job and the worker running. The write-locked phase
loses its first seconds: before the executor marks storage degraded, each
write waits SQLite's 5 s busy handler (it ignores contexts), once per
writer. History written behind execution reached the database after each
fault cleared (`pending=0`); nothing was dropped. A full disk at boot opens
the metadata database read-only (`read_only=True`); a dead filesystem at
boot is fatal because the database cannot be read (owner decision).

## Issues found and fixed in this pass

- **Write lock stalled scheduled jobs** (2 of 15 runs): every fire waited
  for its schedule-state write in the busy handler. Schedule-state writes are
  now skipped while the executor reports storage degraded.
- **Full disk at boot was fatal** (`SQLITE_IOERR_SHMSIZE`: the WAL index
  cannot grow) although the database was readable. Boot now falls back to a
  read-only open whenever the normal open fails and the schema is current.
- **Workers ignored `scheduler.shutdown_grace`** (found with real
  `docker stop`, 10.2 s, exit 137): supervision stopped them as ordinary
  stops. Now 5.2 s and exit 0.
- Per-line `writing run system log failed` errors for degraded log writers.

## Remaining

- The daemon log is noisy while degraded: each run logs its log-open failure
  and each history retry (see `log-summary.txt` in the output directory;
  bounded by the retry backoff, not rate-limited).
- In read-only mode the history writer still retries (and logs) writes that
  can never succeed until its 1024-run budget fills.
- Power loss and `EIO` mid-write on the log archive were not tested.
