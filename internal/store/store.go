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
	"time"

	_ "modernc.org/sqlite"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/model"
)

const SchemaVersion = 1

// Sentinel errors used by callers to map storage failures onto API statuses.
var (
	ErrAuthorityConflict = errors.New("authority conflict")
	ErrRevisionConflict  = errors.New("revision conflict")
)

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
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
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
	if version == 0 {
		if _, err := s.db.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("migration 1: %w", err)
		}
	}
	return nil
}

const schema = `
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
PRAGMA user_version=1;
COMMIT;`

// SyncFiles atomically reconciles all file-managed definitions.
func (s *Store) SyncFiles(ctx context.Context, defs []model.Definition, prune bool) error {
	return s.syncFiles(ctx, defs, prune, "")
}

// SyncSource atomically reconciles definitions from one source without changing
// definitions from other file sources.
func (s *Store) SyncSource(ctx context.Context, defs []model.Definition, source string, prune bool) error {
	for _, d := range defs {
		if d.SourceFile != source {
			return fmt.Errorf("definition %q does not belong to source %q", d.Name, source)
		}
	}
	return s.syncFiles(ctx, defs, prune, source)
}

func (s *Store) syncFiles(ctx context.Context, defs []model.Definition, prune bool, source string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := make(map[string]bool)
	for _, d := range defs {
		seen[d.Name] = true
		b, hash, err := config.Canonical(d)
		if err != nil {
			return err
		}
		var id, rev int64
		var authority, oldHash string
		err = tx.QueryRowContext(ctx, "SELECT definition_id,revision,authority,spec_hash FROM definitions WHERE name=? AND deleted_us IS NULL", d.Name).Scan(&id, &rev, &authority, &oldHash)
		now := time.Now().UnixMicro()
		enabled := d.IsEnabled()
		switch {
		case errors.Is(err, sql.ErrNoRows):
			res, err := tx.ExecContext(ctx, `INSERT INTO definitions(name,authority,kind,spec,spec_hash,source_file,revision,enabled,created_us,updated_us) VALUES(?,?,?,?,?,?,1,?,?,?)`, d.Name, "file", d.Kind, string(b), hash, d.SourceFile, enabled, now, now)
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
			rev = 1
		case err != nil:
			return err
		case authority == "db":
			return fmt.Errorf("%w: %s is managed by the database", ErrAuthorityConflict, d.Name)
		case oldHash == hash:
			_, err = tx.ExecContext(ctx, "UPDATE definitions SET source_file=?,enabled=?,updated_us=? WHERE definition_id=?", d.SourceFile, enabled, now, id)
			if err != nil {
				return err
			}
			continue
		default:
			rev++
			if _, err := tx.ExecContext(ctx, "UPDATE definitions SET spec=?,spec_hash=?,source_file=?,revision=?,enabled=?,updated_us=? WHERE definition_id=?", string(b), hash, d.SourceFile, rev, enabled, now, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO definition_revisions(definition_id,revision,spec,spec_hash,actor,at_us) VALUES(?,?,?,?,?,?)`, id, rev, string(b), hash, "file:"+d.SourceFile, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit(at_us,actor,action,target,after) VALUES(?,?,?,?,?)`, now, "file:"+d.SourceFile, "import", d.Name, string(b)); err != nil {
			return err
		}
	}
	query := "SELECT definition_id,name FROM definitions WHERE authority='file' AND deleted_us IS NULL"
	var args []any
	if source != "" {
		query += " AND source_file=?"
		args = append(args, source)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	type missing struct {
		id   int64
		name string
	}
	var absent []missing
	for rows.Next() {
		var x missing
		if err := rows.Scan(&x.id, &x.name); err != nil {
			rows.Close()
			return err
		}
		if !seen[x.name] {
			absent = append(absent, x)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, x := range absent {
		if prune {
			_, err = tx.ExecContext(ctx, "UPDATE definitions SET deleted_us=?,enabled=0 WHERE definition_id=?", time.Now().UnixMicro(), x.id)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE definitions SET enabled=0 WHERE definition_id=?", x.id)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Definitions(ctx context.Context) ([]model.Definition, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT definition_id,spec,authority,COALESCE(source_file,''),revision,enabled FROM definitions WHERE deleted_us IS NULL ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Definition
	for rows.Next() {
		var d model.Definition
		var raw string
		var enabled bool
		if err := rows.Scan(&d.ID, &raw, &d.Authority, &d.SourceFile, &d.Revision, &enabled); err != nil {
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
	var enabled bool
	err := s.db.QueryRowContext(ctx, "SELECT definition_id,spec,spec_hash,authority,COALESCE(source_file,''),revision,enabled FROM definitions WHERE name=? AND deleted_us IS NULL", name).Scan(&d.ID, &raw, &hash, &d.Authority, &d.SourceFile, &d.Revision, &enabled)
	if err != nil {
		return d, "", err
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return d, "", err
	}
	d.Enabled = &enabled
	return d, hash, nil
}

func (s *Store) CopyDefinitions(ctx context.Context, defs []model.Definition, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMicro()
	for _, d := range defs {
		d.Authority = "db"
		d.SourceFile = ""
		b, hash, err := config.Canonical(d)
		if err != nil {
			return err
		}
		var id, rev int64
		var authority, before string
		err = tx.QueryRowContext(ctx, "SELECT definition_id,revision,authority,spec FROM definitions WHERE name=? AND deleted_us IS NULL", d.Name).Scan(&id, &rev, &authority, &before)
		if errors.Is(err, sql.ErrNoRows) {
			res, execErr := tx.ExecContext(ctx, `INSERT INTO definitions(name,authority,kind,spec,spec_hash,revision,enabled,created_us,updated_us) VALUES(?,'db',?,?,?,1,?,?,?)`, d.Name, d.Kind, string(b), hash, d.IsEnabled(), now, now)
			if execErr != nil {
				return execErr
			}
			id, _ = res.LastInsertId()
			rev = 1
		} else if err != nil {
			return err
		} else {
			if authority == "file" {
				return fmt.Errorf("%w: %s is managed by a file", ErrAuthorityConflict, d.Name)
			}
			rev++
			if _, err = tx.ExecContext(ctx, "UPDATE definitions SET kind=?,spec=?,spec_hash=?,revision=?,enabled=?,updated_us=? WHERE definition_id=?", d.Kind, string(b), hash, rev, d.IsEnabled(), now, id); err != nil {
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

func (s *Store) DeleteDefinition(ctx context.Context, name, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var authority, before string
	if err = tx.QueryRowContext(ctx, "SELECT authority,spec FROM definitions WHERE name=? AND deleted_us IS NULL", name).Scan(&authority, &before); err != nil {
		return err
	}
	if authority == "file" {
		return fmt.Errorf("%w: %s is managed by a file", ErrAuthorityConflict, name)
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
	res, err := s.db.ExecContext(ctx, "UPDATE definitions SET enabled=?,updated_us=? WHERE name=? AND authority='db' AND deleted_us IS NULL", enabled, time.Now().UnixMicro(), name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w or definition not found", ErrAuthorityConflict)
	}
	return nil
}

func (s *Store) PutDefinition(ctx context.Context, d model.Definition, expected int64, actor string) (model.Definition, error) {
	d.Authority = "db"
	d.SourceFile = ""
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
	var authority string
	var before string
	err = tx.QueryRowContext(ctx, "SELECT definition_id,revision,authority,spec FROM definitions WHERE name=? AND deleted_us IS NULL", d.Name).Scan(&id, &rev, &authority, &before)
	if errors.Is(err, sql.ErrNoRows) {
		res, e := tx.ExecContext(ctx, `INSERT INTO definitions(name,authority,kind,spec,spec_hash,revision,enabled,created_us,updated_us) VALUES(?,'db',?,?,?,1,?,?,?)`, d.Name, d.Kind, string(b), hash, d.IsEnabled(), now, now)
		if e != nil {
			return d, e
		}
		id, _ = res.LastInsertId()
		rev = 1
	} else if err != nil {
		return d, err
	} else {
		if authority == "file" {
			return d, fmt.Errorf("%w: %s is managed by a file", ErrAuthorityConflict, d.Name)
		}
		if expected != 0 && expected != rev {
			return d, fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, expected, rev)
		}
		rev++
		if _, err = tx.ExecContext(ctx, "UPDATE definitions SET kind=?,spec=?,spec_hash=?,revision=?,enabled=?,updated_us=? WHERE definition_id=?", d.Kind, string(b), hash, rev, d.IsEnabled(), now, id); err != nil {
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO idempotency(principal,operation,key,request_hash,run_id,created_us) VALUES(?,?,?,?,?,?)`, principal, operation, key, requestHash, runID, time.Now().UnixMicro())
	return err
}

func (s *Store) CreateRun(ctx context.Context, r model.Run) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(run_id,definition_id,job,kind,revision,definition_hash,status,end_reason,trigger,attempt,scheduled_for_us,missed_count,boot_id,queued_us,ended_us,log_ref) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.DefinitionID, r.Job, r.Kind, r.Revision, r.DefinitionHash, r.Status, nullString(r.EndReason), r.Trigger, r.Attempt, timePtrUS(r.ScheduledFor), r.MissedCount, r.BootID, r.QueuedAt.UnixMicro(), timePtrUS(r.EndedAt), r.LogRef)
	return err
}
func (s *Store) StartRun(ctx context.Context, id string, pid, pgid int, startID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE runs SET status='running',pid=?,pgid=?,process_start_id=?,started_us=? WHERE run_id=? AND status='pending'", pid, pgid, startID, at.UnixMicro(), id)
	return err
}
func (s *Store) FinishRun(ctx context.Context, id, status, reason string, code *int, signal string, at time.Time, bytes int64, truncated bool) error {
	_, err := s.db.ExecContext(ctx, "UPDATE runs SET status=?,end_reason=?,exit_code=?,signal=?,ended_us=?,log_bytes=?,log_truncated=? WHERE run_id=? AND status IN ('pending','running')", status, reason, code, nullString(signal), at.UnixMicro(), bytes, truncated, id)
	return err
}
func (s *Store) Recoverable(ctx context.Context) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,COALESCE(pid,0),COALESCE(pgid,0),COALESCE(process_start_id,'') FROM runs WHERE status IN ('pending','running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Run
	for rows.Next() {
		var r model.Run
		if err := rows.Scan(&r.ID, &r.PID, &r.PGID, &r.ProcessStartID); err != nil {
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
	limit = min(max(limit, 1), 500)
	q := `SELECT run_id,definition_id,job,kind,revision,definition_hash,status,COALESCE(end_reason,''),trigger,attempt,scheduled_for_us,missed_count,COALESCE(boot_id,''),COALESCE(pid,0),COALESCE(pgid,0),COALESCE(process_start_id,''),exit_code,COALESCE(signal,''),queued_us,started_us,ended_us,COALESCE(log_ref,''),log_bytes,log_truncated FROM runs`
	var args []any
	if job != "" {
		q += " WHERE job=?"
		args = append(args, job)
	}
	q += " ORDER BY queued_us DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Run
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
	jobStats := make(map[string]*RunJobMetrics)
	var durations []int64
	startUS, endUS := since.UnixMicro(), now.UnixMicro()
	windowUS := max(endUS-startUS, 1)
	bucketIndex := func(us int64) int {
		return min(buckets-1, max(0, int((us-startUS)*int64(buckets)/windowUS)))
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
		for i := range buckets {
			t := startUS + (int64(i)*windowUS)/int64(buckets) + windowUS/int64(buckets)/2
			endedUS := int64(1 << 62)
			if ended.Valid {
				endedUS = ended.Int64
			}
			if queuedUS <= t && (!started.Valid || t < started.Int64) && t < endedUS {
				out.Buckets[i].Queued++
			}
			if started.Valid && started.Int64 <= t && t < endedUS {
				out.Buckets[i].Active++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
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
	row := s.db.QueryRowContext(ctx, `SELECT run_id,definition_id,job,kind,revision,definition_hash,status,COALESCE(end_reason,''),trigger,attempt,scheduled_for_us,missed_count,COALESCE(boot_id,''),COALESCE(pid,0),COALESCE(pgid,0),COALESCE(process_start_id,''),exit_code,COALESCE(signal,''),queued_us,started_us,ended_us,COALESCE(log_ref,''),log_bytes,log_truncated FROM runs WHERE run_id=?`, id)
	return scanRun(row)
}

type scanner interface{ Scan(...any) error }

func scanRun(row scanner) (model.Run, error) {
	var r model.Run
	var scheduled, started, ended sql.NullInt64
	var queued int64
	var code sql.NullInt64
	err := row.Scan(&r.ID, &r.DefinitionID, &r.Job, &r.Kind, &r.Revision, &r.DefinitionHash, &r.Status, &r.EndReason, &r.Trigger, &r.Attempt, &scheduled, &r.MissedCount, &r.BootID, &r.PID, &r.PGID, &r.ProcessStartID, &code, &r.Signal, &queued, &started, &ended, &r.LogRef, &r.LogBytes, &r.LogTruncated)
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
func (s *Store) RetentionCandidates(ctx context.Context, definitionID int64, keep int, olderThan time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,ended_us FROM runs WHERE definition_id=? AND status IN ('succeeded','failed','timeout','stopped','interrupted','skipped','missed') ORDER BY COALESCE(ended_us,queued_us) DESC`, definitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	index := 0
	for rows.Next() {
		var id string
		var ended sql.NullInt64
		if err := rows.Scan(&id, &ended); err != nil {
			return nil, err
		}
		index++
		if index == 1 {
			continue
		}
		tooMany := keep > 0 && index > keep
		tooOld := !olderThan.IsZero() && ended.Valid && time.UnixMicro(ended.Int64).Before(olderThan)
		if tooMany || tooOld {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}
func (s *Store) DeleteRun(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM runs WHERE run_id=? AND status NOT IN ('pending','running')", id)
	return err
}

func (s *Store) ScheduleState(ctx context.Context, definitionID int64) (anchor, last time.Time, hash string, err error) {
	var anchorUS int64
	var lastUS sql.NullInt64
	err = s.db.QueryRowContext(ctx, "SELECT anchor_us,last_fire_us,schedule_hash FROM schedule_state WHERE definition_id=?", definitionID).Scan(&anchorUS, &lastUS, &hash)
	if err != nil {
		return time.Time{}, time.Time{}, "", err
	}
	anchor = time.UnixMicro(anchorUS)
	if lastUS.Valid {
		last = time.UnixMicro(lastUS.Int64)
	}
	return anchor, last, hash, nil
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
