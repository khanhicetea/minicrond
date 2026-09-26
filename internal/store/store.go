package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/model"
)

const SchemaVersion = 7

// Sentinel errors used by callers to map storage failures onto API statuses.
var ErrRevisionConflict = errors.New("revision conflict")
var ErrReadOnly = errors.New("config-owned definition is read-only")

type Store struct{ db *sql.DB }

type RunMetrics struct {
	Total         int               `json:"total"`
	Succeeded     int               `json:"succeeded"`
	Failed        int               `json:"failed"`
	Active        int               `json:"active"`
	Queued        int               `json:"queued"`
	DurationP50MS *int64            `json:"duration_p50_ms,omitempty"`
	DurationP95MS *int64            `json:"duration_p95_ms,omitempty"`
	Jobs          []RunJobMetrics   `json:"jobs"`
	Buckets       []RunMetricBucket `json:"buckets"`
}

type RunJobMetrics struct {
	Name          string `json:"name"`
	Total         int    `json:"total"`
	Succeeded     int    `json:"succeeded"`
	Failed        int    `json:"failed"`
	Active        int    `json:"active"`
	DurationP50MS *int64 `json:"duration_p50_ms,omitempty"`
	DurationP95MS *int64 `json:"duration_p95_ms,omitempty"`
}

type RunMetricBucket struct {
	Success       int    `json:"success"`
	Failure       int    `json:"failure"`
	Active        int    `json:"active"`
	Queued        int    `json:"queued"`
	DurationP50MS *int64 `json:"duration_p50_ms,omitempty"`
	DurationP95MS *int64 `json:"duration_p95_ms,omitempty"`
}

func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "minicron.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	for _, dbPath := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(dbPath, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	return version, err
}
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&value)
	return value, err
}
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) migrate(ctx context.Context) error {
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return fmt.Errorf("database schema %d is newer than supported schema %d", version, SchemaVersion)
	}
	if version != 0 && version < 2 {
		return fmt.Errorf("database schema %d is incompatible with schema %d; remove the development database", version, SchemaVersion)
	}
	if version == 0 {
		if _, err := s.db.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
		return nil
	}
	if version == 2 {
		if _, err := s.db.ExecContext(ctx, migration3); err != nil {
			return fmt.Errorf("migration 3: %w", err)
		}
	}
	if version <= 3 {
		if _, err := s.db.ExecContext(ctx, migration4); err != nil {
			return fmt.Errorf("migration 4: %w", err)
		}
	}
	if version <= 4 {
		if _, err := s.db.ExecContext(ctx, migration5); err != nil {
			return fmt.Errorf("migration 5: %w", err)
		}
	}
	if version <= 5 {
		if _, err := s.db.ExecContext(ctx, migration6); err != nil {
			return fmt.Errorf("migration 6: %w", err)
		}
	}
	if version <= 6 {
		if _, err := s.db.ExecContext(ctx, migration7); err != nil {
			return fmt.Errorf("migration 7: %w", err)
		}
	}
	return nil
}

const schema = `
BEGIN;
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE definitions (
 definition_id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
 kind TEXT NOT NULL CHECK(kind IN ('job','worker')), spec TEXT NOT NULL,
 spec_hash TEXT NOT NULL, revision INTEGER NOT NULL,
 enabled INTEGER NOT NULL, source TEXT NOT NULL DEFAULT '', created_us INTEGER NOT NULL, updated_us INTEGER NOT NULL,
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
 parent_run_id TEXT,
 scheduled_for_us INTEGER, missed_count INTEGER NOT NULL DEFAULT 0, boot_id TEXT,
 pid INTEGER, pgid INTEGER, process_start_id TEXT, exit_code INTEGER, signal TEXT,
 queued_us INTEGER NOT NULL, started_us INTEGER, ended_us INTEGER, log_ref TEXT,
 log_bytes INTEGER NOT NULL DEFAULT 0, log_truncated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_runs_job_time ON runs(job, queued_us DESC);
CREATE INDEX idx_runs_time ON runs(queued_us DESC);
CREATE INDEX idx_runs_definition_terminal ON runs(definition_id, ended_us DESC);
CREATE INDEX idx_runs_retention ON runs(definition_id, COALESCE(ended_us,queued_us) DESC, run_id DESC) WHERE status IN ('succeeded','failed','timeout','stopped','interrupted','skipped','missed');
CREATE INDEX idx_runs_active ON runs(status) WHERE status IN ('pending','running');
CREATE UNIQUE INDEX idx_runs_schedule_occurrence ON runs(definition_id, scheduled_for_us) WHERE trigger='schedule' AND scheduled_for_us IS NOT NULL;
CREATE TABLE schedule_state (
 definition_id INTEGER PRIMARY KEY REFERENCES definitions(definition_id), schedule_hash TEXT NOT NULL,
 anchor_us INTEGER NOT NULL, last_fire_us INTEGER, next_fire_us INTEGER
);
CREATE TABLE audit (id INTEGER PRIMARY KEY, at_us INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL, before TEXT, after TEXT);
CREATE TABLE idempotency (principal TEXT NOT NULL, operation TEXT NOT NULL, key TEXT NOT NULL, request_hash TEXT NOT NULL, run_id TEXT NOT NULL, created_us INTEGER NOT NULL, PRIMARY KEY(principal,operation,key));
CREATE INDEX idx_idempotency_run_time ON idempotency(run_id,created_us);
CREATE TABLE alert_deliveries (run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE, channel TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT, updated_us INTEGER NOT NULL, PRIMARY KEY(run_id,channel));
CREATE INDEX idx_alert_deliveries_status ON alert_deliveries(status,updated_us);
PRAGMA user_version=7;
COMMIT;`

