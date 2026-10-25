# Memory and commit cost experiments (2026-10-25)

Follow-up to the [workload tiers](../2026-10-25-workload-tiers/README.md)
benchmark, which showed the large tier (100 workers, 300 jobs) using 414 MiB
median RSS and 0.8 s median fire→child latency. This page records what was
profiled, what was tried, and what was kept ([ADR-17](../../adr/0017-gc-pacing.md)).

Setup is the same as the tiers page: tree `ae6503a` plus the experiment named
in each column, `CGO_ENABLED=0 go build -trimpath`, Go 1.27.3, 4-vCPU KVM
guest on ext4, zero log viewers, daemon on 2 CPUs (`GOMAXPROCS=2`) for large
and normal and 1 CPU for small, children on CPUs 2-3. **120 s windows after a
20 s warm-up, 2 runs per cell, interleaved.** Cells are median [min–max].
Per-run JSON: [`raw/`](raw/).

## Profile of the large tier (one run, pprof)

| Finding | Evidence |
|---|---|
| Live heap is ~119 MiB, not 264 MiB | `heap?gc=1` in-use after a forced GC: 119 MiB; the monitor's 264 MiB counts unswept garbage |
| RSS is mostly retained heap headroom | runtime stats at the end: `HeapInuse` 130 MiB, `HeapIdle` 369 MiB of which only 126 MiB released, `NextGC` 249 MiB (default GOGC=100) |
| ~77% of allocation is per-run setup | 2.47 GB allocated in 105 s: zstd encoder creation 1.44 GB, Pipe read buffers and line buffers 0.74 GB |
| SQLite is the largest CPU consumer | 28% of CPU samples under `database/sql.ExecContext`, mostly syscalls; scheduler occurrence writes 12%, run finish 8% |
| Fsync pressure | system block layer: ~250 cache flushes/s at the large tier (the `blk_flush` counter of `/sys/block/sda/stat`, a system-wide proxy for fsync because the host was otherwise quiet) |

## Large tier: what each change did

| metric | default (GOGC 100) | **GOGC=50** | `GOMEMLIMIT=256MiB` | pooled encoders+buffers | pooled + GOGC=50 | `synchronous=normal` (measurement only) |
|---|---|---|---|---|---|---|
| CPU ms/s | 168 [163–173] | 172 [167–176] | **312** [302–322] | 158 [157–158] | 157 [155–159] | 117 [116–117] |
| RSS median MiB | 434 [430–438] | **281** [280–281] | 244 [241–246] | 355 [354–355] | 346 [345–348] | 478 [432–524] |
| RSS peak MiB | 594 [568–621] | 370 [350–391] | 318 [315–321] | 411 [410–411] | 365 [363–368] | 631 [581–681] |
| block flushes/s | 256 | 249 | 255 | 258 | 252 | **118** |
| fire→child p50 ms | 857 | 840 | 851 | 808 | 846 | **122** |
| fire→child p95 ms | 2,258 | 2,654 | 4,312 | 2,383 | 2,603 | **428** |
| runs skipped (`overlap_skip`) | 86 | 148 | 76 | 117 | 102 | **0** |

Reading it:

- **GOGC=50** removes about a third of resident memory at no CPU cost. Chosen.
- **`GOMEMLIMIT=256MiB`** saves a little more memory but nearly doubles CPU: live data is ~130 MiB, so the collector
  runs constantly near the limit. Rejected as a default (a limit well above live
  data would behave differently, but picking one is capacity planning, not a default).
- **Pooling** lowers CPU by about 8% but, combined with GOGC=50, raised median RSS from 281 to 346 MiB
  (`sync.Pool` keeps encoders alive across a collection, which raises the live heap the next GC target is computed
  from; this explanation was not separately verified). The memory-first order of AGENTS.md prefers the
  three-line GC change. Rejected; the code was reverted.
- **`synchronous=normal`** is **not proposed**: it weakens metadata durability, which AGENTS.md reserves to the owner. It
  is here only to measure what the commit fsyncs cost: flushes halve, CPU drops by a third and the median
  start latency falls 7×. Skips go to zero.

## Small and normal tiers: GC default check

Old = `ae6503a`, new = the same plus `GOGC` default 50.

