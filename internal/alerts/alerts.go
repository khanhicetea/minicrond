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
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/fault"
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
		case d.queue <- delivery{channel: channel, alert: Alert{Run: run.Clone()}, name: name, window: d.windows[name]}:
		default:
			d.pending.Add(-1)
			d.report(run.ID, name, "dropped", 0, "alert queue is full")
		}
	}
}

func (d *Dispatcher) report(runID, name, status string, attempts int, reason string) {
	if d.record == nil || d.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	if err := fault.Call(func() error { return d.record(ctx, runID, name, status, attempts, reason) }); err != nil && d.ctx.Err() == nil {
		logFailure("recording alert delivery failed", err, "channel", name, "run", runID)
	}
}

func logFailure(message string, err error, attrs ...any) {
	attrs = append(attrs, "error", err)
	var panicErr *fault.PanicError
	if errors.As(err, &panicErr) {
		attrs = append(attrs, "stack", string(panicErr.Stack))
	}
	slog.Error(message, attrs...)
}

// Non-comparable extension values cannot be safely tested for identity. Keep
// their deliveries separate rather than risking a panic or mixed credentials.
func sameChannel(a, b Channel) bool {
	left, right := reflect.ValueOf(a), reflect.ValueOf(b)
	return left.IsValid() && right.IsValid() && left.Type() == right.Type() && left.Comparable() && right.Comparable() && a == b
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
	enqueue := func(group []delivery) {
		select {
		case d.work <- group:
		case <-d.ctx.Done():
			d.pending.Add(-int64(len(group)))
		}
	}
	flush := func(key string) {
		p := batches[key]
		delete(batches, key)
		var group []delivery
		length := len("minicrond alerts\n")
		for _, item := range p.items {
			n := len(formatTelegram(item.alert.Run)) + 2
			if len(group) > 0 && length+n > 3500 {
				enqueue(group)
				group = nil
				length = len("minicrond alerts\n")
			}
			group = append(group, item)
			length += n
		}
		if len(group) > 0 {
			enqueue(group)
		}
	}
	var timer *time.Timer
	var deadline <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	resetDeadline := func() {
		var next time.Time
		for _, p := range batches {
			if next.IsZero() || p.until.Before(next) {
				next = p.until
			}
		}
		if next.IsZero() {
			if timer != nil {
				timer.Stop()
			}
			deadline = nil
			return
		}
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		if timer == nil {
			timer = time.NewTimer(wait)
		} else {
			timer.Reset(wait)
		}
		deadline = timer.C
	}
	for {
		select {
		case <-d.ctx.Done():
			for _, batch := range batches {
				d.pending.Add(-int64(len(batch.items)))
			}
			// Close closes the producer queue before waiting for batch exit.
			for range d.queue {
				d.pending.Add(-1)
			}
			return
		case item, ok := <-d.queue:
			if !ok {
				for key := range batches {
					flush(key)
				}
				return
			}
			p := batches[item.name]
			if len(p.items) > 0 && !time.Now().Before(p.until) {
				flush(item.name)
				p = pending{}
			}
			if len(p.items) > 0 && !sameChannel(p.items[0].channel, item.channel) {
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
			resetDeadline()
		case <-deadline:
			now := time.Now()
			for key, p := range batches {
				if !now.Before(p.until) {
					flush(key)
				}
			}
			resetDeadline()
		}
	}
}

func (d *Dispatcher) run() {
	for batch := range d.work {
		if d.ctx.Err() != nil {
			d.pending.Add(-int64(len(batch)))
			continue
		}
		alert := Alert{Runs: make([]model.Run, 0, len(batch))}
		for _, item := range batch {
			alert.Runs = append(alert.Runs, item.alert.Run)
		}
		var err error
		attempts := 0
		for attempt := range 3 {
			if d.ctx.Err() != nil {
				err = d.ctx.Err()
				break
			}
			attempts = attempt + 1
			for _, item := range batch {
				d.report(item.alert.Run.ID, item.name, "sending", attempts, "")
			}
			if d.ctx.Err() != nil {
				err = d.ctx.Err()
				break
			}
			ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
			err = fault.Call(func() error { return batch[0].channel.Send(ctx, alert) })
			cancel()
			var panicErr *fault.PanicError
			if err == nil || d.ctx.Err() != nil || errors.As(err, &panicErr) {
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
			if d.ctx.Err() == nil {
				logFailure("sending alert failed", err, "channel", batch[0].name, "runs", len(batch), "attempts", attempts)
			}
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
	return fault.Call(func() error { return channel.Send(ctx, Alert{Run: model.Run{Job: "test", Status: "test", ID: "test"}}) })
}

// Close drains queued deliveries. Call it only after alert producers stop.
func (d *Dispatcher) Close(ctx context.Context) error {
	defer d.cancel()
	// Notify holds a read lock while recording the queued delivery. Cancel
	// that operation even if Close is still waiting for the producer lock.
	stopCancellation := context.AfterFunc(ctx, d.cancel)
	defer stopCancellation()
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
