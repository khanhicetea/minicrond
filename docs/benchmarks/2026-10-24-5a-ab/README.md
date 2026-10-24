# 5A A/B (2026-10-24): before ADR-16, ADR-16, and this round

`scripts/bench-5a/run-profile.sh`, scenarios `idle-sleepers` and `active`
(see the [baseline](../2026-10-22-5a-baseline/README.md)), 2 reps × 120 s after
a 20 s warm-up, run serially per binary on the same 4-vCPU KVM guest, ext4
data dir (`/var/tmp`), daemon pinned to CPU 0 with `GOMAXPROCS=1`. strace was
not available, so syscall rows are empty. Values are median [min–max], n=2:
treat differences under ~10% as noise.

Binaries: `dc86de1` (before ADR-16), `defade7` (ADR-16), `53e34e2` (this round:
128 KiB log encoder window, breaker-aware schedule writes, shutdown_grace…).

| metric | dc86de1 | defade7 | 53e34e2 |
|---|---|---|---|
| idle-sleepers RSS median MiB | 37.9 | 37.9 | **30.1** |
| idle-sleepers Go heap MiB | 17.8 | 17.8 | **9.0** |
| active CPU ms/s | 13.6 | 12.9 | 12.5 |
| active RSS median MiB | 75.3 | 75.0 | **48.8** |
| active VmHWM MiB | – | 82.4 | **52.1** |
| active Go heap MiB | 36.7 | 29.2 | **15.9** |
| active fire→start p50 ms (1 s jobs) | 11.4 | 16.8 | 18.4 |
| active restart overhead p95 ms | – | 43.0 | 38.8 |
| daemon WARN/ERROR lines | 0 | 0 | 0 |

ADR-16 did not cost measurable CPU or memory. This round cuts resident memory
of the 10-worker active profile by about 35% (75 → 49 MiB) and halves the Go
heap, with CPU, write volume and disk growth unchanged. The fire→start p50
difference between dc86de1 and later builds (11 → 17 ms) is within this
host's run-to-run variation in earlier runs but was not investigated.
Full tables: `dc86de1-vs-defade7.md`, `defade7-vs-53e34e2.md`.
