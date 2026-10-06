# ADR-12: On-demand process monitoring and kernel exit accounting

- Status: accepted; implemented
- Related: [ADR-8](0008-background-first-trade-offs.md), [ADR-11](0011-read-admission.md)

## Workload and decision

Almost all executions have no viewers. Resource monitoring must not introduce
continuous sampling, process-tree discovery, or time-series writes for those
runs. Add a live `/monitor` page, polling every 3 seconds only while mounted and
visible, and save kernel exit accounting for each job/worker attempt.

Live monitoring reads Linux `/proc/PID/stat` for the daemon and the direct
children owned by the executor. CPU percentages are calculated in the browser
from successive cumulative CPU counters; 100% is one logical CPU. A first
sample cannot give an interval percentage. RSS is current resident memory.
Daemon Go heap-object bytes and goroutine count are also read on demand.
Darwin reports live process stats as unsupported but retains exit accounting.

The executor copies at most 256 identities under its lock; all procfs I/O and
sorting occur outside that lock. The API participates in the existing read gate
with a 1 MiB working-set reservation and **no admission wait**, a 2-second
cooperative work deadline, and one collector at a time. Collection is synchronous
in the request, with no sampler goroutine. Each stat file read is capped at
4 KiB. A bounded snapshot is shared for 3 seconds; one request-triggered expiry
timer releases it even if the last viewer leaves. Concurrent collection or a
full gate returns retryable `503` with `Retry-After`; clients do not build a
retry queue. There is no recurring timer or sampling work without requests.
As with other local filesystem I/O, a syscall stalled by the OS cannot be
forcibly canceled by this deadline; the work/identity caps still apply.

Missing processes, denied/missing procfs, zombies, and changed process-start
identities produce unavailable (`null`) stats, never fabricated zeros or stats
for a reused PID. Active includes execution setup/cleanup; excess active runs
are reported explicitly as a truncated subset. Selection above 256 is not a
stable pagination contract. Diagnostic collection never controls execution.

After `cmd.Wait` returns, read its existing `ProcessState`/`Rusage`: user CPU
microseconds, system CPU microseconds, and peak RSS bytes. Linux Maxrss KiB is
converted to bytes; Darwin Maxrss already uses bytes. These are committed with
the terminal run transition, carried through bounded finalizer retries, and
included in completion callbacks and run history APIs. This adds no extra
SQLite transaction, no child sampling, and no monitoring goroutine. Worker
restarts and job retries each have their own run record and accounting.

Schema 10 adds three nullable counters. Historical, skipped, queued-expired,
unspawned, and crash-interrupted runs have no accounting, not measured zero.
A measured all-zero result is distinct from missing. Accounting is collected
only on normal wait/completion paths; exceptional execution panics may lack it.
Existing metadata SQLite durability and history retention are unchanged.
A daemon crash before the terminal commit cannot reconstruct exit accounting;
startup recovery keeps the existing interrupted-run semantics.

## Alternatives and accepted downsides

- Rejected always-on 3-second sampling and persistent charts: ongoing wakeups,
  procfs reads, and history writes for unattended executions.
- Rejected process-tree scanning: more reader interference and missed short-lived
  descendants despite the extra work.
- Rejected cgroups for now: better whole-job totals but deployment permissions,
  lifecycle management, and more complexity than the owner needs.
- Simpler uncached per-request reads were rejected to bound repeated collection
  from many viewers; the small shared cache owns one transient expiry timer.

Live values intentionally exclude descendants. Exit CPU can include waited-for
descendants but is not guaranteed to cover detached children; peak RSS is not
the simultaneous combined process-tree peak. Live and exit figures therefore
need not agree. No durable live charts or aggregation endpoint is added; saved
counters support later aggregation. Never sum peak RSS and label it a concurrent
memory peak. Display can lag one poll/cache interval and unavailable values are
acceptable; execution control and final-state persistence remain independent.

## Validation and revisit conditions

Tests cover parsing/overflow, PID identity checks, missing stats, first/interval
CPU calculations, reader admission, cancellation, shared-cache expiry, active
worker monitoring, stored accounting for jobs/workers/failures/stops, finalizer
recovery, migration, zero-versus-missing, cloning, and reopen persistence.
The timeout test also asserts stored accounting; existing restart/recovery tests
remain part of full-suite validation.

Validation run for this change: `go test ./...`, `go test -race ./...`,
`go vet ./...`, frontend `npm run check`, `npm test`, and `npm run build`.
A Lightpanda browser functional smoke check covered login/navigation, interval
CPU, polling stopping after navigation and a simulated hidden-page visibility
event, active worker stats, and the stopped worker's exit summary. Chromium was
not installed, so styled visual review was not performed. The feature packages
cross-build for Darwin arm64; the full Darwin CLI cross-build is blocked by the
pre-existing `unix.TCGETS` reference in `cmd/minicrond/crontab.go`.

No numerical overhead claim is made: small-server active capture with no viewers
and occasional readers has not yet been benchmarked for this feature. Revisit
if whole-tree totals, persistent resource charts, stable pagination beyond 256,
or measured reader interference become requirements.
