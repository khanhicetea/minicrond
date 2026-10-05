package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// ErrQueueFull reports that an enqueue would exceed a queue limit. The wrapped
// message names the limit.
var ErrQueueFull = errors.New("execution queue is full")

// ErrQueueDuplicate reports that the definition already has a queued run and
// its overlap policy refuses another.
var ErrQueueDuplicate = errors.New("definition already has a queued run")

// QueueLimits bound the durable execution queue; every limit is enforced inside
// the enqueue transaction.
type QueueLimits struct {
	MaxItems  int
	MaxPerJob int
	MaxBytes  int64
	MaxAge    time.Duration
}

// Idempotency identifies a replay key reserved together with a queued run.
type Idempotency struct{ Principal, Operation, Key, RequestHash string }

// QueueStats summarizes the queue.
type QueueStats struct {
	Depth    int
	Bytes    int64
	OldestUS int64 // enqueue time of the oldest item, 0 when empty
}

// QueueItem is the scheduling view of a queued run.
type QueueItem struct {
	Seq          int64
	RunID        string
	DefinitionID int64
	ExpiresUS    int64
}

// queuePayloadOverhead approximates the fixed part of one persisted item.
const queuePayloadOverhead = 128

// QueuePayloadBytes is the accounted size of a queued run: its identity strings
// plus a fixed row overhead. The queue never stores a definition snapshot.
func QueuePayloadBytes(r model.Run, idem *Idempotency) int64 {
	n := queuePayloadOverhead + len(r.ID) + len(r.Job) + len(r.DefinitionHash) + len(r.ParentRunID) + len(r.Trigger) + len(r.BootID)
	if idem != nil {
		n += len(idem.Principal) + len(idem.Operation) + len(idem.Key) + len(idem.RequestHash)
	}
	return int64(n)
}

// EnqueueRun persists r (status "queued") and its queue row in one transaction,
// after checking every limit. unique additionally refuses a second queued item
// for the same definition (ErrQueueDuplicate). A non-empty replay ID means a
// live idempotency key already owns a run and nothing was queued.
func (s *Store) EnqueueRun(ctx context.Context, r model.Run, idem *Idempotency, lim QueueLimits, unique bool) (replay string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if idem != nil && idem.Key != "" {
		if replay, err = reserveIdempotency(ctx, tx, idem.Principal, idem.Operation, idem.Key, idem.RequestHash); err != nil || replay != "" {
			return replay, err
		}
	}
	var depth, perJob int
	var bytes int64
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(payload_bytes),0) FROM exec_queue").Scan(&depth, &bytes); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM exec_queue WHERE definition_id=?", r.DefinitionID).Scan(&perJob); err != nil {
		return "", err
	}
	size := QueuePayloadBytes(r, idem)
	switch {
	case unique && perJob > 0:
		return "", ErrQueueDuplicate
	case depth >= lim.MaxItems:
		return "", fmt.Errorf("%w: %d items queued (queue.max_items)", ErrQueueFull, depth)
	case perJob >= lim.MaxPerJob:
		return "", fmt.Errorf("%w: %d items queued for %s (queue.max_per_job)", ErrQueueFull, perJob, r.Job)
	case bytes+size > lim.MaxBytes:
		return "", fmt.Errorf("%w: %d payload bytes queued (queue.max_bytes)", ErrQueueFull, bytes)
	}
	r.Status = "queued"
	if _, err = tx.ExecContext(ctx, createRunSQL, runArgs(r)...); err != nil {
		return "", err
	}
	now := time.Now()
	if _, err = tx.ExecContext(ctx, "INSERT INTO exec_queue(run_id,definition_id,enqueued_us,expires_us,payload_bytes) VALUES(?,?,?,?,?)", r.ID, r.DefinitionID, now.UnixMicro(), now.Add(lim.MaxAge).UnixMicro(), size); err != nil {
		return "", err
	}
	if idem != nil && idem.Key != "" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO idempotency(principal,operation,key,request_hash,run_id,created_us) VALUES(?,?,?,?,?,?)", idem.Principal, idem.Operation, idem.Key, idem.RequestHash, r.ID, now.UnixMicro()); err != nil {
			return "", err
		}
	}
	return "", tx.Commit()
}

