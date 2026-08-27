# 06 — Scheduler & Cron Engine

Status: Draft · Seconds-field decision: OQ-5

## Grammar

One parser, one AST, used by validation, scheduling, and UI preview
(described below). Accepted forms:

| Form | Examples | Notes |
|---|---|---|
| 5-field cron | `0 2 * * *`, `*/5 9-17 * * 1-5` | minute hour dom month dow; Vixie semantics |
| Descriptors | `@hourly @daily @weekly @monthly @yearly` | |
| Interval | `@every 90m`, `@every 30s`, `@every 2h30m` | sub-minute **only** via this form |

- `dow`: Sunday = `0` or `7`; steps on ranges are standard.
- Day-of-month vs day-of-week OR semantics (Vixie rule), documented with
  examples in UI help.
- **No 6-field seconds syntax** (OQ-5 recommendation): positional
  5-vs-6-field ambiguity is a classic foot-gun. Sub-minute scheduling exists
  only through unambiguous `@every`. If seconds ever land, it's an explicit
  `seconds = true` key, never a guessed 6th field.
- `schedule` is required for jobs unless `run_on_start`-only (a job with no
  schedule and no `run_on_start` is manual-only — valid, with a UI hint).

## Evaluation model

- Each job computes its **next fire instant** in its own timezone (default
  from `[scheduler] timezone`, default system local, fallback UTC if unknown;
  source tagged in UI). IANA tzdata is bundled into the binary.
- The scheduler sleeps on the earliest next-fire across all jobs (a binary
  heap keyed by instant); config changes rebuild the heap. Evaluation is
  wall-clock aligned for cron forms (`@every` anchors to daemon start, not
  wall grid).
- A fire produces a **trigger request** that passes admission (overlap/queue
  policy, global concurrency, disk-space precheck) before a `pending` run is
  persisted. Admission rejection is recorded as a run with `skipped` /
  `queue_full` end reasons — visible history, not a silent drop.

## Timezone & DST contract ("no missing ticks")

Prime directive, test budget allocated accordingly:

- **Spring forward (gap):** a tick that falls in a nonexistent wall time
  fires once at the moment the gap ends.
- **Fall back (ambiguous hour):** a tick fires on the **first** occurrence of
  the duplicated wall time; the second occurrence is suppressed and recorded
  as a run with end reason `dst_skip` (browsable, so operators can prove
  what happened).
- Per-job timezone is re-evaluated per fire (jobs may change tz on reload);
  catch-up also evaluates in the job's zone.

## Missed runs & catch-up (daemon was down)

On startup, for each job, compute schedule fires between
`last_recorded_fire` and now:

| `catch_up` | Behavior |
|---|---|
| `none` (default, OQ-6) | record one `missed` run summarizing the count; nothing executes (crond parity — quiet) |
| `latest` | execute one run now (with `trigger = schedule`, annotated `caught_up`) |
| `all` | execute each missed fire, capped by `[scheduler] max_catchup` (default 5); beyond the cap, recorded `missed` |

Disabled jobs and workers skip catch-up entirely.

## Jitter

`jitter = "10m"` slips each fire by a uniform random duration in `[0, 10m)` —
for de-synchronizing a fleet of boxes hammering the same API at 02:00.
Jittered runs are marked as such; the run record carries both `scheduled_at`
and `started_at`. v1 admits jittered runs like any other (no daemon-wide
serialization gate; `jitter_gate` reserved in config for a v1.0+ refinement).

## Overlap & concurrency policies (jobs)

When a trigger arrives while another run of the same job is active:

| `on_overlap` | Behavior |
|---|---|
| `parallel` | classic crond: overlap freely |
| `skip` (recommended default — OQ-16) | new trigger → run recorded `skipped` (`overlap_skip`) |
| `queue` | new trigger → `pending` in FIFO queue, cap `max_queued` (default 10); over cap → `skipped` (`queue_full`) |
| `replace` | stop the active run via stop ladder, start the new one; old run → `stopped` (`overlap_replace`) |

Global cap: `[scheduler] max_concurrent_runs` (default 32) applies across all
jobs; over-cap triggers follow the job's queue policy or `queue_full`.

Queue semantics, stated precisely (this class of bug is common in the
category): strict FIFO per job, no overtaking; queued runs are cancelable;
a definition change finalizes queued runs of the removed definition
(`stopped`, `daemon_shutdown`-like marker but `stopped/reload`); a queued run
starts with a `system` log line noting the wait duration.

## Worker boot ordering (supervisor side)

- `priority` (int, default 0): ascending boot; ties by name (deterministic).
- `depends_on`: a worker starts only after listed workers are healthy
  (`healthy_after` reached) **or** exhausted their attempts; an unmet
  dependency never blocks forever — the dependent starts with a warning
  event after dependencies settle. Teardown is reverse order. This is boot
  ordering, not a health-gated DAG — documented as such.

## Observability

- Every job exposes `next_fire_at` (+ humanized text, e.g. "in 4 hours, every
  night at 2:00 AM Asia/Ho_Chi_Minh") via API and UI.
- "Next 5 firings" preview endpoint (uses the same AST — preview can never
  lie about actual behavior).
- Scheduler metrics: fires total, admission rejections by reason, queue
  depth, largest wait time, clock-step events (NTP jumps > 1 s logged).

## Open questions

- OQ-5 confirm no 6-field seconds; OQ-6 confirm `catch_up = "none"` default;
  OQ-16 confirm `on_overlap = "skip"` default.
- Cron year field / `@reboot` alias: we use `run_on_start` (explicit, no
  crond-compatible-but-surprising alias). Confirm.
