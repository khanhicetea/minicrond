# 08 — Storage: SQLite

Status: Draft

## Data directory layout (D-2)

```
/var/lib/minicron/            # root default; ~/.local/share/minicron otherwise (0700)
├── minicron.db               # SQLite database (0600)
├── minicron.db-wal / -shm    # WAL sidecars
├── minicron.lock             # flock single-instance guard
├── minicron.sock             # Unix control socket (0600) [if enabled]
├── daemon.log                 # the daemon's OWN log (never mixes with run logs)
└── logs/                      # run logs, FileSink backend (spec 09)
    └── <job>/<YYYY>/<MM>/<DD>/<run_id>.log[.gz]
```

Run logs are content, not metadata: they live behind the LogSink abstraction
and may be entirely in S3 (S3Sink) while the database stays local. The DB
always fits on a small disk — target < 200 MiB at default retention on a busy
box (10k runs/day).

## SQLite configuration

- `journal_mode = WAL`, `synchronous = NORMAL` (crash-safe with WAL; power-loss
  may lose the last transactions but never corrupts).
- `foreign_keys = ON`, `busy_timeout = 5s`.
- **One writer connection** (all mutations through a dedicated DB actor with
  a bounded queue — the run state machine tolerates a lost final write via
  crash recovery) + a small read pool for API queries.
- `PRAGMA user_version` = schema migration ledger, forward-only, embedded in
  the binary. Opening a DB with a newer schema than the binary = hard error
  naming both versions.
- Auto-`PRAGMA optimize` + periodic `wal_checkpoint(TRUNCATE)` on idle.

## Schema (v0.1 sketch — normative in intent, names may polish)

```sql
CREATE TABLE meta (                 -- singleton KV
  key TEXT PRIMARY KEY,             -- schema_version, instance_id, created_at
  value TEXT NOT NULL
);

CREATE TABLE users (                -- system-mode registration (spec 17);
  id INTEGER PRIMARY KEY,           -- unused in user mode (single admin)
  uid INTEGER NOT NULL UNIQUE,
  username TEXT NOT NULL,
  gid INTEGER NOT NULL,
  home TEXT NOT NULL,
  token_hash TEXT,                  -- user-scoped network token
  registered_at TEXT NOT NULL
);

CREATE TABLE definitions (
  name         TEXT NOT NULL,       -- unique per (owner, name)
  owner_user_id INTEGER,            -- NULL = admin scope (system mode, 17)
  authority    TEXT NOT NULL CHECK (authority IN ('file','db')),
  kind         TEXT NOT NULL CHECK (kind IN ('job','worker')),
  spec         TEXT NOT NULL,       -- canonical JSON — authoritative for db,
                                    -- cached parse for file (spec 05)
  spec_hash    TEXT NOT NULL,       -- sha256(spec), change detection
  source_file  TEXT,                -- managing file when authority = 'file'
  revision     INTEGER NOT NULL,    -- bumps on db-authority changes
  enabled      INTEGER NOT NULL DEFAULT 1,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  deleted_at   TEXT,                -- soft delete; restorable
  PRIMARY KEY (owner_user_id, name)
);

CREATE TABLE definition_revisions ( -- audit trail of specs
  name TEXT NOT NULL, revision INTEGER NOT NULL,
  spec TEXT NOT NULL, spec_hash TEXT NOT NULL,
  actor TEXT NOT NULL,              -- 'file:<path>' | 'ui' | 'api' | 'cli:<cmd>'
  at TEXT NOT NULL,
  PRIMARY KEY (name, revision)
);

CREATE TABLE runs (
  run_id     TEXT PRIMARY KEY,      -- UUIDv7
  job        TEXT NOT NULL,         -- scoped name: owner.name in system mode (17)
  kind       TEXT NOT NULL,         -- job | worker-instance
  instance   INTEGER,               -- worker slot (1-based), else NULL
  revision   INTEGER NOT NULL,      -- definition revision executed
  status     TEXT NOT NULL,         -- pending|running|succeeded|failed|stopped|
                                    -- interrupted|skipped|missed|timeout
  end_reason TEXT,                  -- taxonomy in spec 01
  trigger    TEXT NOT NULL,         -- schedule|startup|manual|retry
  attempt    INTEGER NOT NULL DEFAULT 1,
  parent_run_id TEXT,               -- retry chain
  params     TEXT,                  -- trigger-time params JSON (v0.2)
  run_as     TEXT,
  pid        INTEGER, exit_code INTEGER, signal TEXT,
  queued_at  TEXT, started_at TEXT, ended_at TEXT,
  wait_ms    INTEGER,               -- queue wait
  log_ref    TEXT,                  -- backend-neutral: file:<path> | s3://<bucket>/<prefix>/<run_id>
  log_bytes  INTEGER NOT NULL DEFAULT 0,
  log_truncated INTEGER NOT NULL DEFAULT 0,
  soft_deleted_at TEXT
);
CREATE INDEX idx_runs_job_time ON runs (job, started_at DESC);
CREATE INDEX idx_runs_status   ON runs (status) WHERE status NOT IN
  ('succeeded','failed','stopped','skipped','missed','timeout','interrupted');

CREATE TABLE schedule_state (       -- per job, scheduler bookkeeping
  job TEXT PRIMARY KEY,
  last_fire_at TEXT,                -- last admitted-or-recorded fire
  next_fire_at TEXT,                -- cache; recomputed on boot/reload
  last_missed_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE import_sources (       -- include-file tracking (spec 05)
  path TEXT PRIMARY KEY,
  sha256 TEXT NOT NULL, mtime TEXT NOT NULL,
  imported_at TEXT NOT NULL,
  provides TEXT NOT NULL            -- JSON array of names
);

CREATE TABLE events (               -- notification inbox + event history
  id TEXT PRIMARY KEY,              -- UUIDv7
  kind TEXT NOT NULL,               -- run.failed | worker.fatal | disk.low | ...
  job TEXT, severity TEXT NOT NULL, -- info|warn|error
  payload TEXT NOT NULL,            -- JSON
  coalesce_count INTEGER NOT NULL DEFAULT 1,
  read_at TEXT, created_at TEXT NOT NULL
);

CREATE TABLE audit (                -- config-change audit (spec 05)
  id TEXT PRIMARY KEY, at TEXT NOT NULL,
  actor TEXT NOT NULL, action TEXT NOT NULL,  -- create|update|delete|import|export
  target TEXT NOT NULL,                       -- definition name or 'daemon'
  before TEXT, after TEXT                     -- actor may be user:<name> (17)
);
```

