package config

import (
	"path/filepath"
	"testing"
)

func TestReadsDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "")
	cfg, err := Load(path)
	if err != nil || cfg.Reads != (Reads{Slots: 4, Budget: 32, WorkTimeout: 20}) {
		t.Fatalf("reads defaults: %#v, error %v", cfg.Reads, err)
	}
	mustWrite(t, path, "[reads]\nslots=8\nbudget=128\nwork_timeout=60\n")
	cfg, err = Load(path)
	if err != nil || cfg.Reads != (Reads{Slots: 8, Budget: 128, WorkTimeout: 60}) {
		t.Fatalf("explicit reads: %#v, error %v", cfg.Reads, err)
	}
	for _, reads := range []string{"slots=-1", "slots=65", "budget=7", "budget=4097", "work_timeout=-1", "work_timeout=301", "slots='4'", "unknown=1"} {
		mustWrite(t, path, "[reads]\n"+reads+"\n")
		if _, err := Load(path); err == nil {
			t.Fatalf("expected invalid reads config for %s", reads)
		}
	}
}
