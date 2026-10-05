package config

import (
	"path/filepath"
	"testing"
)

func TestQueueDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Queue{MaxItems: 100, MaxPerJob: 25, MaxBytes: 256, MaxAge: 900, DrainRate: 5, MaxPendingRetries: 500}
	got := cfg.Queue
	if !got.On() || got.MaxItems != want.MaxItems || got.MaxPerJob != want.MaxPerJob || got.MaxBytes != want.MaxBytes || got.MaxAge != want.MaxAge || got.DrainRate != want.DrainRate || got.MaxPendingRetries != want.MaxPendingRetries {
		t.Fatalf("queue defaults = %#v", got)
	}
	mustWrite(t, path, "[queue]\nenabled=false\nmax_items=10\nmax_age=60\n")
	cfg, err = Load(path)
	if err != nil || cfg.Queue.On() || cfg.Queue.MaxItems != 10 || cfg.Queue.MaxPerJob != 10 || cfg.Queue.MaxAge != 60 {
		t.Fatalf("explicit queue = %#v, %v", cfg.Queue, err)
	}
	for _, bad := range []string{
		"max_items=-1", "max_items=10001", "max_items=10\nmax_per_job=11", "max_per_job=-1",
		"max_bytes=-1", "max_bytes=65537", "max_age=-1", "max_age=604801", "max_age='15m'",
		"drain_rate=-1", "drain_rate=1001", "max_pending_retries=-1", "max_pending_retries=100001", "unknown=1",
	} {
		mustWrite(t, path, "[queue]\n"+bad+"\n")
		if _, err := Load(path); err == nil {
			t.Fatalf("expected invalid queue config for %q", bad)
		}
	}
}
