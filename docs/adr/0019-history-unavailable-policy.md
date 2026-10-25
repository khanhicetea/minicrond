# ADR-19: Per-job choice to skip a run whose row cannot be stored

- Status: accepted (owner decision, 2026-10-25)
- Date: 2026-10-25
- Amends: [ADR-16](0016-execution-first.md) for jobs that opt in
- Related: [ADR-18](0018-relaxed-commits.md)

## Context

Under ADR-16 a job starts even when its `pending` run row cannot be inserted
(database full, read-only, locked or stalled). The row then waits in the
background history writer, in memory.

That row does two things: it is history, and its scheduled-occurrence
uniqueness (`idx_runs_schedule_occurrence`) is what stops one occurrence from
running twice. Losing history is acceptable. Losing the duplicate check is not
always acceptable: if the daemon stops before the deferred row is stored, the
restarted scheduler reads its last fire from the stored rows, which miss that
occurrence, and `catch_up = "latest"` can run it again. For a job that bills,
sends mail or moves money, a duplicate run is worse than a missed one; for a
cleanup or sync job, the reverse holds. The daemon cannot tell which.

Zero-viewer cost: none. The setting is read only on the failure path.

## Decision

New job setting `on_history_unavailable`:

- `run` (default; an omitted value means `run`): ADR-16 behavior.
- `skip`: when the run row cannot be stored, the job does not start. The run
  is handed to the background history writer as `pending` and then
  `skipped` / `history_unavailable`, so it appears in history once the
  database recovers. Trigger returns that skipped run without an error, so
  the scheduler moves on to the next occurrence. An error is logged.

It applies to every trigger of the job (schedule, manual, startup, retry).
Workers reject the setting: they have no occurrences to duplicate and must
keep running (ADR-16). It can be set in `[defaults]`, which then applies to
jobs only.

The default stays `run`: it keeps existing behavior, and an omitted value
leaves stored definitions and their hashes unchanged, so upgrading creates no
new revisions.

A `skip` job therefore only ever runs after its `pending` row was committed
with full durability.

## Alternatives

- **Change the default to `skip`:** safer for side-effecting jobs, but
  reverses ADR-16 for every job and makes a database outage stop all
  schedules. Rejected; the owner chose an opt-in with the existing default.
- **Make all jobs idempotent instead:** the right fix for each job, but out of
  the daemon's control. `skip` is for jobs that cannot be made idempotent.
- **Persist the occurrence somewhere other than SQLite:** a second durable
  store for the same fact. Rejected as more complex.

## Accepted downside

A `skip` job misses its run during a database outage and any `catch_up` will
not replay it (the skipped row, once stored, claims the occurrence). The
first skip can still wait up to SQLite's 5 s busy handler; later ones skip at
once while the outage is known (same path as ADR-16). If the deferred-history
budget (1024 runs) is full, the skipped row itself is not kept (logged and
counted as dropped).

If the insert timed out but did commit, the writer finds that row and moves
it from `pending` to `skipped`.

## Alerts

A `history_unavailable` skip reaches alert channels whose `notify_on` includes
`skipped` (see [configuration](../configuration.md#alerts)). The delivery
record waits for the run row like any other run admitted without one.

## Tests

- `TestSkipJobDoesNotRunWithoutRow` (executor): with the database locked, a
  `skip` job returns `skipped` / `history_unavailable`, its command does not
  run, the row is stored after the lock is released, and the capacity slot is
  free for the next trigger.
- `TestOnHistoryUnavailable` (config): default empty; `run`/`skip` accepted;
  other values and workers rejected; `[defaults]` applies to jobs only and an
  explicit job value wins.

## Revisit

If a durable occurrence record that does not depend on SQLite is introduced.
