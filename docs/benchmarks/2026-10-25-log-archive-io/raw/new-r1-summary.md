| metric | active (n=1) | tier-normal (n=1) | tier-large (n=1) |
|---|---|---|---|
| CPU ms/s (stat utime+stime) | 13.9 | 20.7 | 128 |
|   user ms/s | 9.17 | 13.2 | 78.7 |
|   sys ms/s | 4.75 | 7.42 | 49.1 |
| CPU ms/s (schedstat) | 13.9 | 20.6 | 128 |
| CPU ms per 1 s, p95 | 20 | 54.3 | 329 |
| CPU ms per 1 s, max | 22.7 | 135 | 493 |
| context switches/s | 267 | 484 | 3382 |
| RSS median MiB | 40.7 | 84 | 292 |
| RSS max MiB | 44.2 | 115 | 455 |
| VmHWM MiB | 44 | 115 | 455 |
| threads max | 23 | 81 | 350 |
| goroutines (end) | 92 | 247 | 864 |
| Go heap objects MiB (end) | 13.2 | 100 | 271 |
| write_bytes KiB/s | 396 | 460 | 4374 |
| write syscalls/s (syscw) | 531 | 925 | 6085 |
| read syscalls/s (syscr) | 196 | 305 | 2519 |
| block flush requests/s (system-wide, fsync proxy) | 12 | 17.2 | 129 |
| block write IOs/s (system-wide) | 40.1 | 52.7 | 407 |
| data dir du growth KiB/min | 1904 | 3246 | 6860 |
|   durable growth KiB/min (db files + log buffers, excl. WAL) | 658 | 1271 | 6740 |
|     metadata db KiB/min | 104 | 262 | 998 |
|     log archive db KiB/min | 154 | 264 | 2372 |
|     live log buffers KiB/min | 400 | 745 | 3370 |
| data dir du after shutdown MiB | 1.7 | 2.91 | 16.5 |
| fsync/s (strace) | – | – | – |
| fdatasync/s (strace) | – | – | – |
| write/s (strace) | – | – | – |
| read/s (strace) | – | – | – |
| futex/s (strace) | – | – | – |
| epoll_pwait/s (strace) | – | – | – |
| nanosleep/s (strace) | – | – | – |
| all syscalls/s (strace) | – | – | – |
| runs started in window | 357 | 392 | 3524 |
| fire→start p50 ms (1s jobs) | 12.9 | – | – |
| fire→start p95 ms (1s jobs) | 39.7 | – | – |
| fire→start p50 ms (5s jobs) | 33.4 | – | – |
| fire→start p95 ms (5s jobs) | 49.1 | – | – |
| fire→start max ms (all) | 55.8 | 1309 | 1610 |
| fire→child p50 ms | 20.7 | 91.3 | 327 |
| fire→child p95 ms | 48.3 | 314 | 1055 |
| restart overhead p50 ms (child exit→next child start − delay) | 17.3 | 3018 | 3018 |
| restart overhead p95 ms | 23 | 3558 | 3468 |
| restart overhead max ms | 24 | 4047 | 3675 |
| child exit→run ended p50 ms | 0.67 | 0.73 | 0.5 |
| host busy % of 4 CPUs (noise) | 2.1 | 2.7 | 10.4 |
| host load1 median (noise) | 0.28 | 0.23 | 0.7 |
| shutdown s | 0.092 | 0.164 | 0.419 |
| daemon WARN/ERROR lines | 0 | 0 | 0 |