const migration3 = `
BEGIN;
CREATE INDEX IF NOT EXISTS idx_runs_time ON runs(queued_us DESC);
CREATE INDEX IF NOT EXISTS idx_runs_definition_terminal ON runs(definition_id, ended_us DESC);
DELETE FROM runs WHERE trigger='schedule' AND scheduled_for_us IS NOT NULL AND rowid NOT IN (
 SELECT MIN(rowid) FROM runs WHERE trigger='schedule' AND scheduled_for_us IS NOT NULL GROUP BY definition_id,scheduled_for_us
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_schedule_occurrence ON runs(definition_id, scheduled_for_us) WHERE trigger='schedule' AND scheduled_for_us IS NOT NULL;
PRAGMA user_version=3;
COMMIT;`

const migration4 = `
BEGIN;
CREATE TABLE alert_deliveries (run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE, channel TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT, updated_us INTEGER NOT NULL, PRIMARY KEY(run_id,channel));
CREATE INDEX idx_alert_deliveries_status ON alert_deliveries(status,updated_us);
PRAGMA user_version=4;
COMMIT;`

const migration5 = `
BEGIN;
ALTER TABLE runs ADD COLUMN parent_run_id TEXT;
PRAGMA user_version=5;
COMMIT;`

const migration6 = `
BEGIN;
ALTER TABLE definitions ADD COLUMN source TEXT NOT NULL DEFAULT '';
PRAGMA user_version=6;
COMMIT;`

const migration7 = `
BEGIN;
CREATE INDEX IF NOT EXISTS idx_runs_retention ON runs(definition_id, COALESCE(ended_us,queued_us) DESC, run_id DESC) WHERE status IN ('succeeded','failed','timeout','stopped','interrupted','skipped','missed');
CREATE INDEX IF NOT EXISTS idx_idempotency_run_time ON idempotency(run_id,created_us);
PRAGMA user_version=7;
COMMIT;`

func (s *Store) Definitions(ctx context.Context) ([]model.Definition, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT definition_id,spec,revision,enabled,source FROM definitions WHERE deleted_us IS NULL ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Definition
	for rows.Next() {
		var d model.Definition
		var raw string
		var source string
		var enabled bool
		if err := rows.Scan(&d.ID, &raw, &d.Revision, &enabled, &source); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		d.Source = source
		d.Enabled = &enabled
		out = append(out, d)
	}
	return out, rows.Err()
}