## Retention (sweeper, hourly + on reload)

Per job, most specific wins: `keep_runs` (count) / `keep_for` (age) on the
definition → `[storage] keep_runs_default` / `keep_for_default` → built-in
floor (keep at least the last run of any job for UI sanity). Terminal runs
only — `pending`/`running` are never reaped. Deletion order: DB row soft
delete → purge logs via the run's LogSink (`file:` unlink / `s3:` batch
delete) → hard delete row after 24 h (undo window). Events capped by
`keep_notifications` (default 1000) / 90 d; `definition_revisions` capped by
`[storage] audit_keep` (default 5000) rows; audit likewise.

## Backup / restore

- `minicron backup [--out file]` [v0.2]: `VACUUM INTO` a snapshot DB +
  manifest (binary version, schema version, timestamp, definition count).
  Run logs optionally included for the file backend (`--with-logs` tars the
  logs tree). S3-backed logs already live off-box — the manifest records
  the prefix + run_id list.
- Restore = stop daemon, replace db, start. Documented, tested, boring.
- The DB is a supported interface: users may query `runs` directly (read-only
  guidance in docs; we reserve the right to migrate schema, never to lie).

## Scale expectations & ceiling

Designed-comfortable: 100 definitions, 10k runs/day, 5-year horizon at
default retention. The single-writer actor serializes ~ hundreds of writes/s
— far beyond run-state needs. If a user outgrows it, the answer is "lower
retention", not "get a server DB" (non-goal, aligns with one-machine scope).

## Open questions

- Partition/shard `runs` by month? Recommendation: no v1 (index is enough at
  target scale); revisit at 10M+ rows.
- Store `spec` as canonical JSON vs TOML text: JSON chosen (queryable,
  diffable); the TOML rendering for export is generated, not stored. Confirm.
