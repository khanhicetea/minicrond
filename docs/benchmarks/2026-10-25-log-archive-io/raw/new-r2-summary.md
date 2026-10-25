| metric | active (n=1) | tier-normal (n=1) | tier-large (n=1) |
|---|---|---|---|
| CPU ms/s (stat utime+stime) | 14.3 | 19.5 | 125 |
|   user ms/s | 9.17 | 12.3 | 75.8 |
|   sys ms/s | 5.17 | 7.17 | 49.2 |
| CPU ms/s (schedstat) | 14.3 | 19.6 | 125 |
| CPU ms per 1 s, p95 | 22.1 | 50.7 | 335 |
| CPU ms per 1 s, max | 24.1 | 114 | 448 |
| context switches/s | 265 | 469 | 3310 |
| RSS median MiB | 42.1 | 85.4 | 288 |
| RSS max MiB | 45 | 121 | 460 |
| VmHWM MiB | 44.9 | 121 | 460 |
| threads max | 23 | 81 | 348 |
| goroutines (end) | 90 | 241 | 842 |
| Go heap objects MiB (end) | 13 | 30.9 | 122 |
| write_bytes KiB/s | 395 | 458 | 4283 |
| write syscalls/s (syscw) | 534 | 922 | 6029 |
| read syscalls/s (syscr) | 194 | 306 | 2473 |
| block flush requests/s (system-wide, fsync proxy) | 12.1 | 17.2 | 126 |
| block write IOs/s (system-wide) | 38.6 | 52.7 | 397 |
| data dir du growth KiB/min | 1884 | 3202 | 6842 |
|   durable growth KiB/min (db files + log buffers, excl. WAL) | 669 | 1265 | 6733 |
|     metadata db KiB/min | 104 | 258 | 994 |
|     log archive db KiB/min | 162 | 264 | 2372 |
|     live log buffers KiB/min | 403 | 743 | 3367 |
| data dir du after shutdown MiB | 1.74 | 2.9 | 16.5 |
| fsync/s (strace) | – | – | – |
| fdatasync/s (strace) | – | – | – |
| write/s (strace) | – | – | – |
| read/s (strace) | – | – | – |
| futex/s (strace) | – | – | – |
| epoll_pwait/s (strace) | – | – | – |
| nanosleep/s (strace) | – | – | – |
| all syscalls/s (strace) | – | – | – |
| runs started in window | 355 | 388 | 3523 |
| fire→start p50 ms (1s jobs) | 12.7 | – | – |
| fire→start p95 ms (1s jobs) | 37.5 | – | – |
| fire→start p50 ms (5s jobs) | 28 | – | – |
| fire→start p95 ms (5s jobs) | 45 | – | – |
| fire→start max ms (all) | 58.8 | 322 | 2085 |
| fire→child p50 ms | 19.9 | 80.9 | 370 |
| fire→child p95 ms | 46 | 270 | 1209 |
| restart overhead p50 ms (child exit→next child start − delay) | 18.3 | 3017 | 3017 |
| restart overhead p95 ms | 23.3 | 3070 | 4058 |
| restart overhead max ms | 23.5 | 3092 | 4586 |
| child exit→run ended p50 ms | 0.94 | 0.66 | 0.56 |
| host busy % of 4 CPUs (noise) | 2.1 | 2.8 | 10.7 |
| host load1 median (noise) | 1.05 | 0.32 | 0.89 |
| shutdown s | 0.084 | 0.139 | 0.419 |
| daemon WARN/ERROR lines | 0 | 0 | 0 |
