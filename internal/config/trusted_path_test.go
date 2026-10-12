package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustedPathBootstrapValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	for _, tt := range []struct {
		input string
		valid bool
	}{
		{"", true},
		{"[scheduler]\ntrusted_path=['/usr/local/bin','/usr/bin','/bin']\n", true},
		{"[scheduler]\ntrusted_path=[]\n", false},
		{"[scheduler]\ntrusted_path=['bin']\n", false},
		{"[scheduler]\ntrusted_path=['/tmp']\n", false},
		{"[scheduler]\ntrusted_path=['/minicrond-missing-directory']\n", false},
	} {
		mustWrite(t, path, tt.input)
		cfg, err := Load(path)
		if (err == nil) != tt.valid {
			t.Fatalf("%q: %v", tt.input, err)
		}
		if err != nil && !strings.Contains(err.Error(), "scheduler.trusted_path") {
			t.Fatalf("unhelpful error: %v", err)
		}
		if err == nil && tt.input == "" && cfg.Scheduler.TrustedPath != nil {
			t.Fatal("standalone lookup policy changed")
		}
		if err == nil && tt.input != "" && cfg.Scheduler.TrustedPath[0] != "/usr/local/bin" {
			t.Fatal("search order lost")
		}
	}
}
