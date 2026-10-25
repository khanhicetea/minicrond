| metric | active (n=1) | tier-normal (n=1) | tier-large (n=1) |
|---|---|---|---|
| CPU ms/s (stat utime+stime) | 14.9 | 22.6 | 150 |
|   user ms/s | 9.42 | 14.1 | 89 |
|   sys ms/s | 5.5 | 8.5 | 60.8 |
| CPU ms/s (schedstat) | 14.9 | 22.7 | 150 |
| CPU ms per 1 s, p95 | 23.1 | 61 | 356 |
| CPU ms per 1 s, max | 30.4 | 156 | 423 |
| context switches/s | 355 | 653 | 4618 |
| RSS median MiB | 43 | 89.5 | 292 |
| RSS max MiB | 45.2 | 129 | 423 |
| VmHWM MiB | 45.2 | 129 | 423 |
| threads max | 24 | 81 | 348 |
| goroutines (end) | 90 | 232 | 939 |
| Go heap objects MiB (end) | 12.7 | 92 | 254 |
| write_bytes KiB/s | 405 | 464 | 4396 |
| write syscalls/s (syscw) | 535 | 923 | 6087 |
| read syscalls/s (syscr) | 195 | 308 | 2531 |
| block flush requests/s (system-wide, fsync proxy) | 20.6 | 27.1 | 194 |
| block write IOs/s (system-wide) | 63.9 | 80 | 603 |
| data dir du growth KiB/min | 1894 | 3182 | 7362 |
|   durable growth KiB/min (db files + log buffers, excl. WAL) | 666 | 1262 | 6702 |
|     metadata db KiB/min | 104 | 260 | 982 |
|     log archive db KiB/min | 158 | 260 | 2312 |
|     live log buffers KiB/min | 404 | 742 | 3408 |
| data dir du after shutdown MiB | 1.68 | 2.8 | 16.2 |
| fsync/s (strace) | – | – | – |
| fdatasync/s (strace) | – | – | – |
| write/s (strace) | – | – | – |
| read/s (strace) | – | – | – |
| futex/s (strace) | – | – | – |
| epoll_pwait/s (strace) | – | – | – |
| nanosleep/s (strace) | – | – | – |
| all syscalls/s (strace) | – | – | – |
| runs started in window | 353 | 386 | 3513 |
| fire→start p50 ms (1s jobs) | 13.6 | – | – |
| fire→start p95 ms (1s jobs) | 40.4 | – | – |
| fire→start p50 ms (5s jobs) | 26.2 | – | – |
| fire→start p95 ms (5s jobs) | 41.3 | – | – |
| fire→start max ms (all) | 65 | 389 | 1814 |
| fire→child p50 ms | 20.8 | 103 | 491 |
| fire→child p95 ms | 45.6 | 328 | 1353 |
| restart overhead p50 ms (child exit→next child start − delay) | 33.2 | 3046 | 3054 |
| restart overhead p95 ms | 43.7 | 3209 | 3482 |
| restart overhead max ms | 44.2 | 3363 | 3570 |
| child exit→run ended p50 ms | 15.9 | 27.7 | 31 |
| host busy % of 4 CPUs (noise) | 2.1 | 2.8 | 11.2 |
| host load1 median (noise) | 0.33 | 0.21 | 2.83 |
| shutdown s | 0.123 | 0.24 | 4.08 |
| daemon WARN/ERROR lines | 0 | 0 | 0 |
