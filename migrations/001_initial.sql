
BEGIN;
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE definitions (
 definition_id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
 authority TEXT NOT NULL CHECK(authority IN ('file','db')),
 kind TEXT NOT NULL CHECK(kind IN ('job','worker')), spec TEXT NOT NULL,
 spec_hash TEXT NOT NULL, source_file TEXT, revision INTEGER NOT NULL,
 enabled INTEGER NOT NULL, created_us INTEGER NOT NULL, updated_us INTEGER NOT NULL,
 deleted_us INTEGER
);
CREATE TABLE definition_revisions (
 definition_id INTEGER NOT NULL REFERENCES definitions(definition_id), revision INTEGER NOT NULL,
 spec TEXT NOT NULL, spec_hash TEXT NOT NULL, actor TEXT NOT NULL, at_us INTEGER NOT NULL,
 PRIMARY KEY(definition_id, revision)
);
CREATE TABLE runs (
 run_id TEXT PRIMARY KEY, definition_id INTEGER NOT NULL REFERENCES definitions(definition_id),
 job TEXT NOT NULL, kind TEXT NOT NULL, revision INTEGER NOT NULL, definition_hash TEXT NOT NULL,
 status TEXT NOT NULL, end_reason TEXT, trigger TEXT NOT NULL, attempt INTEGER NOT NULL DEFAULT 1,
 scheduled_for_us INTEGER, missed_count INTEGER NOT NULL DEFAULT 0, boot_id TEXT,
 pid INTEGER, pgid INTEGER, process_start_id TEXT, exit_code INTEGER, signal TEXT,
 queued_us INTEGER NOT NULL, started_us INTEGER, ended_us INTEGER, log_ref TEXT,
 log_bytes INTEGER NOT NULL DEFAULT 0, log_truncated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_runs_job_time ON runs(job, queued_us DESC);
CREATE INDEX idx_runs_active ON runs(status) WHERE status IN ('pending','running');
CREATE TABLE schedule_state (
 definition_id INTEGER PRIMARY KEY REFERENCES definitions(definition_id), schedule_hash TEXT NOT NULL,
 anchor_us INTEGER NOT NULL, last_fire_us INTEGER, next_fire_us INTEGER
);
CREATE TABLE import_sources (path TEXT PRIMARY KEY, sha256 TEXT NOT NULL, mtime_us INTEGER NOT NULL, imported_us INTEGER NOT NULL, provides TEXT NOT NULL);
CREATE TABLE audit (id INTEGER PRIMARY KEY, at_us INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL, before TEXT, after TEXT);
CREATE TABLE idempotency (principal TEXT NOT NULL, operation TEXT NOT NULL, key TEXT NOT NULL, request_hash TEXT NOT NULL, run_id TEXT NOT NULL, created_us INTEGER NOT NULL, PRIMARY KEY(principal,operation,key));

COMMIT;