// QueueStats reads the queue's depth, bytes and oldest enqueue time.
func (s *Store) QueueStats(ctx context.Context) (QueueStats, error) {
	var st QueueStats
	var oldest sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(payload_bytes),0),MIN(enqueued_us) FROM exec_queue").Scan(&st.Depth, &st.Bytes, &oldest)
	st.OldestUS = oldest.Int64
	return st, err
}

// QueueStatsRead is QueueStats on the read pool, for diagnostics requests.
func (s *Store) QueueStatsRead(ctx context.Context) (QueueStats, error) {
	var st QueueStats
	var oldest sql.NullInt64
	err := s.rdb.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(payload_bytes),0),MIN(enqueued_us) FROM exec_queue").Scan(&st.Depth, &st.Bytes, &oldest)
	st.OldestUS = oldest.Int64
	return st, err
}

// NextQueueExpiry returns the earliest expiry of any queued item.
func (s *Store) NextQueueExpiry(ctx context.Context) (time.Time, bool, error) {
	var us sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(expires_us) FROM exec_queue").Scan(&us); err != nil {
		return time.Time{}, false, err
	}
	if !us.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMicro(us.Int64), true, nil
}

// QueueHeads returns the oldest queued item of each definition, oldest first,
// at most limit of them. Fairness across definitions is chosen by the caller.
func (s *Store) QueueHeads(ctx context.Context, limit int) ([]QueueItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT q.seq,q.run_id,q.definition_id,q.expires_us FROM exec_queue AS q
		WHERE q.seq=(SELECT MIN(seq) FROM exec_queue WHERE definition_id=q.definition_id) ORDER BY q.seq LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	for rows.Next() {
		var it QueueItem
		if err := rows.Scan(&it.Seq, &it.RunID, &it.DefinitionID, &it.ExpiresUS); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ExpireQueued ends up to limit queued runs whose expiry has passed: each
// becomes a terminal skipped/queue_expired run and leaves the queue in the
// same transaction. It returns the run IDs expired.
func (s *Store) ExpireQueued(ctx context.Context, now time.Time, limit int) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT run_id FROM exec_queue WHERE expires_us<=? ORDER BY expires_us LIMIT ?", now.UnixMicro(), limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}
	args := []any{now.UnixMicro()}
	for _, id := range ids {
		args = append(args, id)
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET status='skipped',end_reason='queue_expired',ended_us=? WHERE status='queued' AND run_id IN ("+in+")", args...); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM exec_queue WHERE run_id IN ("+in+")", args[1:]...); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

// DropQueued ends one queued run as skipped with reason and removes it from the
// queue. ErrInvalidTransition means it was no longer queued.
func (s *Store) DropQueued(ctx context.Context, runID, reason string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE runs SET status='skipped',end_reason=?,ended_us=? WHERE run_id=? AND status='queued'", reason, now.UnixMicro(), runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("drop queued run %s: %w", runID, ErrInvalidTransition)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM exec_queue WHERE run_id=?", runID); err != nil {
		return err
	}
	return tx.Commit()
}

// DequeueRun turns a queued run into a pending one and removes its queue row in
// one transaction, recording the definition revision and hash it will run
// under. After it commits the run is no longer replayable: recovery marks a
// pending run interrupted, never queued again. ErrInvalidTransition means the
// run was no longer queued (for example it expired first).
func (s *Store) DequeueRun(ctx context.Context, runID string, revision int64, hash, bootID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE runs SET status='pending',revision=?,definition_hash=?,boot_id=? WHERE run_id=? AND status='queued'", revision, hash, bootID, runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("dequeue run %s: %w", runID, ErrInvalidTransition)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM exec_queue WHERE run_id=?", runID); err != nil {
		return err
	}
	return tx.Commit()
}
