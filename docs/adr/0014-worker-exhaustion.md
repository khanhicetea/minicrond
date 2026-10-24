# ADR-14: durable worker exhaustion outcomes

## Context and choice

Unattended workers must not silently disappear when their crash budget runs out.
A warning alone is not operator state. Persist one small exhaustion record per
definition in the existing metadata database (`meta`), and query it on demand.
The record includes reason (bounded to 512 bytes), attempts, revision, timestamp,
and the last run ID if admission created a run. It is not a fabricated execution
run: run history, run status, retention and metrics retain their existing meaning.

We rejected a memory-only fatal flag (lost on process death) and synthetic failed
runs (pollute history/metrics and cannot reliably outlive run retention). No new
storage engine, migration, polling service or always-on dashboard work is needed.

## Ordering, resources and failure behavior

The supervisor's existing worker loop commits exhaustion before sending an alert.
A dedicated write/reset mutex and reset epochs prevent an old persistence retry
from reinstating state after an operator reset. Database attempts have a two-second
context budget; retries use capped 1–30 second backoff in that same worker loop.
Process control and shutdown never acquire the state-write mutex. No execution
slot or supervisor mutex is held over storage I/O or backoff. Readers use the read
pool and never acquire the state-write mutex. Outcomes are small and bounded by
the number of definition identities, not the number of failed runs.

Pending persistence is explicitly visible. A start/restart or reload must commit
the clear before allowing replacement work; storage failure is returned to the
caller. Changed/deleted revisions cannot write stale outcomes. Startup reload
also clears exhaustion, intentionally granting a fresh budget. If storage remains
unavailable until forced shutdown, a pending (explicitly non-durable) outcome
cannot survive that shutdown; already committed outcomes remain queryable.

Alerts reuse bounded dispatch, batching, overload shedding and observation work.
Exhaustion uses a transport-only `exhausted` status and a distinct delivery key
`<channel>~worker_exhausted`; execution statuses are unchanged. Admission failures
without a run can still alert, but cannot have run-FK delivery observations.
Delivery is best-effort, not a durable outbox; a crash between persistence and
notification may lose that notification, not the exhaustion evidence.

## Accepted downside and validation

Start/reload gain a metadata write; an exhausted worker may retain its supervisor
loop while storage is down. These rare costs buy recoverable evidence without
per-run writes beyond the existing terminal transition or zero-viewer timers.
Tests cover persistence/reopen, revision guards, reset ordering, cancellation,
query surfaces, alert formatting/batching, and UI labels. Revisit an alert outbox
only if operators require guaranteed delivery rather than queryable evidence.
No performance improvement is claimed; no power-loss experiment was performed.

## Addendum (2026-10-24): opt-in restart backoff

Owner choice: keep retirement as the default and add an opt-in instead of
giving up on a container's main service. With `restart_backoff_max` (seconds)
set, a worker is never retired and no exhaustion record is written; each
consecutive unhealthy exit doubles `restart_delay` up to the cap, a healthy
run resets it, and reaching `max_restart_attempts` only logs a `crash
looping` warning once per episode. The cost is one timer per waiting worker,
as before; nothing new is persisted. Worker start/stop/restart are now
accepted for config-owned workers (runtime control, not edits), so an
exhausted config-owned worker can be revived without a reload.
