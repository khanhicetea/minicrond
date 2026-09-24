// Package alerts routes terminal run alerts to configured delivery channels.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/model"
)

const telegramAPI = "https://api.telegram.org"

// Channel is implemented by an outbound alert transport.
type Channel interface {
	Send(context.Context, Alert) error
}

// Alert is the transport-neutral payload produced for an unsuccessful run.
type Alert struct {
	Run  model.Run   // retained for single-run transports
	Runs []model.Run // populated for batched delivery
}

type delivery struct {
	channel Channel
	alert   Alert
	name    string
	window  time.Duration
}

// Dispatcher asynchronously delivers alerts. Its channel registry can be
// replaced during a configuration reload without interrupting deliveries
// already in progress.
type Dispatcher struct {
	mu       sync.RWMutex
	channels map[string]Channel
	windows  map[string]time.Duration
	queue    chan delivery
	work     chan []delivery
	record   func(context.Context, string, string, string, int, string) error
	closed   bool
	pending  atomic.Int64
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

// New creates a dispatcher and validates/resolves channel credentials.
func New(channels []config.AlertChannel, record func(context.Context, string, string, string, int, string) error) (*Dispatcher, error) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{channels: make(map[string]Channel), queue: make(chan delivery, 256), work: make(chan []delivery, 256), record: record, ctx: ctx, cancel: cancel}
	if err := d.Reload(channels); err != nil {
		cancel()
		return nil, err
	}
	for range 4 {
		d.wg.Go(d.run)
	}
	d.wg.Go(d.batch)
	return d, nil
}

// Reload atomically replaces the available delivery channels.
func (d *Dispatcher) Reload(configs []config.AlertChannel) error {
	channels := make(map[string]Channel, len(configs))
	windows := make(map[string]time.Duration, len(configs))
	for _, cfg := range configs {
		window := cfg.BatchWindow
		if window == 0 {
			window = 10
		}
		if window < 1 || window > 3600 {
			return fmt.Errorf("alert channel %q: invalid batch_window", cfg.Name)
		}
		windows[cfg.Name] = time.Duration(window) * time.Second
		var channel Channel
		switch cfg.Type {
		case "telegram":
			token, err := resolveSecret(cfg.BotToken)
			if err != nil {
				return fmt.Errorf("alert channel %q: resolve bot_token: %w", cfg.Name, err)
			}
			channel = newTelegram(token, cfg.ChatID, cfg.DisableNotification, telegramAPI, &http.Client{Timeout: 10 * time.Second})
		default:
			return fmt.Errorf("alert channel %q: unsupported type %q", cfg.Name, cfg.Type)
		}
		channels[cfg.Name] = channel
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("alert dispatcher is closed")
	}
	d.channels = channels
	d.windows = windows
	return nil
}

// Notify queues alerts for failed and timed-out runs. Definitions opt in by
// naming channels in their alerts field.
func (d *Dispatcher) Notify(run model.Run, definition model.Definition) {
	if run.Status != "failed" && run.Status != "timeout" {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return
	}
	for _, name := range definition.Alerts {
		channel := d.channels[name]
		if channel == nil {
			d.report(run.ID, name, "dropped", 0, "alert channel is unavailable")
			continue
		}
		d.report(run.ID, name, "queued", 0, "")
		d.pending.Add(1)
		select {
		case d.queue <- delivery{channel: channel, alert: Alert{Run: run}, name: name, window: d.windows[name]}:
		default:
			d.pending.Add(-1)
			d.report(run.ID, name, "dropped", 0, "alert queue is full")
		}
	}
}

func (d *Dispatcher) report(runID, name, status string, attempts int, reason string) {
	if d.record == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.record(ctx, runID, name, status, attempts, reason); err != nil {
		slog.Error("recording alert delivery failed", "channel", name, "run", runID, "error", err)
	}
}

// batch starts a window on the first delivery to each channel. A reload
// flushes the previous channel instance rather than mixing credentials.
func (d *Dispatcher) batch() {
	defer close(d.work)
	type pending struct {
		items []delivery
		until time.Time
	}
	batches := make(map[string]pending)
	flush := func(key string) {
		p := batches[key]
		delete(batches, key)
		var group []delivery
		length := len("minicrond alerts\n")
		for _, item := range p.items {
			n := len(formatTelegram(item.alert.Run)) + 2
			if len(group) > 0 && length+n > 3500 {
				d.work <- group
				group = nil
				length = len("minicrond alerts\n")
			}
			group = append(group, item)
			length += n
		}
		if len(group) > 0 {
			d.work <- group
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case item, ok := <-d.queue:
			if !ok {
				for key := range batches {
					flush(key)
				}
				return
			}
			p := batches[item.name]
			if len(p.items) > 0 && p.items[0].channel != item.channel {
				flush(item.name)
				p = pending{}
			}
			if len(p.items) == 0 {
				p.until = time.Now().Add(item.window)
			}
			p.items = append(p.items, item)
			batches[item.name] = p
			if len(p.items) >= 256 {
				flush(item.name)
			}
		case <-ticker.C:
			for key, p := range batches {
				if !time.Now().Before(p.until) {
					flush(key)
				}
			}
		}
	}
}

