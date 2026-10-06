# ADR-13: Database-owned alert channels and credentials

## Status

Accepted; implemented. This intentionally breaks file-defined alert channels.

## Context and workload

Jobs/workers already live in the registry, while alert channels formerly lived
in bootstrap TOML. UI/API channel administration should not require file edits,
secret environment setup or daemon restarts. Approximately 99.9999% of execution
is assumed unattended, so channel administration must not add background polling,
per-alert database reads, payload fan-out or synchronous completion-path writes.
This is a management change, not a measured performance optimization.

## Decision and alternatives

SQLite owns channel definitions **and literal credentials**, as explicitly chosen
by the owner. Remove TOML support and env/file credential resolution completely;
do not import existing file channels. Metadata schema 11 creates an empty channel
table for existing databases; it does not migrate any channel configuration.
Old TOML blocks are errors. Before startup, remove stale definition references;
create replacement channels via UI/API, then restore references.

Rejected: retaining file ownership (simpler implementation, but split management),
DB settings plus external secret references (less secret-bearing storage, but
owner wants direct credential entry), and dual file/DB ownership (drift and
precedence complexity). Job/worker secret_env remains external-reference based.

## Ownership, bounds and correctness

- Store at most 100 channels, lowercase names up to 100 characters, tokens/chat
  IDs up to 256 bytes, and batch windows of 1–3600 seconds (default 10).
- Startup loads all channels into memory. Every successful save/delete reloads
  the full set. No periodic polling, new goroutines, per-channel ticker, or
  per-delivery DB lookup is introduced. Existing bounded dispatcher queues,
  drop observations, delivery concurrency and best-effort crash semantics stay.
- Mutation ownership shares the daemon's reload lock with config reload and
  reconciliation. Persist before success; after commit, loading is independent
  of caller cancellation and has a five-second deadline. Post-commit failure
  returns an error rather than claiming live installation; restart reloads DB.
- Queued deliveries retain their original immutable transport/credentials/window.
  Editing or deleting a channel does not retarget or cancel them; a new transport
  instance may flush an old batch sooner when the next alert arrives.
- Definition save/create/import/config-sync validates references inside the
  writer transaction, closing delete-versus-save races.
- Default deletion rejects references. The optional checkbox removes references
  from registry-owned jobs/workers, writes revisions/hashes/audit and deletes the
  channel in one transaction. No partial removal is committed on failure.
  Config-owned definitions remain file-authoritative: edit them first. Affected
  definitions are reconciled with existing scheduling/restart semantics, including
  possible worker replacement. Historical run/alert evidence is retained.
- Channel names are immutable in the editor; API PUT is an upsert. Blank/omitted
  credentials on an existing channel retain the current DB credential. Settings
  changes are last-write-wins, consistent with the simple administration scope.
- Authenticated admins may write credentials, but read/save responses and audit
  entries redact them. They are excluded from job/worker bundle export/import.
  Malformed channel JSON and validation/provider failures must not echo tokens.

## Accepted downsides

The database, WAL, snapshots and backups now contain plaintext credentials.
Existing 0700 directory/0600 database permissions and trusted admin API access
are the protection, **not encryption**. Use TLS for remote administration and
secret-safe backups. Delete/rotation is not secure erasure of old DB/WAL/backup
copies. No encryption key lifecycle or secret-manager subsystem is introduced.

Deployments with file channels must recreate them manually; no compatibility
shim or automatic import is provided. Full reload after a rare configuration
mutation can allocate new transports, but there is no added zero-viewer execution
work. Delivery remains best-effort, not a durable alert queue.

## Evidence and revisiting

Regression coverage includes DB reopen/schema upgrade, credential retention and
redaction, count/field validation, atomic removal and revision history,
config-owned rollback, transactional reference validation and concurrent
save/delete, API authorization and CRUD, daemon restart/full reload/cancellation,
and queued delivery preservation after removal. TypeScript checks and web tests
cover the existing frontend contract; manual browser checks cover administration.
Passed: `go test ./...`, `go test -race ./...`, `go vet ./...`,
`make contracts`, `cd web && npx tsc --noEmit`, `npm test`, `npm run build`.
The rebuilt embedded UI was checked in an isolated browser: create, edit with
blank-token retention, referenced-delete refusal, and checkbox deletion removing
a job reference (revision advanced from 1 to 2). No Telegram delivery to an actual
provider account was attempted.

No performance benchmark, storage-encryption guarantee or power-loss experiment
is claimed. Revisit if many providers/channels warrant incremental reload, if
remote administrators need narrower privileges, if backup exposure calls for
external secret references/encryption, or if durable alert replay is required.
