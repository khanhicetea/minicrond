# 5A small-server baseline (2026-10-22, before ADR-16)

Partial results from `scripts/bench-5a/run-profile.sh`; the run was stopped
before profiling and the A/B comparison against ADR-16 finished.

- Binary: `dc86de1` (the tree **before** the execution-first change), built
  with plain `go build`.
- Host: 4-vCPU KVM guest, ext4 data dir; daemon pinned to 1 CPU
  (`taskset -c 0`, `GOMAXPROCS=1`), children on other CPUs; zero log viewers.
- Window: 180 s per run after a 20 s warm-up. Values are median
  [min–max] over n runs. Other agents ran on the same host, so treat small
  differences as noise.
- Scenarios:
  - `idle-empty`: no definitions.
  - `idle-sleepers`: 10 silent sleeping workers.
  - `active`: 10 workers (2 silent, 1 crash-loop, 4 printing one line a
    second, 2 bursty, 1 chatty), four `@every 5s` jobs and two `@every 1s`
    jobs.
  - `active-flush1`: `active` with `worker_flush_interval = 1`.

Observation worth following up: Go heap grows from 0.6 MiB (no
definitions) to about 18 MiB with 10 *silent* workers, roughly 1.7 MiB per
open run. That points at per-writer buffers (likely each run's zstd encoder)
and matters for the 512 MiB target. It is a hypothesis; it has not been
profiled yet.

Reproduce with `scripts/bench-5a/run-profile.sh <minicrond-binary> <out-dir>`
(root; needs `strace`, `taskset` and python3).

| metric | idle-empty (n=3) | idle-sleepers (n=3) | active (n=3) | active-flush1 (n=2) |
|---|---|---|---|---|
| CPU ms/s (stat utime+stime) | 0 [0–0] | 0 [0–0] | 22.3 [22.2–23.7] | 22.1 [21.7–22.5] |
|   user ms/s | 0 [0–0] | 0 [0–0] | 11.5 [11.5–12.4] | 11.4 [11.2–11.5] |
|   sys ms/s | 0 [0–0] | 0 [0–0] | 10.8 [10.7–11.3] | 10.8 [10.5–11] |
| CPU ms/s (schedstat) | 0.012 [0.002–0.013] | 0.021 [0.012–0.024] | 22.2 [22.2–23.7] | 22.1 [21.7–22.4] |
| CPU ms per 1 s, p95 | 0 [0–0] | 0 [0–0] | 34.1 [31.6–37.9] | 33.5 [32.1–34.9] |
| CPU ms per 1 s, max | 1.86 [0.12–2.12] | 2.2 [1.69–2.34] | 45.1 [43–46.8] | 58.7 [55.7–61.8] |
| context switches/s | 0.3 [0.1–0.3] | 0.3 [0.1–0.4] | 393 [385–424] | 399 [397–402] |
| RSS median MiB | 17.2 [17.2–17.2] | 21.9 [21.8–22] | 64.4 [63–64.5] | 65.5 [65.3–65.7] |
| RSS max MiB | 17.2 [17.2–17.2] | 22.1 [21.9–22.1] | 69.9 [67.8–70.3] | 69.9 [69.3–70.5] |
| VmHWM MiB | 17.2 [17.2–17.2] | 22.1 [21.9–22.1] | 69.8 [67.7–70.2] | 70 [69.3–70.8] |
| threads max | 6 [6–7] | 17 [17–17] | 23 [23–24] | 24 [24–24] |
| goroutines (end) | 22 [22–22] | 72 [72–72] | 94 [94–94] | 94 [94–94] |
| Go heap objects MiB (end) | 0.6 [0.6–0.6] | 17.9 [17.9–17.9] | 44.9 [44.8–45] | 39.9 [35.1–44.8] |
| write_bytes KiB/s | 0 [0–0] | 0 [0–0] | 453 [444–456] | 473 [470–475] |
| write syscalls/s (syscw) | 0 [0–0] | 0 [0–0] | 537 [531–537] | 540 [540–541] |
| read syscalls/s (syscr) | 0 [0–0] | 0 [0–0] | 192 [191–194] | 197 [196–198] |
| data dir du growth KiB/min | 0 [0–0] | 0 [0–0] | 1449 [1445–1464] | 1464 [1456–1472] |
|   durable growth KiB/min (db files + log buffers, excl. WAL) | 0 [0–0] | 0 [0–0] | 627 [625–645] | 647 [638–656] |
|     metadata db KiB/min | 0 [0–0] | 0 [0–0] | 100 [98.7–101] | 101 [100–101] |
|     log archive db KiB/min | 0 [0–0] | 0 [0–0] | 123 [123–144] | 553 [544–563] |
|     live log buffers KiB/min | 0 [0–0] | 0 [0–0] | 402 [402–403] | -6.9 [-7.1–-6.7] |
| data dir du after shutdown MiB | 0.18 [0.18–0.18] | 0.2 [0.2–0.2] | 2.57 [2.56–2.65] | 2.59 [2.56–2.62] |
| fsync/s (strace) | – | – | 30.1 [30–30.3] | 30.2 [30–30.5] |
| fdatasync/s (strace) | – | – | – | – |
| write/s (strace) | – | – | 325 [314–344] | 328 [328–329] |
| read/s (strace) | – | – | 62.7 [62.5–63.1] | 63 [62.7–63.2] |
| futex/s (strace) | 0.03 [0.03–0.03] | 0.03 [0.03–0.03] | 98.4 [92.9–104] | 95.7 [93.2–98.2] |
| epoll_pwait/s (strace) | 0.17 [0.17–0.17] | 0.17 [0.17–0.17] | 45.9 [42.6–46.4] | 46 [45.3–46.6] |
| nanosleep/s (strace) | 1.13 [0.67–1.4] | 1.47 [1.47–1.73] | 558 [553–563] | 554 [543–564] |
| all syscalls/s (strace) | 1.37 [0.9–1.63] | 1.7 [1.7–1.97] | 1750 [1722–1758] | 1740 [1720–1759] |
| runs started in window | 0 [0–0] | 0 [0–0] | 530 [530–530] | 530 [530–530] |
| fire→start p50 ms (1s jobs) | – | – | 9.97 [5.87–10.2] | 6.74 [5.24–8.24] |
| fire→start p95 ms (1s jobs) | – | – | 22.4 [14.4–29.4] | 21.3 [14.3–28.3] |
| fire→start p50 ms (5s jobs) | – | – | 6.8 [6.51–14.3] | 8.44 [5.14–11.7] |
| fire→start p95 ms (5s jobs) | – | – | 20.1 [10.5–35.1] | 19.3 [13.5–25.1] |
| fire→start max ms (all) | – | – | 60 [21.6–144] | 55.8 [27.6–84] |
| fire→child p50 ms | – | – | 17 [11.1–18.8] | 14 [13.7–14.2] |
| fire→child p95 ms | – | – | 32.5 [19–41.9] | 29.9 [23.8–36] |
| restart overhead p50 ms (child exit→next child start − delay) | – | – | 15 [14.5–15.6] | 15 [14.8–15.3] |
| restart overhead p95 ms | – | – | 19.9 [19.2–21.4] | 17.9 [17.8–17.9] |
| restart overhead max ms | – | – | 23.1 [21.7–40.9] | 18.4 [18.2–18.7] |
| child exit→run ended p50 ms | – | – | 3.27 [3.19–3.64] | 3.21 [3.15–3.28] |
| host busy % of 4 CPUs (noise) | 17.4 [9.4–25.4] | 12.3 [8.4–29.8] | 8.2 [4–11.6] | 5.1 [4.3–5.9] |
| host load1 median (noise) | 0.53 [0.21–0.85] | 0.6 [0.28–1.66] | 0.61 [0.37–1.2] | 0.33 [0.16–0.5] |
| shutdown s | 0.011 [0.009–0.011] | 0.041 [0.038–0.044] | 1.54 [1.44–1.81] | 1.61 [1.16–2.06] |
| daemon WARN/ERROR lines | 0 [0–0] | 0 [0–0] | 0 [0–0] | 0 [0–0] |
