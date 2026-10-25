package alerts

import (
	"slices"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestNotifyOnFiltersStatusesPerChannel(t *testing.T) {
	configs := []model.AlertChannel{
		{Name: "failures", Type: "telegram", BotToken: "123:token", ChatID: "1"},
		{Name: "backups", Type: "telegram", BotToken: "123:token", ChatID: "2", NotifyOn: []string{"skipped", "succeeded"}},
	}
	registry, err := Prepare(configs)
	if err != nil {
		t.Fatal(err)
	}
	failures := &captureChannel{messages: make(chan Alert, 4)}
	backups := &captureChannel{messages: make(chan Alert, 4)}
	registry.channels = map[string]Channel{"failures": failures, "backups": backups}
	registry.windows = map[string]time.Duration{"failures": time.Hour, "backups": time.Hour}
	d, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Apply(registry); err != nil {
		t.Fatal(err)
	}
	both := model.Definition{Alerts: []string{"failures", "backups"}}
	for _, status := range []string{"succeeded", "failed", "timeout", "skipped", "missed", "stopped"} {
		d.Notify(model.Run{ID: status, Status: status}, both)
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	ids := func(c *captureChannel) []string {
		var out []string
		for len(c.messages) > 0 {
			for _, run := range (<-c.messages).Runs {
				out = append(out, run.ID)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := ids(failures); !slices.Equal(got, []string{"failed", "timeout"}) {
		t.Fatalf("default channel delivered %v", got)
	}
	if got := ids(backups); !slices.Equal(got, []string{"skipped", "succeeded"}) {
		t.Fatalf("notify_on channel delivered %v", got)
	}
}

func TestNotifyOnValidation(t *testing.T) {
	c := model.AlertChannel{Name: "ops", Type: "telegram", BotToken: "123:token", ChatID: "1"}
	if err := c.Normalize(); err != nil || !slices.Equal(c.NotifyOn, model.DefaultNotifyOn) {
		t.Fatalf("default = %v, %v", c.NotifyOn, err)
	}
	c.NotifyOn = []string{"skipped", "succeeded", "skipped"}
	if err := c.Normalize(); err != nil || !slices.Equal(c.NotifyOn, []string{"succeeded", "skipped"}) {
		t.Fatalf("canonical = %v, %v", c.NotifyOn, err)
	}
	c.NotifyOn = []string{"stopped"}
	if err := c.Normalize(); err == nil {
		t.Fatal("an unknown status must be rejected")
	}
}
