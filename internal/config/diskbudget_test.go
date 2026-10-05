package config

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ADR-10: the headroom rule is on by default; the byte budget and the
// quarantine purge are opt-in.
func TestDiskBudgetDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.Logs
	if l.DiskBudgetBytes() != 0 || l.DiskMinFreeBytes() != 512<<20 || l.QuarantineMaxBytes() != 0 || l.QuarantineKeepDuration() != 0 {
		t.Fatalf("defaults: budget=%d minfree=%d qmax=%d qkeep=%v", l.DiskBudgetBytes(), l.DiskMinFreeBytes(), l.QuarantineMaxBytes(), l.QuarantineKeepDuration())
	}
	if l.DiskMinFree == nil || *l.DiskMinFree != DefaultDiskMinFreeMiB {
		t.Fatalf("default disk_min_free = %v", l.DiskMinFree)
	}
}

func TestDiskBudgetExplicitSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "[logs]\ndisk_budget=2048\ndisk_min_free=1024\nquarantine_keep_for=14\nquarantine_max_size=256\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.Logs
	if l.DiskBudgetBytes() != 2048<<20 || l.DiskMinFreeBytes() != 1024<<20 || l.QuarantineMaxBytes() != 256<<20 || l.QuarantineKeepDuration() != 14*24*time.Hour {
		t.Fatalf("explicit settings: %+v", l)
	}
}

// An explicit zero disables the headroom rule instead of selecting the default
// (the A12 pattern: omitted and zero are different).
func TestDiskMinFreeExplicitZeroDisablesHeadroom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "[logs]\ndisk_min_free=0\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.DiskMinFree == nil || cfg.Logs.DiskMinFreeBytes() != 0 {
		t.Fatalf("explicit zero was replaced: %v", cfg.Logs.DiskMinFree)
	}
}

func TestDiskBudgetValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	for _, logs := range []string{
		"disk_budget=-1", "disk_budget=1073741825", "disk_budget='5GiB'",
		"disk_min_free=-1", "disk_min_free=1073741825", "disk_min_free='512MiB'",
		"quarantine_keep_for=-1", "quarantine_keep_for='30d'", "quarantine_keep_for=9223372036854775807",
		"quarantine_max_size=-1", "quarantine_max_size=1073741825",
	} {
		mustWrite(t, path, "[logs]\n"+logs+"\n")
		if _, err := Load(path); err == nil {
			t.Errorf("expected invalid logs config for %s", logs)
		}
	}
	// The largest values still convert without wrapping.
	mustWrite(t, path, "[logs]\ndisk_budget=1073741824\ndisk_min_free=1073741824\nquarantine_max_size=1073741824\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.DiskBudgetBytes() != 1<<50 || cfg.Logs.DiskMinFreeBytes() != 1<<50 || cfg.Logs.QuarantineMaxBytes() != 1<<50 {
		t.Fatalf("largest values: %+v", cfg.Logs)
	}
	if got := (Logs{DiskBudget: math.MaxInt}).DiskBudgetBytes(); got != math.MaxInt64 {
		t.Fatalf("out-of-range value wrapped to %d", got)
	}
}

// The schema served to the UI editor documents the same defaults and limits as
// the code, and both embedded copies are identical.
func TestSchemaDocumentsDiskBudgetSettings(t *testing.T) {
	a, err := os.ReadFile("../../schema/minicron.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../cmd/minicrond/minicron.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("the two copies of minicron.schema.json differ")
	}
	var schema struct {
		Properties struct {
			Logs struct {
				Properties map[string]struct {
					Default json.RawMessage `json:"default"`
					Maximum json.RawMessage `json:"maximum"`
				} `json:"properties"`
			} `json:"logs"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(a, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema.Properties.Logs.Properties
	for name, want := range map[string]int{"disk_min_free": DefaultDiskMinFreeMiB, "disk_budget": 0, "quarantine_keep_for": 0, "quarantine_max_size": 0} {
		p, ok := props[name]
		if !ok || string(p.Default) != strconv.Itoa(want) {
			t.Errorf("schema logs.%s default = %s, want %d", name, p.Default, want)
		}
	}
	for _, name := range []string{"disk_min_free", "disk_budget", "quarantine_max_size"} {
		if p := props[name]; string(p.Maximum) != strconv.FormatInt(MaxDBMaxSizeMiB, 10) {
			t.Errorf("schema logs.%s maximum = %s", name, p.Maximum)
		}
	}
}