// RetentionDefinitions includes soft-deleted definitions so their run history
// continues to receive the configured retention policy.
func (s *Store) RetentionDefinitions(ctx context.Context) ([]model.Definition, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT definition_id,spec,revision,enabled FROM definitions ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Definition
	for rows.Next() {
		var d model.Definition
		var raw string
		var enabled bool
		if err := rows.Scan(&d.ID, &raw, &d.Revision, &enabled); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		d.Enabled = &enabled
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) Definition(ctx context.Context, name string) (model.Definition, string, error) {
	var d model.Definition
	var raw, hash string
	var source string
	var enabled bool
	err := s.db.QueryRowContext(ctx, "SELECT definition_id,spec,spec_hash,revision,enabled,source FROM definitions WHERE name=? AND deleted_us IS NULL", name).Scan(&d.ID, &raw, &hash, &d.Revision, &enabled, &source)
	if err != nil {
		return d, "", err
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return d, "", err
	}
	d.Source = source
	d.Enabled = &enabled
	return d, hash, nil
}

func (s *Store) ImportDefinitions(ctx context.Context, defs []model.Definition, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMicro()
	for _, d := range defs {
		d.Source = ""
		b, hash, err := config.Canonical(d)
		if err != nil {
			return err
		}
		var id, rev int64
		var before string
		var deleted sql.NullInt64
		var source string
		err = tx.QueryRowContext(ctx, "SELECT definition_id,revision,spec,deleted_us,source FROM definitions WHERE name=?", d.Name).Scan(&id, &rev, &before, &deleted, &source)
		if err == nil && source == "config" {
			return ErrReadOnly
		}
		if errors.Is(err, sql.ErrNoRows) {
			res, execErr := tx.ExecContext(ctx, `INSERT INTO definitions(name,kind,spec,spec_hash,revision,enabled,created_us,updated_us) VALUES(?,?,?,?,1,?,?,?)`, d.Name, d.Kind, string(b), hash, d.IsEnabled(), now, now)
			if execErr != nil {
				return execErr
			}
			id, _ = res.LastInsertId()
			rev = 1
		} else if err != nil {
			return err
		} else {
			rev++
			if _, err = tx.ExecContext(ctx, "UPDATE definitions SET kind=?,spec=?,spec_hash=?,revision=?,enabled=?,updated_us=?,deleted_us=NULL WHERE definition_id=?", d.Kind, string(b), hash, rev, d.IsEnabled(), now, id); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO definition_revisions(definition_id,revision,spec,spec_hash,actor,at_us) VALUES(?,?,?,?,?,?)", id, rev, string(b), hash, actor, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target,before,after) VALUES(?,?,?,?,?,?)", now, actor, "import", d.Name, before, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SyncConfigDefinitions atomically mirrors the main config into the registry.
// Names already owned by an API definition are never taken over.
func (s *Store) SyncConfigDefinitions(ctx context.Context, defs []model.Definition) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMicro()
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		seen[d.Name] = true
		d.Source = "config"
		b, hash, err := config.Canonical(d)
		if err != nil {
			return err
		}
		var id, rev int64
		var before, oldHash, source string
		var deleted sql.NullInt64
		err = tx.QueryRowContext(ctx, "SELECT definition_id,revision,spec,spec_hash,source,deleted_us FROM definitions WHERE name=?", d.Name).Scan(&id, &rev, &before, &oldHash, &source, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			res, err := tx.ExecContext(ctx, `INSERT INTO definitions(name,kind,spec,spec_hash,revision,enabled,source,created_us,updated_us) VALUES(?,?,?,?,1,?,?,?,?)`, d.Name, d.Kind, string(b), hash, d.IsEnabled(), "config", now, now)
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
			rev = 1
		} else if err != nil {
			return err
		} else {
			if source != "config" {
				return fmt.Errorf("config definition %q conflicts with registry definition", d.Name)
			}
			if oldHash == hash && !deleted.Valid {
				continue
			}
			rev++
			if _, err := tx.ExecContext(ctx, "UPDATE definitions SET kind=?,spec=?,spec_hash=?,revision=?,enabled=?,updated_us=?,deleted_us=NULL WHERE definition_id=?", d.Kind, string(b), hash, rev, d.IsEnabled(), now, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO definition_revisions(definition_id,revision,spec,spec_hash,actor,at_us) VALUES(?,?,?,?,?,?)", id, rev, string(b), hash, "config", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target,before,after) VALUES(?,?,?,?,?,?)", now, "config", "sync", d.Name, before, string(b)); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT name FROM definitions WHERE source='config' AND deleted_us IS NULL")
	if err != nil {
		return err
	}
	var removed []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if !seen[name] {
			removed = append(removed, name)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, name := range removed {
		if _, err := tx.ExecContext(ctx, "UPDATE definitions SET deleted_us=?,enabled=0,updated_us=? WHERE name=?", now, now, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target) VALUES(?,?,?,?)", now, "config", "remove", name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteDefinition(ctx context.Context, name, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var before string
	var source string
	if err = tx.QueryRowContext(ctx, "SELECT spec,source FROM definitions WHERE name=? AND deleted_us IS NULL", name).Scan(&before, &source); err != nil {
		return err
	}
	if source == "config" {
		return ErrReadOnly
	}
	now := time.Now().UnixMicro()
	if _, err = tx.ExecContext(ctx, "UPDATE definitions SET deleted_us=?,enabled=0,updated_us=? WHERE name=?", now, now, name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target,before) VALUES(?,?,?,?,?)", now, actor, "delete", name, before); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetEnabled(ctx context.Context, name string, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, revision int64
	var spec, hash string
	var source string
	if err = tx.QueryRowContext(ctx, "SELECT definition_id,revision,spec,spec_hash,source FROM definitions WHERE name=? AND deleted_us IS NULL", name).Scan(&id, &revision, &spec, &hash, &source); err != nil {
		return err
	}
	if source == "config" {
		return ErrReadOnly
	}
	now := time.Now().UnixMicro()
	revision++
	if _, err = tx.ExecContext(ctx, "UPDATE definitions SET enabled=?,revision=?,updated_us=? WHERE definition_id=?", enabled, revision, now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO definition_revisions(definition_id,revision,spec,spec_hash,actor,at_us) VALUES(?,?,?,?,?,?)", id, revision, spec, hash, "api", now); err != nil {
		return err
	}
	after := fmt.Sprintf(`{"enabled":%t}`, enabled)
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target,before,after) VALUES(?,?,?,?,?,?)", now, "api", "set_enabled", name, nil, after); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutDefinition(ctx context.Context, d model.Definition, expected int64, actor string) (model.Definition, error) {
	return s.putDefinition(ctx, d, expected, actor, false)
}

// CreateDefinition claims a name only if it has never been used, including by
// a deleted definition. It gives automation an atomic collision check.
func (s *Store) CreateDefinition(ctx context.Context, d model.Definition, actor string) (model.Definition, error) {
	return s.putDefinition(ctx, d, 0, actor, true)
}

func (s *Store) putDefinition(ctx context.Context, d model.Definition, expected int64, actor string, createOnly bool) (model.Definition, error) {
	d.Source = ""
	b, hash, err := config.Canonical(d)
	if err != nil {
		return d, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMicro()
	var id, rev int64
	var before string
	var deleted sql.NullInt64
	var source string
	err = tx.QueryRowContext(ctx, "SELECT definition_id,revision,spec,deleted_us,source FROM definitions WHERE name=?", d.Name).Scan(&id, &rev, &before, &deleted, &source)
	if err == nil && source == "config" {
		return d, ErrReadOnly
	}
	if errors.Is(err, sql.ErrNoRows) {
		res, e := tx.ExecContext(ctx, `INSERT INTO definitions(name,kind,spec,spec_hash,revision,enabled,created_us,updated_us) VALUES(?,?,?,?,1,?,?,?)`, d.Name, d.Kind, string(b), hash, d.IsEnabled(), now, now)
		if e != nil {
			return d, e
		}
		id, _ = res.LastInsertId()
		rev = 1
	} else if err != nil {
		return d, err
	} else {
		if createOnly {
			return d, fmt.Errorf("%w: definition name %q already exists", ErrRevisionConflict, d.Name)
		}
		if !deleted.Valid && expected != 0 && expected != rev {
			return d, fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, expected, rev)
		}
		if deleted.Valid && expected != 0 {
			return d, fmt.Errorf("%w: definition was deleted", ErrRevisionConflict)
		}
		rev++
		if _, err = tx.ExecContext(ctx, "UPDATE definitions SET kind=?,spec=?,spec_hash=?,revision=?,enabled=?,updated_us=?,deleted_us=NULL WHERE definition_id=?", d.Kind, string(b), hash, rev, d.IsEnabled(), now, id); err != nil {
			return d, err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO definition_revisions(definition_id,revision,spec,spec_hash,actor,at_us) VALUES(?,?,?,?,?,?)", id, rev, string(b), hash, actor, now); err != nil {
		return d, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target,before,after) VALUES(?,?,?,?,?,?)", now, actor, "update", d.Name, before, string(b)); err != nil {
		return d, err
	}
	if err = tx.Commit(); err != nil {
		return d, err
	}
	d.ID = id
	d.Revision = rev
	return d, nil
}

func (s *Store) IdempotentRun(ctx context.Context, principal, operation, key, requestHash string) (string, error) {
	var runID, existingHash string
	err := s.db.QueryRowContext(ctx, "SELECT run_id,request_hash FROM idempotency WHERE principal=? AND operation=? AND key=? AND created_us>?", principal, operation, key, time.Now().Add(-24*time.Hour).UnixMicro()).Scan(&runID, &existingHash)
	if err != nil {
		return "", err
	}
	if existingHash != requestHash {
		return "", errors.New("idempotency key reused with different request")
	}
	return runID, nil
}
func (s *Store) SaveIdempotency(ctx context.Context, principal, operation, key, requestHash, runID string) error {
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO idempotency(principal,operation,key,request_hash,run_id,created_us) VALUES(?,?,?,?,?,?)
		ON CONFLICT(principal,operation,key) DO UPDATE SET request_hash=excluded.request_hash,run_id=excluded.run_id,created_us=excluded.created_us
		WHERE idempotency.created_us<=?`, principal, operation, key, requestHash, runID, now.UnixMicro(), now.Add(-24*time.Hour).UnixMicro())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("active idempotency key already exists")
	}
	return nil
}

func (s *Store) CreateRun(ctx context.Context, r model.Run) error {
	_, err := s.db.ExecContext(ctx, createRunSQL, runArgs(r)...)
	return err
}

const createRunSQL = `INSERT INTO runs(run_id,definition_id,job,kind,revision,definition_hash,status,end_reason,trigger,attempt,parent_run_id,scheduled_for_us,missed_count,boot_id,queued_us,ended_us,log_ref) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

func runArgs(r model.Run) []any {
	return []any{r.ID, r.DefinitionID, r.Job, r.Kind, r.Revision, r.DefinitionHash, r.Status, nullString(r.EndReason), r.Trigger, r.Attempt, nullString(r.ParentRunID), timePtrUS(r.ScheduledFor), r.MissedCount, r.BootID, r.QueuedAt.UnixMicro(), timePtrUS(r.EndedAt), r.LogRef}
}

// AdmitIdempotentRun atomically reserves/reuses a key and creates its pending
// run. The returned replay ID is non-empty when a still-live key already won.
func (s *Store) AdmitIdempotentRun(ctx context.Context, r model.Run, principal, operation, key, requestHash string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := time.Now()
	var existingID, existingHash string
	var created int64
	err = tx.QueryRowContext(ctx, "SELECT run_id,request_hash,created_us FROM idempotency WHERE principal=? AND operation=? AND key=?", principal, operation, key).Scan(&existingID, &existingHash, &created)
	if err == nil && created > now.Add(-24*time.Hour).UnixMicro() {
		if existingHash != requestHash {
			return "", errors.New("idempotency key reused with different request")
		}
		return existingID, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err == nil {
		if _, err = tx.ExecContext(ctx, "DELETE FROM idempotency WHERE principal=? AND operation=? AND key=?", principal, operation, key); err != nil {
			return "", err
		}
	}
	if _, err = tx.ExecContext(ctx, createRunSQL, runArgs(r)...); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO idempotency(principal,operation,key,request_hash,run_id,created_us) VALUES(?,?,?,?,?,?)", principal, operation, key, requestHash, r.ID, now.UnixMicro()); err != nil {
		return "", err
	}
	return "", tx.Commit()
}
func (s *Store) StartRun(ctx context.Context, id string, pid, pgid int, startID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE runs SET status='running',pid=?,pgid=?,process_start_id=?,started_us=? WHERE run_id=? AND status='pending'", pid, pgid, startID, at.UnixMicro(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("start run %s: invalid state transition", id)
	}
	return nil
}
func (s *Store) FinishRun(ctx context.Context, id, status, reason string, code *int, signal string, at time.Time, bytes int64, truncated bool) error {
	res, err := s.db.ExecContext(ctx, "UPDATE runs SET status=?,end_reason=?,exit_code=?,signal=?,ended_us=?,log_bytes=?,log_truncated=? WHERE run_id=? AND (status IN ('pending','running') OR status=?)", status, reason, code, nullString(signal), at.UnixMicro(), bytes, truncated, id, status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("finish run %s: invalid state transition", id)
	}
	return nil
}
func (s *Store) Recoverable(ctx context.Context) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,COALESCE(boot_id,''),COALESCE(pid,0),COALESCE(pgid,0),COALESCE(process_start_id,'') FROM runs WHERE status IN ('pending','running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Run
	for rows.Next() {
		var r model.Run
		if err := rows.Scan(&r.ID, &r.BootID, &r.PID, &r.PGID, &r.ProcessStartID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) Recover(ctx context.Context) error {
	now := time.Now().UnixMicro()
	_, err := s.db.ExecContext(ctx, "UPDATE runs SET status='interrupted',end_reason='crash_recovery',ended_us=? WHERE status IN ('pending','running')", now)
	return err
}
func (s *Store) Runs(ctx context.Context, job string, limit int) ([]model.Run, error) {
	return s.RunsPage(ctx, job, min(max(limit, 1), 500), "", "")
}

// RunsPage returns runs in a stable newest-first order. before is the run ID
// at the end of the previous page, so new runs cannot shift later pages.
func (s *Store) RunsPage(ctx context.Context, job string, limit int, before, filter string) ([]model.Run, error) {
	limit = min(max(limit, 1), 501)
	q := `SELECT run_id,definition_id,job,kind,revision,definition_hash,status,COALESCE(end_reason,''),trigger,attempt,COALESCE(parent_run_id,''),scheduled_for_us,missed_count,COALESCE(boot_id,''),COALESCE(pid,0),COALESCE(pgid,0),COALESCE(process_start_id,''),exit_code,COALESCE(signal,''),queued_us,started_us,ended_us,COALESCE(log_ref,''),log_bytes,log_truncated FROM runs`
	var args []any
	var conditions []string
	if job != "" {
		conditions = append(conditions, "job=?")
		args = append(args, job)
	}
	switch filter {
	case "failed":
		conditions = append(conditions, "status IN ('failed','timeout','interrupted')")
	case "scheduled":
		conditions = append(conditions, "trigger='schedule'")
	case "manual":
		conditions = append(conditions, "trigger='manual'")
	case "active":
		conditions = append(conditions, "status IN ('pending','running')")
	}
	if before != "" {
		var queued int64
		if err := s.db.QueryRowContext(ctx, "SELECT queued_us FROM runs WHERE run_id=?", before).Scan(&queued); err != nil {
			return nil, err
		}
		conditions = append(conditions, "(queued_us < ? OR (queued_us = ? AND run_id < ?))")
		args = append(args, queued, queued, before)
	}
	if len(conditions) > 0 {
		q += " WHERE " + strings.Join(conditions, " AND ")
	}
	q += " ORDER BY queued_us DESC, run_id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Run, 0)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) RunMetrics(ctx context.Context, since, now time.Time, buckets int) (RunMetrics, error) {
	buckets = min(max(buckets, 1), 288)
	out := RunMetrics{Buckets: make([]RunMetricBucket, buckets)}
	jobDurations := make(map[string][]int64)
	bucketDurations := make([][]int64, buckets)
	queuedDiff := make([]int, buckets+1)
	activeDiff := make([]int, buckets+1)
	jobStats := make(map[string]*RunJobMetrics)
	var durations []int64
	startUS, endUS := since.UnixMicro(), now.UnixMicro()
	windowUS := max(endUS-startUS, 1)
	bucketIndex := func(us int64) int {
		return min(buckets-1, max(0, int((us-startUS)*int64(buckets)/windowUS)))
	}
	bucketSample := func(i int) int64 {
		return startUS + (int64(i)*windowUS)/int64(buckets) + windowUS/int64(buckets)/2
	}
	addInterval := func(diff []int, from, until int64) {
		first := sort.Search(buckets, func(i int) bool { return bucketSample(i) >= from })
		last := sort.Search(buckets, func(i int) bool { return bucketSample(i) >= until })
		if first < last {
			diff[first]++
			diff[last]--
		}
	}
	isFailed := func(status string) bool {
		return status == "failed" || status == "timeout" || status == "interrupted"
	}
	isActive := func(status string) bool { return status == "pending" || status == "running" }
	jobEntry := func(name string) *RunJobMetrics {
		entry := jobStats[name]
		if entry == nil {
			entry = &RunJobMetrics{Name: name}
			jobStats[name] = entry
		}
		return entry
	}

	rows, err := s.db.QueryContext(ctx, `SELECT job,status,queued_us,started_us,ended_us FROM runs WHERE queued_us>=? OR ended_us>=? OR status IN ('pending','running')`, startUS, startUS)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var job, status string
		var queuedUS int64
		var started, ended sql.NullInt64
		if err := rows.Scan(&job, &status, &queuedUS, &started, &ended); err != nil {
			return out, err
		}
		queuedInWindow := queuedUS >= startUS && queuedUS <= endUS
		if queuedInWindow {
			out.Total++
			entry := jobEntry(job)
			entry.Total++
			if status == "succeeded" {
				out.Succeeded++
				entry.Succeeded++
			} else if isFailed(status) {
				out.Failed++
				entry.Failed++
			}
			if started.Valid && ended.Valid && ended.Int64 >= started.Int64 {
				duration := (ended.Int64 - started.Int64) / 1000
				durations = append(durations, duration)
				jobDurations[job] = append(jobDurations[job], duration)
			}
		}
		if isActive(status) {
			entry := jobEntry(job)
			entry.Active++
			if status == "pending" {
				out.Queued++
			} else {
				out.Active++
			}
		}
		if ended.Valid && ended.Int64 >= startUS && ended.Int64 <= endUS {
			index := bucketIndex(ended.Int64)
			if status == "succeeded" {
				out.Buckets[index].Success++
			} else if isFailed(status) {
				out.Buckets[index].Failure++
			}
			if started.Valid && ended.Int64 >= started.Int64 {
				bucketDurations[index] = append(bucketDurations[index], (ended.Int64-started.Int64)/1000)
			}
		}
		endedUS := endUS + 1
		if ended.Valid && ended.Int64 < endedUS {
			endedUS = ended.Int64
		}
		queuedUntil := endedUS
		if started.Valid && started.Int64 < queuedUntil {
			queuedUntil = started.Int64
		}
		addInterval(queuedDiff, queuedUS, queuedUntil)
		if started.Valid {
			addInterval(activeDiff, started.Int64, endedUS)
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	queuedNow, activeNow := 0, 0
	for i := range buckets {
		queuedNow += queuedDiff[i]
		activeNow += activeDiff[i]
		out.Buckets[i].Queued = queuedNow
		out.Buckets[i].Active = activeNow
	}
	out.DurationP50MS = percentileMS(durations, 0.5)
	out.DurationP95MS = percentileMS(durations, 0.95)
	for i := range buckets {
		out.Buckets[i].DurationP50MS = percentileMS(bucketDurations[i], 0.5)
		out.Buckets[i].DurationP95MS = percentileMS(bucketDurations[i], 0.95)
	}
	for name, entry := range jobStats {
		entry.DurationP50MS = percentileMS(jobDurations[name], 0.5)
		entry.DurationP95MS = percentileMS(jobDurations[name], 0.95)
		out.Jobs = append(out.Jobs, *entry)
	}
	slices.SortFunc(out.Jobs, func(a, b RunJobMetrics) int {
		return cmp.Or(cmp.Compare(b.Total, a.Total), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

func percentileMS(values []int64, p float64) *int64 {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	v := values[min(len(values)-1, int(float64(len(values))*p))]
	return &v
}

func (s *Store) Run(ctx context.Context, id string) (model.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT run_id,definition_id,job,kind,revision,definition_hash,status,COALESCE(end_reason,''),trigger,attempt,COALESCE(parent_run_id,''),scheduled_for_us,missed_count,COALESCE(boot_id,''),COALESCE(pid,0),COALESCE(pgid,0),COALESCE(process_start_id,''),exit_code,COALESCE(signal,''),queued_us,started_us,ended_us,COALESCE(log_ref,''),log_bytes,log_truncated FROM runs WHERE run_id=?`, id)
	return scanRun(row)
}

func (s *Store) ScheduledRun(ctx context.Context, definitionID int64, scheduled time.Time) (model.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT run_id,definition_id,job,kind,revision,definition_hash,status,COALESCE(end_reason,''),trigger,attempt,COALESCE(parent_run_id,''),scheduled_for_us,missed_count,COALESCE(boot_id,''),COALESCE(pid,0),COALESCE(pgid,0),COALESCE(process_start_id,''),exit_code,COALESCE(signal,''),queued_us,started_us,ended_us,COALESCE(log_ref,''),log_bytes,log_truncated FROM runs WHERE definition_id=? AND scheduled_for_us=? AND trigger='schedule'`, definitionID, scheduled.UnixMicro())
	return scanRun(row)
}

type scanner interface{ Scan(...any) error }

func scanRun(row scanner) (model.Run, error) {
	var r model.Run
	var scheduled, started, ended sql.NullInt64
	var queued int64
	var code sql.NullInt64
	err := row.Scan(&r.ID, &r.DefinitionID, &r.Job, &r.Kind, &r.Revision, &r.DefinitionHash, &r.Status, &r.EndReason, &r.Trigger, &r.Attempt, &r.ParentRunID, &scheduled, &r.MissedCount, &r.BootID, &r.PID, &r.PGID, &r.ProcessStartID, &code, &r.Signal, &queued, &started, &ended, &r.LogRef, &r.LogBytes, &r.LogTruncated)
	if err != nil {
		return r, err
	}
	r.QueuedAt = time.UnixMicro(queued)
	if scheduled.Valid {
		t := time.UnixMicro(scheduled.Int64)
		r.ScheduledFor = &t
	}
	if started.Valid {
		t := time.UnixMicro(started.Int64)
		r.StartedAt = &t
	}
	if ended.Valid {
		t := time.UnixMicro(ended.Int64)
		r.EndedAt = &t
	}
	if code.Valid {
		v := int(code.Int64)
		r.ExitCode = &v
	}
	return r, nil
}

// RetentionCandidate is a run eligible for deletion and its position in the
// stable newest-first ordering used to resume a sweep after this page.
type RetentionCandidate struct {
	ID     string
	SortUS int64
}

// RetentionCandidates returns at most limit eligible runs after cursor. A
// protected idempotency key is skipped while its 24-hour window is active.
func (s *Store) RetentionCandidates(ctx context.Context, definitionID int64, keep int, olderThan time.Time, cursor *RetentionCandidate, limit int) ([]RetentionCandidate, error) {
	if limit <= 0 {
		return nil, errors.New("retention page limit must be positive")
	}
	countEnabled := keep > 0
	boundaryOffset := max(keep, 1) - 1 // The newest terminal run always survives.
	var after, sortUS int64
	var id string
	if cursor != nil {
		after, sortUS, id = 1, cursor.SortUS, cursor.ID
	}
	rows, err := s.db.QueryContext(ctx, `
WITH newest AS (
 SELECT run_id FROM runs WHERE definition_id=? AND status IN ('succeeded','failed','timeout','stopped','interrupted','skipped','missed')
 ORDER BY COALESCE(ended_us,queued_us) DESC, run_id DESC LIMIT 1
), boundary AS MATERIALIZED (
 SELECT COALESCE(ended_us,queued_us) AS sort_us,run_id FROM runs
 WHERE definition_id=? AND status IN ('succeeded','failed','timeout','stopped','interrupted','skipped','missed')
 ORDER BY sort_us DESC,run_id DESC LIMIT 1 OFFSET ?
)
SELECT r.run_id,COALESCE(r.ended_us,r.queued_us) AS sort_us FROM runs AS r
WHERE r.definition_id=? AND r.status IN ('succeeded','failed','timeout','stopped','interrupted','skipped','missed')
 AND r.run_id NOT IN (SELECT run_id FROM newest)
 AND ((? AND EXISTS (SELECT 1 FROM boundary AS b WHERE b.sort_us>COALESCE(r.ended_us,r.queued_us)
      OR (b.sort_us=COALESCE(r.ended_us,r.queued_us) AND b.run_id>r.run_id)))
      OR (? AND r.ended_us IS NOT NULL AND r.ended_us<?))
 AND NOT EXISTS (SELECT 1 FROM idempotency AS i WHERE i.run_id=r.run_id AND i.created_us>?)
 AND (?=0 OR sort_us<? OR (sort_us=? AND r.run_id<?))
ORDER BY sort_us DESC,r.run_id DESC LIMIT ?`,
		definitionID, definitionID, boundaryOffset, definitionID, countEnabled, !olderThan.IsZero(), olderThan.UnixMicro(),
		time.Now().Add(-24*time.Hour).UnixMicro(), after, sortUS, sortUS, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetentionCandidate
	for rows.Next() {
		var candidate RetentionCandidate
		if err := rows.Scan(&candidate.ID, &candidate.SortUS); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}
func (s *Store) DeleteRun(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE run_id=? AND status NOT IN ('pending','running')
		AND NOT EXISTS (SELECT 1 FROM idempotency WHERE idempotency.run_id=runs.run_id AND created_us>?)`, id, time.Now().Add(-24*time.Hour).UnixMicro())
	return err
}

func (s *Store) ScheduleState(ctx context.Context, definitionID int64) (anchor, last, next time.Time, hash string, err error) {
	var anchorUS int64
	var lastUS, nextUS sql.NullInt64
	err = s.db.QueryRowContext(ctx, "SELECT anchor_us,last_fire_us,next_fire_us,schedule_hash FROM schedule_state WHERE definition_id=?", definitionID).Scan(&anchorUS, &lastUS, &nextUS, &hash)
	if err != nil {
		return time.Time{}, time.Time{}, time.Time{}, "", err
	}
	anchor = time.UnixMicro(anchorUS)
	if lastUS.Valid {
		last = time.UnixMicro(lastUS.Int64)
	}
	if nextUS.Valid {
		next = time.UnixMicro(nextUS.Int64)
	}
	return anchor, last, next, hash, nil
}
func (s *Store) ScheduleNextBatch(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT definition_id,next_fire_us FROM schedule_state WHERE next_fire_us IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]time.Time)
	for rows.Next() {
		var id, next int64
		if err := rows.Scan(&id, &next); err != nil {
			return nil, err
		}
		out[id] = time.UnixMicro(next).UTC()
	}
	return out, rows.Err()
}

func (s *Store) ScheduleNext(ctx context.Context, definitionID int64) (time.Time, error) {
	var nextUS sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT next_fire_us FROM schedule_state WHERE definition_id=?", definitionID).Scan(&nextUS); err != nil {
		return time.Time{}, err
	}
	if !nextUS.Valid {
		return time.Time{}, nil
	}
	return time.UnixMicro(nextUS.Int64).UTC(), nil
}

func (s *Store) SetScheduleState(ctx context.Context, definitionID int64, hash string, anchor, last time.Time) error {
	return s.setScheduleState(ctx, definitionID, hash, anchor, last, time.Time{})
}

func (s *Store) SetScheduleStateWithNext(ctx context.Context, definitionID int64, hash string, anchor, last, next time.Time) error {
	return s.setScheduleState(ctx, definitionID, hash, anchor, last, next)
}

func (s *Store) setScheduleState(ctx context.Context, definitionID int64, hash string, anchor, last, next time.Time) error {
	var lastUS, nextUS any
	if !last.IsZero() {
		lastUS = last.UnixMicro()
	}
	if !next.IsZero() {
		nextUS = next.UnixMicro()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedule_state(definition_id,schedule_hash,anchor_us,last_fire_us,next_fire_us) VALUES(?,?,?,?,?) ON CONFLICT(definition_id) DO UPDATE SET schedule_hash=excluded.schedule_hash,anchor_us=excluded.anchor_us,last_fire_us=excluded.last_fire_us,next_fire_us=excluded.next_fire_us`, definitionID, hash, anchor.UnixMicro(), lastUS, nextUS)
	return err
}

// PruneMetadata removes expired replay keys and bounds audit/revision growth.
func (s *Store) PruneMetadata(ctx context.Context, auditKeep int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM idempotency WHERE created_us<=?", time.Now().Add(-24*time.Hour).UnixMicro()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM audit WHERE id NOT IN (SELECT id FROM audit ORDER BY id DESC LIMIT ?)", auditKeep); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM definition_revisions WHERE (definition_id,revision) NOT IN (
		SELECT definition_id,revision FROM (SELECT definition_id,revision,ROW_NUMBER() OVER (PARTITION BY definition_id ORDER BY revision DESC) AS n FROM definition_revisions) WHERE n<=100)`); err != nil {
		return err
	}
	return tx.Commit()
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func timePtrUS(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.UnixMicro()
}
