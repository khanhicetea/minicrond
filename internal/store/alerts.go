package store

import (
	"context"
	"database/sql"
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO alert_deliveries(run_id,channel,status,attempts,last_error,updated_us) VALUES(?,?,?,?,?,?)
		ON CONFLICT(run_id,channel) DO UPDATE SET status=excluded.status,attempts=excluded.attempts,last_error=excluded.last_error,updated_us=excluded.updated_us`,
		runID, channel, status, attempts, lastError, time.Now().UTC().UnixMicro())
	return err
}

func (s *Store) RunAlerts(ctx context.Context, runID string) ([]AlertDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel,status,attempts,last_error,updated_us FROM alert_deliveries WHERE run_id=? ORDER BY channel`, runID)
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
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM alert_deliveries GROUP BY status`)
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