func (d *Dispatcher) run() {
	for batch := range d.work {
		alert := Alert{Runs: make([]model.Run, 0, len(batch))}
		for _, item := range batch {
			alert.Runs = append(alert.Runs, item.alert.Run)
		}
		var err error
		attempts := 0
		for attempt := range 3 {
			attempts = attempt + 1
			for _, item := range batch {
				d.report(item.alert.Run.ID, item.name, "sending", attempts, "")
			}
			ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
			err = batch[0].channel.Send(ctx, alert)
			cancel()
			if err == nil || d.ctx.Err() != nil {
				break
			}
			if attempt < 2 {
				timer := time.NewTimer(time.Duration(1<<attempt) * time.Second)
				select {
				case <-d.ctx.Done():
					timer.Stop()
				case <-timer.C:
				}
			}
		}
		status, reason := "sent", ""
		if err != nil {
			status, reason = "failed", err.Error()
			slog.Error("sending alert failed after retries", "channel", batch[0].name, "runs", len(batch), "error", err)
		}
		for _, item := range batch {
			d.report(item.alert.Run.ID, item.name, status, attempts, reason)
			d.pending.Add(-1)
		}
	}
}

// QueueDepth includes deliveries waiting for a window, queued for sending, or in flight.
func (d *Dispatcher) QueueDepth() int { return int(d.pending.Load()) }

// Test sends one synthetic message without creating a run or waiting for a batch.
func (d *Dispatcher) Test(ctx context.Context, name string) error {
	d.mu.RLock()
	channel := d.channels[name]
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		return errors.New("alert dispatcher is closed")
	}
	if channel == nil {
		return fmt.Errorf("unknown alert channel %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return channel.Send(ctx, Alert{Run: model.Run{Job: "test", Status: "test", ID: "test"}})
}

// Close drains queued deliveries. Call it only after alert producers stop.
func (d *Dispatcher) Close(ctx context.Context) error {
	defer d.cancel()
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		d.cancel()
		return ctx.Err()
	}
}

func resolveSecret(ref string) (string, error) {
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		value, found := os.LookupEnv(name)
		if !found || value == "" {
			return "", fmt.Errorf("environment variable %s is empty or unset", name)
		}
		return value, nil
	}
	if path, ok := strings.CutPrefix(ref, "file:"); ok {
		value, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if token := strings.TrimSpace(string(value)); token != "" {
			return token, nil
		}
		return "", errors.New("secret file is empty")
	}
	return "", errors.New("must be an env:NAME or file:/absolute/path reference")
}

// telegram is intentionally private: Channel is the stable extension seam;
// provider-specific details stay inside this package.
type telegram struct {
	token               string
	chatID              string
	disableNotification bool
	apiBase             string
	client              *http.Client
}

func newTelegram(token, chatID string, disableNotification bool, apiBase string, client *http.Client) *telegram {
	return &telegram{token: token, chatID: chatID, disableNotification: disableNotification, apiBase: apiBase, client: client}
}

func (t *telegram) Send(ctx context.Context, alert Alert) error {
	form := url.Values{
		"chat_id":              {t.chatID},
		"text":                 {formatAlert(alert)},
		"disable_notification": {fmt.Sprint(t.disableNotification)},
	}
	endpoint := strings.TrimRight(t.apiBase, "/") + "/bot" + t.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build Telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		// net/http errors can contain the request URL, which contains the bot
		// token. Do not propagate the provider error across this trust boundary.
		return errors.New("Telegram request failed")
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return errors.New("reading Telegram response failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Telegram API returned %s", resp.Status)
	}
	if len(body) > 0 {
		var result struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(body, &result); err != nil || !result.OK {
			return errors.New("Telegram API rejected the alert")
		}
	}
	return nil
}

func formatAlert(alert Alert) string {
	if len(alert.Runs) == 0 {
		return formatTelegram(alert.Run)
	}
	var b strings.Builder
	b.WriteString("minicrond alerts\n")
	for i, run := range alert.Runs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimPrefix(formatTelegram(run), "minicrond alert\n"))
	}
	return b.String()
}

func formatTelegram(run model.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "minicrond alert\nDefinition: %s\nStatus: %s\nRun: %s", run.Job, run.Status, run.ID)
	if run.EndReason != "" {
		fmt.Fprintf(&b, "\nReason: %s", run.EndReason)
	}
	if run.ExitCode != nil {
		fmt.Fprintf(&b, "\nExit code: %d", *run.ExitCode)
	}
	if run.StartedAt != nil && run.EndedAt != nil {
		fmt.Fprintf(&b, "\nDuration: %s", run.EndedAt.Sub(*run.StartedAt).Round(time.Millisecond))
	}
	return b.String()
}
