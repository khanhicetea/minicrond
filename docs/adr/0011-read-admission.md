# ADR-11: Admission and work budgets for expensive on-demand reads

- Status: accepted; implemented on `feat/reads` (audit A14, observation 5)
- Related: [ADR-8](0008-background-first-trade-offs.md) (decision 5, small-server-first), [audit](../../astra-audit-code.md)

## Context and zero-viewer cost

Almost all runs are unattended, so these reads are rare diagnostics. Their cost
is paid only when someone asks, but it competes with job execution on a small
server (about 1 CPU / 512 MiB): a JSON log page could carry about 16 MiB of
payload plus its encodings, downloads decompressed without limit, and each
metrics request allocated and sorted one sample per in-window run (about 31 ms
and 7 MB at 30,000 rows). Nothing bounded the total, and `WriteTimeout` is not a
handler CPU deadline.

## Decision

JSON log pages, raw downloads and run metrics share one small gate
(`[reads]`: 4 slots, 32 MiB estimated working set, 20 s work timeout). A request
waits up to 0.5 s (at most four waiters per slot) and is then refused with
`503` + `Retry-After` (`read_busy`); an exceeded work deadline is `503
read_timeout`. Both are retryable; the CLI and web app retry them.

- **Whole-download slot hold, capped.** A raw download holds one slot until it
  finishes (a stalled client is cut off by the write deadline) and at most
  `max(1, slots-1)` downloads run at once, so downloads cannot lock out pages
  and metrics. *Rejected:* releasing the slot between pages — under saturation
  a started download would have to abort mid-file or wait unboundedly, and a
  truncated file is worse than a refused request. A failure after the first
  byte aborts the connection so a partial file never looks complete.
- **503 over indefinite queueing.** Unbounded waiting would hold goroutines and
  sockets for exactly the callers that cannot be served; a short bounded wait
  absorbs bursts and the rest is told when to retry. *Rejected:* queue until the
  client times out.
- **1 MiB JSON pages** (was about 16 MiB), via the existing stream page budget.
  Clients already page by sequence. Accepted downside: more requests for a large
  backlog, each separately admitted.
- **Work deadline separate from the write deadline.** Reads run under
  `work_timeout`; the socket write deadline is renewed just before the response
  is written, so queueing and computing do not consume the write budget.
- **Metrics coalescing, 3 s TTL, no background work.** Concurrent requests for
  one view share a computation led by the first request's own goroutine; a
  result is reused for about 3 seconds and dropped by a timer that exists only
  after a computation, so an idle daemon retains nothing. Failures are not
  cached; a canceled leader hands over to a waiter. Aggregation and the sort
  check the request context. *Rejected:* periodically refreshed or incremental
  quantile structures (continuous cost, no measured need).
- **Metadata lookups off the writer connection.** API-facing `Run`,
  `Definition` and `Definitions` reads and retention selection use the read
  pool; WAL makes a commit visible to the next read, and deletes re-check
  eligibility. Execution, supervisor, scheduler and daemon paths keep the writer
  connection, whose consistency they depend on.

## Bounds and overload behavior

At most `slots` expensive reads and `slots × 4` waiters; per-request memory is
about one 1 MiB page (plus encodings) or the metrics sample array, estimated at
4/4/8 MiB against the budget. At the defaults the budget equals slots × the
largest estimate, so slots bind first; the budget matters when lowered. SSE keeps
its separate 64-stream limit. SQLite durability and archive commit-before-unlink
ordering are untouched.

## Accepted downsides

Dashboard metrics can be up to about 3 s stale; diagnostics are refused with a
retryable 503 while the daemon is busy (a fully occupied single-slot
configuration lets one download exclude everything else); cost estimates are
fixed constants, not measured per request; the metrics benchmark gain
(`BenchmarkMetricsEndpointParallel`) comes from coalescing and the TTL, not from
a cheaper computation — `BenchmarkRunMetrics*` are unchanged.

## Revisit when

Operators report refusals during normal diagnostics (raise `slots`, or split
downloads from pages); a measured per-request footprint differs materially from
the estimates; metrics rows per window make a computation exceed `work_timeout`
(consider an incremental or pre-bucketed aggregate); or large-log retrieval
changes with the archive cursor work, which may change page costs.
