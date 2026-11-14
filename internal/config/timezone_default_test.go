package config

import (
	"path/filepath"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func loadForTimezone(t *testing.T, content string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, content)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Definitions created at runtime resolve their timezone like config ones:
// own value, then [defaults].timezone, then scheduler.timezone, then UTC.
func TestEffectiveJobDefaultsTimezone(t *testing.T) {
	for _, tc := range []struct{ name, config, want string }{
		{"scheduler", "[scheduler]\ntimezone='Asia/Ho_Chi_Minh'\n", "Asia/Ho_Chi_Minh"},
		{"defaults beat scheduler", "[scheduler]\ntimezone='Asia/Ho_Chi_Minh'\n[defaults]\ntimezone='Europe/Berlin'\n", "Europe/Berlin"},
		{"nothing configured", "[server]\nunix_socket=true\n", "UTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadForTimezone(t, tc.config).EffectiveJobDefaults().Timezone; got != tc.want {
				t.Fatalf("timezone = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImportAndAPIInputDefaultToSchedulerTimezone(t *testing.T) {
	defaults := loadForTimezone(t, "[scheduler]\ntimezone='Asia/Ho_Chi_Minh'\n").EffectiveJobDefaults()

	t.Run("import", func(t *testing.T) {
		content := "[[job]]\nname='plain'\ncommand='true'\nschedule='0 9 * * *'\n" +
			"[[job]]\nname='own'\ncommand='true'\nschedule='0 9 * * *'\ntimezone='America/New_York'\n" +
			"[[worker]]\nname='w'\nargv=['sleep','10']\n"
		defs, err := ParseImport([]byte(content), defaults)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"plain": "Asia/Ho_Chi_Minh", "own": "America/New_York", "w": "Asia/Ho_Chi_Minh"}
		for _, d := range defs {
			if d.Timezone != want[d.Name] {
				t.Errorf("%s timezone = %q, want %q", d.Name, d.Timezone, want[d.Name])
			}
		}
	})

	t.Run("import bundle defaults beat the scheduler", func(t *testing.T) {
		content := "[defaults]\ntimezone='Europe/Berlin'\n[[job]]\nname='plain'\ncommand='true'\nschedule='0 9 * * *'\n"
		defs, err := ParseImport([]byte(content), defaults)
		if err != nil {
			t.Fatal(err)
		}
		if defs[0].Timezone != "Europe/Berlin" {
			t.Fatalf("timezone = %q", defs[0].Timezone)
		}
	})

	t.Run("api input", func(t *testing.T) {
		in := DefinitionInput{Definition: model.Definition{Name: "plain", Kind: model.KindJob, Command: "true", Schedule: "0 9 * * *"}}
		d, err := ValidateDefinitionInput(in, defaults)
		if err != nil || d.Timezone != "Asia/Ho_Chi_Minh" {
			t.Fatalf("timezone = %q, err = %v", d.Timezone, err)
		}
		in.Timezone = "Europe/Berlin"
		d, err = ValidateDefinitionInput(in, defaults)
		if err != nil || d.Timezone != "Europe/Berlin" {
			t.Fatalf("explicit timezone = %q, err = %v", d.Timezone, err)
		}
	})

	t.Run("no defaults falls back to UTC", func(t *testing.T) {
		defs, err := ParseImport([]byte("[[job]]\nname='plain'\ncommand='true'\nschedule='0 9 * * *'\n"))
		if err != nil || defs[0].Timezone != "UTC" {
			t.Fatalf("timezone = %q, err = %v", defs[0].Timezone, err)
		}
	})
}
