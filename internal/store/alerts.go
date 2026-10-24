package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AlertDelivery is an observation of best-effort delivery, not a durable work item.
type AlertDelivery struct {
	Channel   string    `json:"channel"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) RecordAlert(ctx context.Context, runID, channel, status string, attempts int, lastError string) error {
	_, err := s.db.ExecContext(ctx, recordAlert, runID, channel, status, attempts, lastError, time.Now().UTC().UnixMicro())
	return err
}

// An observation never overwrites a newer one: observations parked while
// their run row was deferred (ADR-16) are written after later ones.
const recordAlert = `INSERT INTO alert_deliveries(run_id,channel,status,attempts,last_error,updated_us) VALUES(?,?,?,?,?,?)
	ON CONFLICT(run_id,channel) DO UPDATE SET status=excluded.status,attempts=excluded.attempts,last_error=excluded.last_error,updated_us=excluded.updated_us
	WHERE excluded.updated_us >= alert_deliveries.updated_us`

// AlertUpdate is one delivery observation for RecordAlerts.
type AlertUpdate struct {
	RunID, Channel, Status string
	Attempts               int
	LastError              string
	// At is when the change was observed; zero means now.
	At time.Time
}

func (u AlertUpdate) stamp(now int64) int64 {
	if u.At.IsZero() {
		return now
	}
	return u.At.UTC().UnixMicro()
}

// RecordAlerts applies a group of delivery observations in one transaction,
// in order, so a batch transition costs one commit instead of one per item.
func (s *Store) RecordAlerts(ctx context.Context, updates []AlertUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, recordAlert)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC().UnixMicro()
	for _, u := range updates {
		if _, err := stmt.ExecContext(ctx, u.RunID, u.Channel, u.Status, u.Attempts, u.LastError, u.stamp(now)); err != nil {
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordAlertsForStoredRuns is RecordAlerts for runs whose row may not be
// stored yet (ADR-16 deferred history). Updates for a missing run are not
// written but returned, in order, so the caller can retry them once the row
// exists; the rest commit in one transaction.
func (s *Store) RecordAlertsForStoredRuns(ctx context.Context, updates []AlertUpdate) (missing []AlertUpdate, err error) {
	if len(updates) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, recordAlert)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	stored := make(map[string]bool)
	now := time.Now().UTC().UnixMicro()
	for _, u := range updates {
		ok, seen := stored[u.RunID]
		if !seen {
			var one int
			switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE run_id=?`, u.RunID).Scan(&one); {
			case err == nil:
				ok = true
			case errors.Is(err, sql.ErrNoRows):
			default:
				return nil, err
			}
			stored[u.RunID] = ok
		}
		if !ok {
			missing = append(missing, u)
			continue
		}
		if _, err := stmt.ExecContext(ctx, u.RunID, u.Channel, u.Status, u.Attempts, u.LastError, u.stamp(now)); err != nil {
			return nil, err
		}
	}
	if err := stmt.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return missing, nil
}

func (s *Store) RunAlerts(ctx context.Context, runID string) ([]AlertDelivery, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT channel,status,attempts,last_error,updated_us FROM alert_deliveries WHERE run_id=? ORDER BY channel`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AlertDelivery, 0)
	for rows.Next() {
		var item AlertDelivery
		var last sql.NullString
		var updated int64
		if err := rows.Scan(&item.Channel, &item.Status, &item.Attempts, &last, &updated); err != nil {
			return nil, err
		}
		item.LastError = last.String
		item.UpdatedAt = time.UnixMicro(updated).UTC()
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) AlertMetrics(ctx context.Context) (map[string]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT status,count(*) FROM alert_deliveries GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

// Alert work lives only in memory. Mark observations stranded by a restart.
func (s *Store) InterruptAlerts(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alert_deliveries SET status='interrupted',last_error='daemon restarted before delivery completed',updated_us=? WHERE status IN ('queued','sending')`, time.Now().UTC().UnixMicro())
	return err
}
