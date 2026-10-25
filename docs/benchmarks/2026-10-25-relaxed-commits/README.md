# Relaxed commits for the running transition and schedule state (2026-10-25)

A/B for [ADR-18](../../adr/0018-relaxed-commits.md): `StartRun` and the
schedule-state upsert are committed with `synchronous=NORMAL`, everything else
stays `FULL`.

- **base** = tree `5f08d26` (GOGC=50 default, all commits FULL)
- **new** = `5f08d26` plus the ADR-18 change

Setup as in the [memory and commits](../2026-10-25-memory-and-commits/README.md) page: `CGO_ENABLED=0 go build -trimpath`,
Go 1.27.3, 4-vCPU KVM guest (AMD Ryzen AI 9 HX 370), ext4 on `/dev/sda1`
(real disk, not tmpfs), zero log viewers. Daemon on 2 CPUs (`GOMAXPROCS=2`) for
large and normal, 1 CPU for small (`active`, the 5A profile); children on
CPUs 2-3. **120 s windows after a 20 s warm-up, serial and interleaved: 3 runs
per cell for large, 2 for normal and small.** Cells are median [min–max].
Per-run JSON: [`raw/`](raw/), host details: [`environment.txt`](environment.txt).

## Large tier (100 workers, 300 jobs, ≈29 runs/s)

| metric | base | new |
|---|---|---|
| block flushes/s (system-wide fsync proxy) | 254 [250–262] | **190** [189–200] |
| block write IOs/s | 748 [742–772] | 591 [590–616] |
| CPU ms/s | 166 [165–172] | 150 [146–155] |
| RSS median / max MiB | 284 / 418 | 286 / 392 |
| fire→child p50 ms | 793 [769–807] | **478** [469–478] |
| fire→child p95 ms | 2,096 [2,027–2,474] | 1,509 [1,364–2,983] |
| runs skipped (`overlap_skip`), per run | 115 [37–130] | 63 [1–81] |
| exit→ended p95 ms | 28 [27–31] | 35 [33–41] |
| runs in window | 3,502 | 3,507 |

## Normal (25 workers, 60 jobs) and small (5A `active`)

| metric | normal base | normal new | small base | small new |
|---|---|---|---|---|
| block flushes/s | 34 | **27** | 26 | **21** |
| CPU ms/s | 25 | 23 | 16 | 16 |
| RSS median MiB | 86 | 83 | 42 | 42 |
| fire→child p50 ms | 180 | **101** | 32 | **20** |
| fire→child p95 ms | 523 | **342** | 87 | **45** |
| exit→ended p95 ms | 39 | 44 | 68 [24–111] | 26 |
| skipped runs | 0 | 0 | 1 | 0 |

No daemon warnings or errors in any run.

## Reading it

- **Flushes fall by about 64/s on the large tier** (254 → 190), close to the
  expected 2 of 4 metadata fsyncs per run at ≈29 runs/s, and by 5–7/s on the
  smaller tiers. `synchronous=normal` for everything reached 118/s
  ([ADR-17](../../adr/0017-gc-pacing.md)); this change recovers about half of that gap.
- **Start latency:** the large-tier median falls 40% (793 → 478 ms), normal 44%,
  small 38%. The p95 falls too, except for one large-tier run (2,983 ms), which
  is inside the 2–6 s run-to-run spread the earlier pages report for this tier.
- **Skips about halve** on the large tier but vary widely between runs (1–81 vs
  37–130); treat this as a direction, not a size.
- **CPU drops ~10%** on large (fewer fsync syscalls and wakeups), unchanged on small.
- **exit→ended p95 rises 5–7 ms** on large and normal, with ranges that do not
  overlap on large. A likely cause is that more runs start in the same burst,
  so terminal writes queue behind more `CreateRun`s on the single writer; the two
  extra `PRAGMA` statements per relaxed write may also contribute. Not isolated. It
  is milliseconds against a 1–3 s tolerance and does not delay process control,
  which never waits for history (ADR-16).
- RSS is unchanged.

## Caveats

- Few reps on a shared host. Only the flush count and the start-latency medians
  separate cleanly; skips, p95 and exit→ended are within or near the noise.
- fsync counts come from the system-wide block flush counter, not per process.
  `strace` is not installed.
- No power-loss test was run. Crash safety tested is a daemon SIGKILL
  (`TestRelaxedWritesSurviveDaemonCrash`).
- Absolute numbers are for this CPU; compare ratios.

## Reproduce

```sh
git worktree add --detach ../base 5f08d26 && (cd ../base && CGO_ENABLED=0 go build -trimpath -o ~/mcb/bin-base ./cmd/minicrond)
CGO_ENABLED=0 go build -trimpath -o ~/mcb/bin-new ./cmd/minicrond
# per cell, alternating base/new:
SCENARIOS=tier-large REPS=1 DURATION=120 WARMUP=20 STRACE=0 CHILD_CPUS=2-3 \
  scripts/bench-5a/run-profile.sh ~/mcb/bin-new ~/mcb/relaxed/new-tier-large-r1
```
