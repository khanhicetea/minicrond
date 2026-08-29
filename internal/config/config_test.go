package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/minicron/minicron/internal/model"
)

func TestLoadStrictAtomicSet(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "minicron.toml"), "[include]\npaths=['*.job.toml']\n[scheduler]\ntimezone='UTC'\n")
	mustWrite(t, filepath.Join(dir, "a.job.toml"), "[[job]]\nname='a'\nargv=['echo','a']\nschedule='@every 1s'\n")
	mustWrite(t, filepath.Join(dir, "b.job.toml"), "[[job]]\nname='b'\ncommand='echo b'\nschedule='0 * * * *'\n")
	cfg, err := Load(filepath.Join(dir, "minicron.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Definitions) != 2 {
		t.Fatalf("got %d definitions", len(cfg.Definitions))
	}
	if cfg.Definitions[0].EnvBase != "clean" {
		t.Fatal("clean environment must be the default")
	}
}

func TestLoadRejectsUnknownAndDuplicate(t *testing.T) {
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "minicron.toml")
	mustWrite(t, bootstrap, "unknown=1\n")
	if _, err := Load(bootstrap); err == nil {
		t.Fatal("unknown key accepted")
	}
	mustWrite(t, bootstrap, "[[job]]\nname='same'\ncommand='true'\n[[worker]]\nname='same'\ncommand='true'\n")
	_, err := Load(bootstrap)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestScheduleValidation(t *testing.T) {
	for _, valid := range []string{"0 2 * * *", "@daily", "@every 1s"} {
		if err := ValidateSchedule(valid); err != nil {
			t.Errorf("%s: %v", valid, err)
		}
	}
	for _, invalid := range []string{"* * * * * *", "@every 500ms"} {
		if err := ValidateSchedule(invalid); err == nil {
			t.Errorf("accepted %s", invalid)
		}
	}
}

func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Export bundles must re-validate into identical canonical definitions, so a
// round trip loses nothing (reconstruction guarantee for export/import).
func TestExportTOMLRoundTrip(t *testing.T) {
	source := model.Definition{Name: "backup", Kind: "job", Command: "echo hi", Shell: "/bin/sh",
		Schedule: "0 2 * * *", Timezone: "UTC", CatchUp: "none", OnOverlap: "skip", EnvBase: "clean",
		Timeout: "0", Grace: "10s", StopSignal: "SIGTERM", SuccessCodes: []int{0}, Restart: "always",
		RestartDelay: "5s", MaxRestartAttempts: 5, HealthyAfter: "30s", LogOnFull: "drop_old"}
	if err := ValidateDefinition(&source); err != nil {
		t.Fatal(err)
	}
	bundle := struct {
		Jobs    []model.Definition `toml:"job"`
		Workers []model.Definition `toml:"worker"`
	}{Jobs: []model.Definition{source}}
	body, err := toml.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseImport(body)
	if err != nil {
		t.Fatalf("exported bundle does not re-validate: %v\n%s", err, body)
	}
	if len(parsed) != 1 {
		t.Fatalf("parsed %d definitions", len(parsed))
	}
	_, wantHash, err := Canonical(source)
	if err != nil {
		t.Fatal(err)
	}
	_, gotHash, err := Canonical(parsed[0])
	if err != nil {
		t.Fatal(err)
	}
	if wantHash != gotHash {
		t.Fatalf("canonical hash changed across round trip:\n%s", body)
	}
}

func TestLogArchiveDefaults(t *testing.T) {
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "minicron.toml")
	mustWrite(t, bootstrap, "[logs]\nbackend='file'\n")
	cfg, err := Load(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.WorkerFlushInterval != "15m" || cfg.Logs.DBKeepFor != "720h" || cfg.Logs.DBPruneAt != "03:30" {
		t.Fatalf("log archive defaults = %#v", cfg.Logs)
	}
}

func TestLogArchiveValidation(t *testing.T) {
	for _, invalid := range []string{
		"[logs]\nworker_flush_interval='500ms'\n",
		"[logs]\nworker_flush_interval='nope'\n",
		"[logs]\ndb_keep_for='0h'\n",
		"[logs]\ndb_keep_for='x'\n",
		"[logs]\ndb_prune_at='25:00'\n",
		"[logs]\ndb_prune_at='3:5'\n",
		"[logs]\ndb_prune_at='noon'\n",
	} {
		dir := t.TempDir()
		bootstrap := filepath.Join(dir, "minicron.toml")
		mustWrite(t, bootstrap, invalid)
		if _, err := Load(bootstrap); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "minicron.toml")
	mustWrite(t, bootstrap, "[logs]\nworker_flush_interval='5m'\ndb_keep_for='168h'\ndb_prune_at='23:45'\n")
	if _, err := Load(bootstrap); err != nil {
		t.Fatal(err)
	}
}

// The [defaults] table must reach every field the executor actually consumes;
// restart/health/log-policy values used to be silently hardcoded.
func TestDefaultsCoverRestartHealthAndLogPolicy(t *testing.T) {
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "minicron.toml")
	mustWrite(t, bootstrap, `[defaults]
restart = "on-failure"
restart_delay = "77ms"
max_restart_attempts = 9
healthy_after = "88ms"
log_on_full = "drop_new"
stop_signal = "SIGINT"

[[job]]
name = "j"
command = "true"
schedule = "@every 1h"
`)
	cfg, err := Load(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Definitions[0]
	if d.Restart != "on-failure" || d.RestartDelay != "77ms" || d.MaxRestartAttempts != 9 ||
		d.HealthyAfter != "88ms" || d.LogOnFull != "drop_new" || d.StopSignal != "SIGINT" {
		t.Fatalf("defaults not applied: %#v", d)
	}
}

// Include-file defaults overlay the bootstrap defaults; fields set in neither
// place keep their built-in fallbacks.
func TestIncludeDefaultsOverlayBootstrapDefaults(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "minicron.toml"), `[defaults]
grace = "30s"
healthy_after = "1h"

[include]
paths = ["inc.toml"]
`)
	mustWrite(t, filepath.Join(dir, "inc.toml"), `[defaults]
healthy_after = "2h"

[[job]]
name = "j"
command = "true"
schedule = "@every 1h"
`)
	cfg, err := Load(filepath.Join(dir, "minicron.toml"))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Definitions[0]
	if d.Grace != "30s" {
		t.Fatalf("bootstrap default lost: grace = %q", d.Grace)
	}
	if d.HealthyAfter != "2h" {
		t.Fatalf("include default must win: healthy_after = %q", d.HealthyAfter)
	}
	if d.RestartDelay != "5s" {
		t.Fatalf("builtin fallback lost: restart_delay = %q", d.RestartDelay)
	}
}

func TestDefinitionFieldValidation(t *testing.T) {
	for _, invalid := range []string{
		"log_on_full = 'recycle'",
		"stop_signal = 'SIGWAT'",
		"keep_for = 'soon'",
		"keep_for = '0h'",
		"log_max = '10Ki'",
	} {
		dir := t.TempDir()
		bootstrap := filepath.Join(dir, "minicron.toml")
		mustWrite(t, bootstrap, "[[job]]\nname='j'\ncommand='true'\nschedule='@every 1h'\n"+invalid+"\n")
		if _, err := Load(bootstrap); err == nil {
			t.Errorf("accepted %s", invalid)
		}
	}
}
