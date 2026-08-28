# v0.1 normative acceptance tables

## Runs

| From | Allowed next states |
|---|---|
| pending | running, failed(start_error), stopped, interrupted, skipped, missed |
| running | succeeded, failed, timeout, stopped, interrupted |
| terminal | none |

A timeout remains `timeout` even if termination yields exit code 0. An operator stop remains `stopped`. Recovery marks unobservable active runs `interrupted`.

## Reload and authority

| Case | Result |
|---|---|
| all bootstrap/includes valid | one DB transaction, then reconciliation |
| any file invalid or duplicate | desired set unchanged |
| file edits file-authority definition | revision increments |
| file collides with DB authority | whole reload rejected |
| linked source absent | retain disabled (or delete with `prune_missing`) |
| API writes file authority | 409 `authority_conflict` |

## Scheduling/workers

`@every` keeps a DB anchor. Schedule hashes reset catch-up history. `none` records one summarized missed run; `latest` admits one. `skip` records overlap decline. Job capacity is independent from workers. A worker operator hold suppresses `always` restart.

## Logs

Frames contain version, stream, flags, stable sequence, Unix-microsecond timestamp, uint32 payload length, and original bytes. Sequence is daemon-ingestion order. Partial final lines, invalid UTF-8, and truncation are flagged. SSE subscribes before backlog reads and deduplicates by sequence.

Pipe lifecycle: stdout/stderr use caller-owned pipes; the normal path drains to EOF before close, a descendant holding an inherited pipe is bounded by a 5-second post-exit drain and then force-closed with a system annotation — never a lost tail, a hang, or a spurious pump error.

## Idempotency

`Idempotency-Key` on trigger is scoped by principal; the stored request hash binds the target. Replay of the same key+target returns the original run (200); reuse with a different target is 409.

## Retention

Only terminal runs are candidates. Keep the newest terminal run. Delete when either count or age is exceeded. Delete logs first; retain DB metadata when log deletion fails.