| metric | small, old | small, new | normal, old | normal, new |
|---|---|---|---|---|
| CPU ms/s | 15.9 [15.8–15.9] | 16.1 [15.8–16.3] | 24 [24–25] | 25 [25–26] |
| RSS median MiB | 50 [49–51] | **43** [41–45] | 113 [111–116] | **84** [83–84] |
| RSS peak MiB | 55 | 45 | 132 | 114 |
| block flushes/s | 26 | 26 | 34 | 34 |
| fire→child p95 ms | 88 | 55 | 529 | 548 |

Memory falls 14–26% with CPU within about 1 ms/s (≤4%) and the same latency. On the large tier CPU is unchanged.

## Tried and dropped: a no-output fast path

Idea: a run that printed nothing could skip its chunk, index and directory
fsyncs (3 per run) and its archive pass. A quiet-cron variant of the large tier
(80% of jobs silent) showed **no change** (157 vs 160 flushes/s). The reason: every run has at
least 46 bytes of log, because the daemon writes its own system line (the exit status) into
each run, so no run is ever empty. The change was dead code and was reverted.
Making the exit line optional would change what users see and is not proposed.

## Unexplained: overload in the quiet variant

The quiet-cron variant (300 jobs, 5 s/15 s/1 min, 80% silent) behaved worse
than the standard large tier with **both** binaries: about 1,700 runs of ~3,500 were
skipped, 100 were queued, 596 hit `queue_full`, and fire→child p95 was 24–26 s.
This is the bounded queue (ADR-9) doing its job once the 256-job concurrency cap is reached
(5 queued runs/s drain rate), but it is **not understood why 256 slots were exhausted**: concurrent jobs
should be ~100–110. The silent jobs run 3.5 s against a 5 s cadence, so little slack remains, and run
completion appears to be held up by the metadata commits (see below). This was not
isolated. It needs its own investigation (per-slot hold time; a slot-occupancy
counter) before claiming a cause. It is a stress scenario, not a typical install.

## What is limiting the large tier

Evidence points at the **metadata commit rate**, not the log files:

- every run makes about 4–5 `synchronous=FULL` commits (schedule state before and after the fire, run row, `running`
  transition, terminal state; counted by reading the code, not measured), each an fsync, and the single SQLite writer serializes them;
- the log buffer adds about 3 fsyncs per run on close (chunk file, index file, directory);
- with metadata commits relaxed, flushes fall 250 → 118/s (the remaining ~118/s is probably mostly log files: ~3 per run at ~29 runs/s; not attributed),
  and start latency drops 7×.

Options that keep durability (not implemented; owner decision, see ADR-17 §Open):
group commit of concurrent metadata writes into one transaction; merging the schedule-state update into the
run-row insert; or moving the three close-time log fsyncs onto the existing grouped sync timer.

## Caveats

- 2 reps of 120 s per cell on one host with other tenants. The large tier's fire→child p95 varies 2–6 s between runs, so
  only differences much larger than that are meaningful (the `synchronous=normal` effect is; pooling's 5% CPU is borderline).
- The first experiment batch was discarded: a harness race (`du` over a data directory while the daemon removed a log
  directory) aborted one run and left its daemon running through the later runs. The harness now tolerates the race and kills
  its daemon on any exit; the batch was rerun in full.
- fsync counts are the system-wide block flush counter, not per-process, and not per-call. `strace` is not installed.
- One build was run during one pooled run (`pool` rep 1), which may have perturbed it slightly; the rep-2 values agree.
- Absolute numbers are for this CPU; compare ratios.

## Reproduce

```sh
CGO_ENABLED=0 go build -trimpath -o ~/mcb/minicrond ./cmd/minicrond
# variants are env vars on the same binary:  GOGC=100 (old behavior), GOGC=50, GOMEMLIMIT=256MiB,
# CFG_APPEND=$'[storage]\nsynchronous = "normal"'
SCENARIOS=tier-large REPS=1 DURATION=120 WARMUP=20 STRACE=0 CHILD_CPUS=2-3 \
  GOGC=100 scripts/bench-5a/run-profile.sh ~/mcb/minicrond ~/mcb/out
# profile: add PPROF_ADDR=127.0.0.1:6061 with a binary that starts net/http/pprof when MINICRON_BENCH_PPROF is set
```

New harness features used: scenarios `tier-*` and `tier-large-quiet`, `CFG_APPEND`, `JOB_KINDS`, a block-device flush/IO
counter (`BLKDEV`, default `sda`) in `sampler.py`/`analyze.py`.
