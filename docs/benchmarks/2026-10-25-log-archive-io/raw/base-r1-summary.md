| metric | active (n=1) | tier-normal (n=1) | tier-large (n=1) |
|---|---|---|---|
| CPU ms/s (stat utime+stime) | 15.8 | 23.4 | 148 |
|   user ms/s | 8.92 | 14.6 | 83.1 |
|   sys ms/s | 6.83 | 8.83 | 64.4 |
| CPU ms/s (schedstat) | 15.7 | 23.4 | 147 |
| CPU ms per 1 s, p95 | 24.1 | 70.7 | 361 |
| CPU ms per 1 s, max | 27.8 | 167 | 408 |
| context switches/s | 358 | 648 | 4562 |
| RSS median MiB | 43.6 | 89.9 | 271 |
| RSS max MiB | 45.6 | 115 | 418 |
| VmHWM MiB | 46.3 | 114 | 418 |
| threads max | 24 | 82 | 348 |
| goroutines (end) | 93 | 224 | 863 |
| Go heap objects MiB (end) | 15.7 | 89.1 | 263 |
| write_bytes KiB/s | 403 | 463 | 4410 |
| write syscalls/s (syscw) | 526 | 922 | 6067 |
| read syscalls/s (syscr) | 196 | 307 | 2493 |
| block flush requests/s (system-wide, fsync proxy) | 20.6 | 26.7 | 194 |
| block write IOs/s (system-wide) | 66.2 | 81.3 | 601 |
| data dir du growth KiB/min | 1908 | 3266 | 6778 |
|   durable growth KiB/min (db files + log buffers, excl. WAL) | 663 | 1264 | 6686 |
|     metadata db KiB/min | 102 | 262 | 992 |
|     log archive db KiB/min | 156 | 260 | 2324 |
|     live log buffers KiB/min | 405 | 742 | 3370 |
| data dir du after shutdown MiB | 1.66 | 2.8 | 16.2 |
| fsync/s (strace) | – | – | – |
| fdatasync/s (strace) | – | – | – |
| write/s (strace) | – | – | – |
| read/s (strace) | – | – | – |
| futex/s (strace) | – | – | – |
| epoll_pwait/s (strace) | – | – | – |
| nanosleep/s (strace) | – | – | – |
| all syscalls/s (strace) | – | – | – |
| runs started in window | 356 | 387 | 3526 |
| fire→start p50 ms (1s jobs) | 13.4 | – | – |
| fire→start p95 ms (1s jobs) | 39.8 | – | – |
| fire→start p50 ms (5s jobs) | 29.2 | – | – |
| fire→start p95 ms (5s jobs) | 44.5 | – | – |
| fire→start max ms (all) | 61.1 | 423 | 2087 |
| fire→child p50 ms | 20.2 | 101 | 494 |
| fire→child p95 ms | 46.9 | 357 | 1447 |
| restart overhead p50 ms (child exit→next child start − delay) | 34.6 | 3048 | 3059 |
| restart overhead p95 ms | 43.6 | 3066 | 3475 |
| restart overhead max ms | 43.8 | 3073 | 3665 |
| child exit→run ended p50 ms | 18.3 | 31.9 | 28.9 |
| host busy % of 4 CPUs (noise) | 3.8 | 4.5 | 11.1 |
| host load1 median (noise) | 0.25 | 1.32 | 2.23 |
| shutdown s | 0.127 | 0.256 | 0.671 |
| daemon WARN/ERROR lines | 0 | 0 | 0 |
