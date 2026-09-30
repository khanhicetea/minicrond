package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestDefinitionRejectsDurationOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("second duration overflow requires 64-bit configuration integers")
	}
	seconds := int((time.Duration(1<<63 - 1)) / time.Second)
	days := int((time.Duration(1<<63 - 1)) / (24 * time.Hour))
	for _, field := range []string{"timeout", "grace", "restart_delay", "healthy_after", "keep_for"} {
		t.Run(field, func(t *testing.T) {
			limit := seconds
			if field == "keep_for" {
				limit = days
			}
			for _, value := range []int{limit, limit + 1} {
				content := fmt.Sprintf("[[job]]\nname='bounded'\ncommand='true'\n%s=%d\n", field, value)
				_, err := ParseImport([]byte(content))
				if value == limit && err != nil {
					t.Fatalf("maximum supported value rejected: %v", err)
				}
				if value > limit && (err == nil || !strings.Contains(err.Error(), field)) {
					t.Fatalf("overflow should identify %s, got %v", field, err)
				}
			}
		})
	}
}

func TestConfigRejectsDurationOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("minute duration overflow requires 64-bit configuration integers")
	}
	for _, test := range []struct {
		section, field string
		unit           time.Duration
	}{
		{"storage", "keep_for_default", 24 * time.Hour},
		{"logs", "worker_flush_interval", time.Minute},
		{"logs", "db_keep_for", 24 * time.Hour},
	} {
		t.Run(test.field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			limit := int((time.Duration(1<<63 - 1)) / test.unit)
			for _, value := range []int{limit, limit + 1} {
				mustWrite(t, path, fmt.Sprintf("[%s]\n%s=%d\n", test.section, test.field, value))
				_, err := Load(path)
				if value == limit && err != nil {
					t.Fatalf("maximum supported value rejected: %v", err)
				}
				if value > limit && (err == nil || !strings.Contains(err.Error(), test.field)) {
					t.Fatalf("overflow should identify %s, got %v", test.field, err)
				}
			}
		})
	}
}

func TestDefinitionsAndDefaultsOwnMutableFields(t *testing.T) {
	enabled := true
	defaults := model.Definition{Env: map[string]string{"MODE": "original"}, SuccessCodes: []int{0}, Enabled: &enabled}
	first := model.Definition{Name: "first", Command: "true"}
	second := model.Definition{Name: "second", Command: "true"}
	if err := ValidateDefinition(&first, defaults); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDefinition(&second, defaults); err != nil {
		t.Fatal(err)
	}
	first.Env["MODE"] = "changed"
	first.SuccessCodes[0] = 1
	*first.Enabled = false
	if defaults.Env["MODE"] != "original" || second.Env["MODE"] != "original" || defaults.SuccessCodes[0] != 0 ||
		second.SuccessCodes[0] != 0 || !*defaults.Enabled || !*second.Enabled {
		t.Fatal("definitions share mutable defaults")
	}
	cfg := Config{Jobs: []model.Definition{second}}
	snapshot := cfg.Definitions()
	snapshot[0].Env["MODE"] = "changed"
	snapshot[0].SuccessCodes[0] = 1
	*snapshot[0].Enabled = false
	if cfg.Jobs[0].Env["MODE"] != "original" || cfg.Jobs[0].SuccessCodes[0] != 0 || !*cfg.Jobs[0].Enabled {
		t.Fatal("definition snapshot shares mutable configuration")
	}
}
