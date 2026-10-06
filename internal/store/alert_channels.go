package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/model"
)

const alertChannelsDDL = `CREATE TABLE alert_channels (
 name TEXT PRIMARY KEY, type TEXT NOT NULL, bot_token TEXT NOT NULL,
 chat_id TEXT NOT NULL, disable_notification INTEGER NOT NULL,
 batch_window INTEGER NOT NULL
);
`
const migration11 = `BEGIN;
` + alertChannelsDDL + `PRAGMA user_version=11;
COMMIT;`

var ErrUnknownAlertChannel = errors.New("unknown or duplicate alert channel")
var ErrAlertChannelReferenced = errors.New("alert channel is referenced by definitions")
var ErrAlertChannelLimit = errors.New("alert channel limit reached")

// AlertChannels is used at startup and on explicit mutations, never per alert.
// It includes credentials and must not be used directly in public responses.
func (s *Store) AlertChannels(ctx context.Context) ([]model.AlertChannel, error) {
	rows, err := s.rdb.QueryContext(ctx, "SELECT name,type,bot_token,chat_id,disable_notification,batch_window FROM alert_channels ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list alert channels: %w", err)
	}
	defer rows.Close()
	out := make([]model.AlertChannel, 0)
	for rows.Next() {
		var c model.AlertChannel
		if err := rows.Scan(&c.Name, &c.Type, &c.BotToken, &c.ChatID, &c.DisableNotification, &c.BatchWindow); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) PutAlertChannel(ctx context.Context, c model.AlertChannel, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var token string
	err = tx.QueryRowContext(ctx, "SELECT bot_token FROM alert_channels WHERE name=?", c.Name).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM alert_channels").Scan(&count); err != nil {
			return err
		}
		if count >= model.MaxAlertChannels {
			return ErrAlertChannelLimit
		}
	} else if err != nil {
		return err
	}
	// An omitted/empty token retains the existing credential on edit.
	if c.BotToken == "" {
		c.BotToken = token
	}
	if err := c.Normalize(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO alert_channels(name,type,bot_token,chat_id,disable_notification,batch_window) VALUES(?,?,?,?,?,?)
 ON CONFLICT(name) DO UPDATE SET type=excluded.type,bot_token=excluded.bot_token,chat_id=excluded.chat_id,disable_notification=excluded.disable_notification,batch_window=excluded.batch_window`, c.Name, c.Type, c.BotToken, c.ChatID, c.DisableNotification, c.BatchWindow); err != nil {
		return err
	}
	safe, err := json.Marshal(c.Redacted())
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target,after) VALUES(?,?,?,?,?)", time.Now().UnixMicro(), actor, "save_alert_channel", c.Name, string(safe)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAlertChannel atomically removes the channel and optionally edits all
// registry-owned references. Config-owned definitions remain file-authoritative
// and must be edited in the file first; no partial removal is committed.
func (s *Store) DeleteAlertChannel(ctx context.Context, name string, removeReferences bool, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM alert_channels WHERE name=?", name).Scan(&exists); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT definition_id,revision,spec,source,enabled FROM definitions WHERE deleted_us IS NULL ORDER BY name")
	if err != nil {
		return err
	}
	type change struct {
		d      model.Definition
		before string
	}
	var changes []change
	for rows.Next() {
		var d model.Definition
		var raw, source string
		var id, revision int64
		var enabled bool
		if err := rows.Scan(&id, &revision, &raw, &source, &enabled); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			rows.Close()
			return err
		}
		if !slices.Contains(d.Alerts, name) {
			continue
		}
		if !removeReferences {
			rows.Close()
			return fmt.Errorf("%w: %s", ErrAlertChannelReferenced, d.Name)
		}
		if source == "config" {
			rows.Close()
			return fmt.Errorf("%w: edit alerts in config-owned definition %s first", ErrReadOnly, d.Name)
		}
		d.ID, d.Revision, d.Source, d.Enabled = id, revision, source, &enabled
		d.Alerts = slices.DeleteFunc(d.Alerts, func(value string) bool { return value == name })
		changes = append(changes, change{d, raw})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	now := time.Now().UnixMicro()
	for _, change := range changes {
		d := change.d
		b, hash, err := config.Canonical(d)
		if err != nil {
			return err
		}
		revision := d.Revision + 1
		if _, err := tx.ExecContext(ctx, "UPDATE definitions SET spec=?,spec_hash=?,revision=?,updated_us=? WHERE definition_id=?", string(b), hash, revision, now, d.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO definition_revisions(definition_id,revision,spec,spec_hash,actor,at_us) VALUES(?,?,?,?,?,?)", d.ID, revision, string(b), hash, actor, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target,before,after) VALUES(?,?,?,?,?,?)", now, actor, "remove_alert_reference", d.Name, change.before, string(b)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM alert_channels WHERE name=?", name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit(at_us,actor,action,target) VALUES(?,?,?,?)", now, actor, "delete_alert_channel", name); err != nil {
		return err
	}
	return tx.Commit()
}

// Check inside the writer transaction as well as at the API boundary so a
// concurrent delete cannot leave a newly saved/imported definition dangling.
func validateAlertReferencesTx(ctx context.Context, tx *sql.Tx, d model.Definition) error {
	seen := make(map[string]bool, len(d.Alerts))
	for _, name := range d.Alerts {
		if seen[name] {
			return fmt.Errorf("%w: %s.alerts", ErrUnknownAlertChannel, d.Name)
		}
		seen[name] = true
		var exists int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM alert_channels WHERE name=?", name).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s.alerts: %q", ErrUnknownAlertChannel, d.Name, name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
