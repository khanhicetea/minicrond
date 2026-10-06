package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func TestDatabaseChannelLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minicron.toml")
	if err := os.WriteFile(path, []byte("[server]\ntcp_enabled=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	start := func() (*Daemon, func()) {
		ctx, cancel := context.WithCancel(t.Context())
		d := &Daemon{ConfigPath: path, DataDir: filepath.Join(dir, "data")}
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		stop := sync.OnceFunc(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(15 * time.Second):
				t.Error("daemon shutdown timed out")
			}
		})
		t.Cleanup(stop)
		waitDaemonReady(t, d)
		// The initial readiness helper sees running early; readyConfig checks all
		// components, including the dispatcher, before mutations proceed.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := d.readyConfig(); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("registry not ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
		return d, stop
	}
	d, stop := start()
	c := model.AlertChannel{Name: "ops", Type: "telegram", BotToken: "123:private_token", ChatID: "-123"}
	if err := d.SaveAlertChannel(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	old := d.alerts.Load()
	if got := d.currentAlertChannels(); len(got) != 1 || got[0].BotToken != c.BotToken {
		t.Fatal("save did not load memory")
	}
	if err := d.SaveAlertChannel(t.Context(), model.AlertChannel{Name: "other", Type: "telegram", BotToken: "456:other_token", ChatID: "-456"}); err != nil {
		t.Fatal(err)
	}
	if len(d.currentAlertChannels()) != 2 || old != d.alerts.Load() {
		t.Fatal("registry reload replaced dispatcher or lost other channel")
	}
	c.BotToken, c.ChatID = "", "-789"
	if err := d.SaveAlertChannel(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err := d.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop()
	d, _ = start()
	if got := d.currentAlertChannels(); len(got) != 2 || got[0].Name != "ops" || got[0].BotToken != "123:private_token" || got[0].ChatID != "-789" {
		t.Fatal("restart did not restore database registry")
	}
	job := model.Definition{Name: "job", Kind: model.KindJob, Command: "true", Alerts: []string{"ops"}}
	if _, err := d.store.PutDefinition(t.Context(), job, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteAlertChannel(t.Context(), "ops", false); !errors.Is(err, store.ErrAlertChannelReferenced) {
		t.Fatalf("delete = %v", err)
	}
	if len(d.currentAlertChannels()) != 2 {
		t.Fatal("failed delete altered memory")
	}
	if err := d.DeleteAlertChannel(t.Context(), "ops", true); err != nil {
		t.Fatal(err)
	}
	if got := d.currentAlertChannels(); len(got) != 1 || got[0].Name != "other" {
		t.Fatal("delete did not reload all channels")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.SaveAlertChannel(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled save = %v", err)
	}
	if len(d.currentAlertChannels()) != 1 {
		t.Fatal("cancelled save changed memory")
	}
}
