package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/khanhicetea/minicrond/internal/alerts"
	"github.com/khanhicetea/minicrond/internal/model"
)

func (d *Daemon) currentAlertChannels() []model.AlertChannel {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.alertChannels)
}

// Channel changes share reload ownership with config reload and reconciliation.
// No polling or per-delivery database access is added.
func (d *Daemon) SaveAlertChannel(ctx context.Context, channel model.AlertChannel) error {
	if !lockContext(ctx, &d.reloadMu) {
		return ctx.Err()
	}
	defer d.reloadMu.Unlock()
	if _, err := d.readyConfig(); err != nil {
		return err
	}
	if err := d.store.PutAlertChannel(ctx, channel, "api"); err != nil {
		return err
	}
	return d.reloadAlertChannels(context.WithoutCancel(ctx))
}

func (d *Daemon) DeleteAlertChannel(ctx context.Context, name string, removeReferences bool) error {
	if !lockContext(ctx, &d.reloadMu) {
		return ctx.Err()
	}
	defer d.reloadMu.Unlock()
	if _, err := d.readyConfig(); err != nil {
		return err
	}
	if err := d.store.DeleteAlertChannel(ctx, name, removeReferences, "api"); err != nil {
		return err
	}
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := d.reloadAlertChannels(postCtx); err != nil {
		return err
	}
	return d.reconcile(postCtx)
}

// Post-commit reload is independent of request cancellation. Failure is reported
// to the caller; restart also reloads the authoritative database.
func (d *Daemon) reloadAlertChannels(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	channels, err := d.store.AlertChannels(ctx)
	if err != nil {
		return fmt.Errorf("reload committed alert channels: %w", err)
	}
	registry, err := alerts.Prepare(channels)
	if err != nil {
		return err
	}
	if err := d.alerts.Load().Apply(registry); err != nil {
		return err
	}
	d.mu.Lock()
	d.alertChannels = channels
	d.mu.Unlock()
	return nil
}
