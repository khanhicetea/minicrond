package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/khanhicetea/minicrond/internal/model"
)

const workerStatePrefix = "worker_exhaustion:"

// SaveWorkerExhaustion uses existing durable metadata, with one bounded record
// per definition. A replaced/deleted definition cannot acquire stale state.
func (s *Store) SaveWorkerExhaustion(ctx context.Context, d model.Definition, state model.WorkerExhaustion) error {
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode worker exhaustion: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value)
 SELECT ?,? FROM definitions WHERE definition_id=? AND revision=? AND deleted_us IS NULL
 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, workerStatePrefix+strconv.FormatInt(d.ID, 10), string(b), d.ID, d.Revision)
	if err != nil {
		return fmt.Errorf("persist worker exhaustion: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRevisionConflict
	}
	return nil
}

// ClearWorkerExhaustion clears one worker, or all workers for an explicit reload.
func (s *Store) ClearWorkerExhaustion(ctx context.Context, name string) error {
	var err error
	if name == "" {
		_, err = s.db.ExecContext(ctx, `DELETE FROM meta WHERE substr(key,1,?)=?`, len(workerStatePrefix), workerStatePrefix)
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM meta WHERE key IN (SELECT ? || definition_id FROM definitions WHERE name=?)`, workerStatePrefix, name)
	}
	if err != nil {
		return fmt.Errorf("clear worker exhaustion: %w", err)
	}
	return nil
}

// WorkerExhaustions reads durable outcomes on demand without capture-path work.
func (s *Store) WorkerExhaustions(ctx context.Context) (map[string]model.WorkerExhaustion, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT d.name,d.revision,m.value FROM definitions d JOIN meta m ON m.key=? || d.definition_id WHERE d.kind='worker' AND d.deleted_us IS NULL`, workerStatePrefix)
	if err != nil {
		return nil, fmt.Errorf("read worker exhaustion: %w", err)
	}
	defer rows.Close()
	result := make(map[string]model.WorkerExhaustion)
	for rows.Next() {
		var name, value string
		var revision int64
		if err := rows.Scan(&name, &revision, &value); err != nil {
			return nil, err
		}
		var state model.WorkerExhaustion
		if err := json.Unmarshal([]byte(value), &state); err != nil {
			return nil, fmt.Errorf("decode worker %s exhaustion: %w", name, err)
		}
		if state.Revision == revision {
			result[name] = state
		}
	}
	return result, rows.Err()
}
