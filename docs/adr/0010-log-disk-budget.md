# ADR-10: Log disk budget outranks retention age (ADR-8 3A), quarantine policy, archive cursor

- Status: accepted; implemented (merge of `feat/disk`, `e815a97`)
- Decision source: owner choice **3A** in [ADR-8](0008-background-first-trade-offs.md), audit "additional capacity observations" 1 and 3, finding A06
- Related: [ADR-6](0006-hybrid-log-storage.md), [operations](../operations.md), [configuration](../configuration.md)

## Context (the zero-viewer cost addressed)

Almost every run is unattended, so log disk use is driven by background
output, not by readers. Before this change nothing bounded it as a whole:

- `log_max` bounds one run's hot buffer, not the disk. Successful archival
  releases that capacity.
- `logs.db_max_size` is disabled by default, runs once a day, counts only the
  archive's data pages (not its WAL), and ignores live and sealed buffers and
  the quarantine. Between sweeps the budget can be exceeded without limit.
- The only reaction to a full disk was the capture-failure policy (2A): every
  run discards its output and the disk stays full.
- The quarantine (corrupt chunks kept for repair) had no size or age limit.

3A says: delete the oldest *eligible* logs early when approaching the budget,
account for every log tier and for headroom, keep quarantine under its own
explicit policy, never reclaim metadata or pending execution records, and let
2A (existing and new runs' output) and, once implemented, 4B (admission of new work) govern what happens when reclamation is not enough.

## Decision

### Eligible data

Reclamation deletes only **completed runs' logs**, oldest first, in two stages:

1. archived chunks in `minicron-logs.db` (oldest by archive time), skipping every
   run that still has a live writer (a worker's earlier checkpoints stay) or is owned
   by another operation (an archive in progress commits in several batches; a
   checkpoint or deletion), exactly as stage 2 skips owned runs;
2. sealed buffers under `logs/<run>/` of runs with no writer that are waiting
   for archival or retrying it (oldest by modification time).

Archive data is older than sealed buffers by construction, so this is
"oldest first" across tiers without a merged index.

**Never reclaimed:** live buffers; archived chunks of runs with a writer;
`.quarantine/`; SQLite metadata (`minicron.db`: definitions, runs, audit,
queue/pending-execution records, idempotency keys); anything outside the log
tiers. A deleted run keeps its run record; reads of its log return nothing
(the same state `db_keep_for` pruning produces).

### Accounting

`DiskUsage` totals what the log tiers occupy **on disk**:

| Tier | Counted |
|---|---|
| Archive | `minicron-logs.db` file size + `minicron-logs.db-wal` size |
| Sealed | file sizes in `logs/<run>/` of runs without a writer |
| Hot | file sizes in `logs/<run>/` of runs with a writer |

`LogBytes = archive + sealed + hot` is what the byte budget applies to. The
quarantine is measured and reported (`quarantine_bytes`, entries, oldest age) but
is outside the budget: it is never reclaimed by pressure, so counting it would
let a large quarantine delete all history. Disk headroom comes from `statfs` on the
data directory and reflects everything on that filesystem, including data this
daemon does not own.

### Watermarks

| Rule | Starts when | Reclaims down to |
|---|---|---|
| Byte budget `logs.disk_budget` (MiB) | `LogBytes` > 90% of the budget | 80% of the budget |
| Headroom `logs.disk_min_free` (MiB) | free space < the minimum | free ≥ 125% of the minimum |

The 90/80 and 125% marks are constants, not settings: a gap between start and
target makes one pass buy a stretch of quiet instead of deleting a little on
every check. The configured headroom is clamped to a quarter of the filesystem
so a small disk cannot make every pass delete all history.

### Defaults

- `logs.disk_min_free = 512` MiB. On by default: the small-server profile (1 CPU /
  512 MiB RAM, a disk of some GiB) needs room for the archive WAL, compaction and
  the buffers of a few busy runs. `0` disables it; omitted and `0` are different.
- `logs.disk_budget = 0`: no byte budget by default. A byte budget is only
  meaningful relative to a disk the operator knows; the earlier 5 GiB example was
  never an approved default. The headroom rule is the safe default; set a budget
  to share a disk with other services.
- Quarantine: keep forever by default (below).

### When a pass runs (no new always-on timer)

- the existing cadences: the hourly retention tick, the worker-flush tick
  (`logs.worker_flush_interval`), the daily log prune, and once at startup and on
  reload;
- a **coalesced hint** raised by the log store when a chunk rotates, a run seals or
  log capture fails (a full disk is the likeliest cause). The hint is a
  non-blocking send on a one-slot channel (nanoseconds, nothing allocated).
  `diskBudgetLoop` blocks on that channel, so an idle daemon has no wakeup; after
  a pass it waits a 10 s cooldown (a timer that exists only then) so a busy daemon
  runs at most one pass per cooldown.

A pass with no pressure is **one statfs** (when only headroom is configured) and
no directory walk. Only when a watermark may be exceeded does it measure the
tiers (one directory listing plus a stat per file of every run buffer still on
disk, plus the quarantine; archived runs have no files).

### A pass under pressure

1. Measure; compute the bytes needed to reach the low watermarks.
2. Delete the oldest archived chunks of completed runs (batches of at most 256 in
   ctx-aware transactions, so archival and readers interleave), drop runs left empty,
   then `Compact`: incremental vacuum and WAL truncate, because freed pages return
   to the filesystem only then.
3. Re-measure. If still above a watermark, delete sealed buffers oldest first
   (claiming each run so an archive in progress is skipped), then re-measure.
4. Log every deletion at WARN with counts and bytes (never silent) and update
   cumulative counters (`pruned_*`, `sealed_*_deleted`).

### If reclamation is insufficient

The pass ends with `Insufficient = true` (`DiskStatus`, `Store.DiskPressure()`,
`diagnostics.log_storage.disk.insufficient`) and one ERROR log per episode, not per
pass; a later pass that finds the watermarks met logs the relief.

- **Existing processes follow 2A:** their log writes fail, capture degrades, output is
  discarded with drop counters and a `system` line, children keep running.
- **New work (4B) is not wired yet.** `Store.DiskPressure()` is the signal the queue
  policy may consult, but nothing calls it in this implementation: admission is
  unchanged, new runs start normally while pressure is insufficient, and the only
  effect on them is that their log output is dropped under 2A. Refusing or queueing new
  work under disk pressure is left to the queue stream (4B).

Timeouts, cancellation and execution-state persistence are unaffected.

### Failure handling

- `statfs` failure: logged once per episode, exposed as `statfs_error`; headroom is
  skipped (nothing is deleted on a guess); the byte budget keeps working; recovery
  is logged.
- Archive database errors during a pass are joined and logged; the pass still tries
  the sealed-buffer stage.
- A canceled context (shutdown) stops reclamation between batches.

### Quarantine

Preserving damaged logs stays the default, but "forever" is not a budget. Opt-in:

- `logs.quarantine_keep_for` (days, `0` = keep): delete entries older than this;
- `logs.quarantine_max_size` (MiB, `0` = no cap): then delete oldest entries until
  the rest fits.

An entry is a top-level item of `.quarantine/` (a `<run>-<chunk>.zst.corrupt` file or
a `<run>/` directory), aged by modification time. The policy runs at the daily log
prune only, never from disk pressure, and every deletion is logged at WARN with the
reason. Without a policy, `quarantine_bytes`, `quarantine_entries` and
`quarantine_oldest_age_s` are the alert-able metrics.

### A06: sequence-oriented archive cursor

`logdb.EachChunk` selected `WHERE run_id=? AND last_seq>? AND number>? ORDER BY number
LIMIT 1`, which walks the `(run_id, number)` index from the first chunk and filters
`last_seq` row by row, so a cursor read cost grew with the chunks behind the cursor
and full pagination was quadratic. It now selects `WHERE run_id=? AND last_seq>?
ORDER BY last_seq LIMIT 1` and advances the cursor to each chunk's last sequence:
one seek on `idx_log_chunks_seq` per chunk (verified with `EXPLAIN QUERY PLAN` in a
test, which also checks that the old query does not use that index). This relies on
chunk numbers and last sequences growing together within a run, which holds because
both are assigned in write order. On a real disk, 100,000 chunks: final page
9.7 ms → 32 µs, empty tail 9.1 ms → 11 µs, full pagination 58.6 s → 1.24 s (numbers
and caveats in `docs/benchmarks/2026-10-05-a06-archive-cursor/`). Ordering,
retention-gap reporting and final-tail-before-done are unchanged; no reader cache
was added.

## Alternatives considered

- **Archive-only quota, more often** (the simplest option): rejected. It misses the
  WAL, sealed and live buffers and the quarantine, and does nothing for a disk
  shared with other data.
- **A dedicated timer/ticker for pressure**: rejected; AGENTS.md forbids always-on
  timers for zero-viewer cost. Event hints plus existing cadences cover it.
- **A running byte counter updated on every write**: rejected; accounting on the capture
  path costs every line for a rare decision. Walking only when a watermark may be exceeded
  costs nothing otherwise.
- **Merging all tiers into one age-ordered deletion list**: rejected as complexity
  without a benefit; archived data is always older than sealed buffers.
- **Counting the quarantine in the budget or reclaiming it under pressure**: rejected;
  it would turn corrupt-data evidence into disposable space and let it delete healthy
  history.
- **`logs.disk_min_free` as a percent**: rejected for the first version; MiB matches
  the other size settings, and the clamp handles small disks.
- **Disabled by default**: rejected for headroom; an unattended server that fills its
  disk silently loses every run's output (2A) and may stop other services.

## Bounds and ownership

- Work per pass: one statfs; under pressure O(run buffers still on disk) stats plus
  prune batches of 256 chunks, each in its own ctx-aware writer transaction. Passes are
  serialized and cooled down (10 s). No goroutine or timer per run or per writer.
- Lifecycle: `diskBudgetLoop` is owned by the daemon's maintenance group and stops with
  its context; the policy is replaced on reload.
- Diagnostics are computed on demand (`GET /api/v1/daemon`, tier walk cached for 5 s and
  shared by concurrent callers); nothing is precomputed or retained per viewer.

## Accepted downsides

- Shorter history during noisy periods; logs of recent completed runs can be deleted
  before their retention age and before an operator looked at them.
- Headroom pressure caused by *other* data on the filesystem also deletes logs (that is
  what "outranks retention age" means); the 25% clamp limits the damage on small disks
  but a nearly full disk can still delete all eligible history.
- Deleting a run's logs does not mark its run record; the UI shows an empty log.
- WAL space is counted as the disk sees it, so a large WAL (checkpointed but not yet
  truncated) inflates `LogBytes` until the next compaction; a pass that prunes truncates it.
- Reading `statfs` and the directory walk can stall on a failing disk; they run in the
  maintenance goroutine, never on the capture path.

## Measurements and tests

Tests: oldest-first pressure pruning before retention age; only eligible data (active
runs' archive, live buffers, quarantine, metadata) protected; insufficient reclamation
reported once per episode and cleared; byte budget watermarks over all tiers; sealed
stage ordering; statfs failure and recovery; headroom clamp; hint on rotate, seal and
capture failure; loop runs only on hints and respects the cooldown; quarantine age and
size policies; diagnostics fields; A06 query plan, cursor positions and paging across
tier migration. The A06 numbers are in `docs/benchmarks/`.

Not measured: reclamation latency on a large archive over slow storage, and `statfs` cost
on network filesystems. Both run off the capture path.

## Revisit when

- operators need a percent-based headroom or per-definition budgets;
- the queue stream adopts `DiskPressure()` for admission and needs a graded signal;
- measured passes on real archives exceed a few seconds (then bound the sealed stage);
- a hard guarantee that recent runs survive pressure is required (protect-newest-N).
