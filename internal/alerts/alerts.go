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
	Run model.Run
}

type delivery struct {
	channel Channel
	alert   Alert
	name    string
}

// Dispatcher asynchronously delivers alerts. Its channel registry can be
// replaced during a configuration reload without interrupting deliveries
// already in progress.
type Dispatcher struct {
	mu       sync.RWMutex
	channels map[string]Channel
	queue    chan delivery
	closed   bool
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

// New creates a dispatcher and validates/resolves channel credentials.
func New(channels []config.AlertChannel) (*Dispatcher, error) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{channels: make(map[string]Channel), queue: make(chan delivery, 256), ctx: ctx, cancel: cancel}
	if err := d.Reload(channels); err != nil {
		return nil, err
	}
	for range 4 {
		d.wg.Add(1)
		go d.run()
	}
	return d, nil
}

// Reload atomically replaces the available delivery channels.
func (d *Dispatcher) Reload(configs []config.AlertChannel) error {
	channels := make(map[string]Channel, len(configs))
	for _, cfg := range configs {
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
			slog.Error("alert channel is unavailable", "channel", name, "run", run.ID)
			continue
		}
		select {
		case d.queue <- delivery{channel: channel, alert: Alert{Run: run}, name: name}:
		default:
			slog.Error("alert queue is full; dropping delivery", "channel", name, "run", run.ID)
		}
	}
}

func (d *Dispatcher) run() {
	defer d.wg.Done()
	for item := range d.queue {
		var err error
		for attempt := range 3 {
			ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
			err = item.channel.Send(ctx, item.alert)
			cancel()
			if err == nil || d.ctx.Err() != nil {
				break
			}
			timer := time.NewTimer(time.Duration(1<<attempt) * time.Second)
			select {
			case <-d.ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		if err != nil {
			slog.Error("sending alert failed after retries", "channel", item.name, "run", item.alert.Run.ID, "error", err)
		}
	}
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
		"text":                 {formatTelegram(alert.Run)},
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
