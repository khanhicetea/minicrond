package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestLoadSettingsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "[server]\nbind='127.0.0.1:9000'\n[scheduler]\ntimezone='UTC'\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Bind != "127.0.0.1:9000" || cfg.Scheduler.MaxConcurrentRuns != 32 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestUnixOnlyConfigRequiresSocketAndIgnoresUnusedBind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "[server]\ntcp_enabled=false\nbind='not a TCP address'\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TCPOn() || !cfg.Server.UnixOn() {
		t.Fatalf("unexpected listeners: %+v", cfg.Server)
	}
	mustWrite(t, path, "[server]\ntcp_enabled=false\nunix_socket=false\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "cannot both be false") {
		t.Fatalf("expected no-listener error, got %v", err)
	}
	mustWrite(t, path, "[server]\ntcp_enabled=true\nbind='not a TCP address'\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "server.bind") {
		t.Fatalf("expected bind error, got %v", err)
	}
}

func TestLoadRejectsDefinitionSources(t *testing.T) {
	for name, content := range map[string]string{
		"include": "[include]\npaths=['jobs/*.toml']\n",
		"job":     "[[job]]\nname='hello'\ncommand='true'\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "minicron.toml")
			mustWrite(t, path, content)
			if _, err := Load(path); err == nil {
				t.Fatal("expected settings parser to reject definition source")
			}
		})
	}
}

func TestLoadJobDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "[defaults]\nshell='/bin/bash'\nretries=3\nretry_delay=12\n[defaults.env]\nFOO='bar'\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	job := model.Definition{Name: "job", Command: "true"}
	if err := ValidateDefinition(&job, cfg.Defaults); err != nil {
		t.Fatal(err)
	}
	if job.Shell != "/bin/bash" || job.Retries != 3 || job.RetryDelay != 12 || job.Env["FOO"] != "bar" {
		t.Fatalf("job defaults not applied: %#v", job)
	}
	worker := model.Definition{Name: "worker", Kind: model.KindWorker, Command: "true"}
	if err := ValidateDefinition(&worker, cfg.Defaults); err != nil {
		t.Fatal(err)
	}
	if worker.Shell != "/bin/sh" || worker.Retries != 0 {
		t.Fatalf("worker got job defaults: %#v", worker)
	}
	for _, invalid := range []string{"[defaults]\nretries=-1", "[defaults]\ncommand='true'", "[defaults]\nunknown=1"} {
		mustWrite(t, path, invalid)
		if _, err := Load(path); err == nil {
			t.Errorf("expected invalid defaults: %s", invalid)
		}
	}
}

func TestParseImportGlobalDefaults(t *testing.T) {
	global := model.Definition{Shell: "/bin/bash", Grace: 40, RetryDelay: 15}
	content := "[defaults]\nshell='/bin/zsh'\n[[job]]\nname='first'\ncommand='true'\n[[job]]\nname='second'\ncommand='true'\ngrace=3\n[[worker]]\nname='worker'\ncommand='true'\n"
	defs, err := ParseImport([]byte(content), global)
	if err != nil {
		t.Fatal(err)
	}
	if defs[0].Shell != "/bin/zsh" || defs[0].Grace != 40 || defs[0].RetryDelay != 15 || defs[1].Grace != 3 || defs[2].Grace != 10 {
		t.Fatalf("unexpected merged defaults: %#v", defs)
	}
}

func TestParseImportAppliesDefaults(t *testing.T) {
	defs, err := ParseImport([]byte("[defaults]\nshell='/bin/bash'\n\n[[job]]\nname='hello'\ncommand='echo hi'\n\n[[worker]]\nname='worker'\nargv=['sleep','10']\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].Kind != model.KindJob || defs[1].Kind != model.KindWorker {
		t.Fatalf("unexpected definitions: %#v", defs)
	}
	if defs[0].Shell != "/bin/bash" || defs[1].Shell != "/bin/bash" {
		t.Fatalf("defaults not applied: %#v", defs)
	}
}

func TestParseImportStrictAndAtomic(t *testing.T) {
	cases := []string{
		"unknown=true\n",
		"[[job]]\nname='same'\ncommand='true'\n[[worker]]\nname='same'\ncommand='true'\n",
		"[[job]]\nname='bad name'\ncommand='true'\n",
	}
	for _, content := range cases {
		if _, err := ParseImport([]byte(content)); err == nil {
			t.Fatalf("expected error for %q", content)
		}
	}
}

func TestLogArchiveDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.WorkerFlushInterval != 15 || cfg.Logs.DBKeepFor != 30 || cfg.Logs.DBPruneAt != "03:30" {
		t.Fatalf("unexpected log defaults: %#v", cfg.Logs)
	}
	for _, logs := range []string{"worker_flush_interval=-1", "worker_flush_interval='15m'", "db_keep_for=-1", "db_keep_for='720h'", "db_prune_at='25:00'"} {
		mustWrite(t, path, "[logs]\n"+logs+"\n")
		if _, err := Load(path); err == nil {
			t.Fatalf("expected invalid logs config for %s", logs)
		}
	}
}

func TestTelegramAlertChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "[[alert_channel]]\nname='ops'\ntype='telegram'\nbot_token='env:BOT_TOKEN'\nchat_id='123'\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AlertChannels) != 1 || cfg.AlertChannels[0].Name != "ops" || cfg.AlertChannels[0].BatchWindow != 10 {
		t.Fatalf("unexpected channels: %#v", cfg.AlertChannels)
	}
	for _, window := range []string{"-1", "3601", "'10s'"} {
		mustWrite(t, path, "[[alert_channel]]\nname='ops'\ntype='telegram'\nbot_token='env:BOT_TOKEN'\nchat_id='123'\nbatch_window="+window+"\n")
		if _, err := Load(path); err == nil {
			t.Errorf("expected invalid batch_window %q", window)
		}
	}
}

func TestSizeUnits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, "[logs]\nmax_line=256\n")
	cfg, err := Load(path)
	if err != nil || cfg.Logs.MaxLine != 256 {
		t.Fatalf("logs.max_line = %d, %v", cfg.Logs.MaxLine, err)
	}
	for _, value := range []string{"-1", "16385", "'256KiB'"} {
		mustWrite(t, path, "[logs]\nmax_line="+value+"\n")
		if _, err := Load(path); err == nil {
			t.Errorf("expected invalid max_line %s", value)
		}
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"0", true}, {"1", true}, {"1048576", true}, {"-1", false}, {"1048577", false}, {"'10MiB'", false},
	} {
		_, err := ParseImport([]byte("[[job]]\nname='job'\ncommand='true'\nlog_max=" + tc.value + "\n"))
		if (err == nil) != tc.valid {
			t.Errorf("log_max=%s: error = %v, valid = %v", tc.value, err, tc.valid)
		}
	}
}

func TestParseImportRejectsDurationStrings(t *testing.T) {
	_, err := ParseImport([]byte("[[job]]\nname='legacy'\ncommand='true'\ntimeout='10s'\n"))
	if err == nil {
		t.Fatal("expected legacy duration string to be rejected")
	}
}

func TestValidateDefinition(t *testing.T) {
	d := model.Definition{Name: "hello", Command: "true"}
	if err := ValidateDefinition(&d); err != nil {
		t.Fatal(err)
	}
	if d.Kind != model.KindJob || d.Shell != "/bin/sh" || d.Timezone != "UTC" {
		t.Fatalf("defaults not applied: %#v", d)
	}
	bad := d
	bad.Command = ""
	bad.Argv = nil
	if err := ValidateDefinition(&bad); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("unexpected validation result: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
