package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigTOMLUsesBind(t *testing.T) {
	got := defaultConfigTOML("127.0.0.1:7500")
	if !strings.Contains(got, `bind = "127.0.0.1:7500"`) {
		t.Fatalf("bind not rendered: %s", got)
	}
	for _, want := range []string{"unix_socket = true", "[scheduler]", "[logs]", `db_keep_for = 30`} {
		if !strings.Contains(got, want) {
			t.Errorf("template missing %q", want)
		}
	}
}

func TestNewServicePlanSystemDefaults(t *testing.T) {
	plan, err := newServicePlan("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.unitName != "minicrond" || plan.configPath != systemConfigPath || plan.dataDir != systemDataDir {
		t.Fatalf("unexpected system plan: %+v", plan)
	}
	if plan.port != defaultAPIPort || plan.runAs != nil {
		t.Fatalf("unexpected defaults: %+v", plan)
	}
}

func TestNewServicePlanPortValidation(t *testing.T) {
	if _, err := newServicePlan("", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := newServicePlan("", 70000); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("want out-of-range error, got %v", err)
	}
}

func TestNewServicePlanUserRules(t *testing.T) {
	if _, err := newServicePlan("definitely-not-a-user-xyz", 7500); err == nil || !strings.Contains(err.Error(), "unknown user") {
		t.Fatalf("want unknown user error, got %v", err)
	}
	if _, err := newServicePlan("root", 7500); err == nil || !strings.Contains(err.Error(), "non-root") {
		t.Fatalf("want non-root rejection, got %v", err)
	}
	if err := validateUserName("bad name!"); err == nil {
		t.Fatal("want invalid user name rejection")
	}
	if err := validateUserName("alice"); err != nil {
		t.Fatalf("valid user name rejected: %v", err)
	}
}

func TestUnitContent(t *testing.T) {
	system := &servicePlan{unitName: "minicrond", configPath: "/etc/minicrond/config.toml", dataDir: "/var/lib/minicron", port: 7423}
	got := system.unit("/usr/local/bin/minicrond")
	for _, want := range []string{
		serviceUnitHeader,
		"Description=minicrond job scheduler (system)",
		"ExecStart=/usr/local/bin/minicrond daemon --config /etc/minicrond/config.toml --data-dir /var/lib/minicron",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system unit missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "User=") {
		t.Errorf("system unit must not pin a user:\n%s", got)
	}

	asUser := &servicePlan{
		unitName:   "minicrond@alice",
		configPath: "/home/alice/.minicrond/config.toml",
		dataDir:    "/home/alice/.local/share/minicron",
		port:       7500,
		runAs:      &user.User{Username: "alice", Uid: "1000", Gid: "1000"},
	}
	got = asUser.unit("/usr/local/bin/minicrond")
	for _, want := range []string{
		"Description=minicrond job scheduler (user alice)",
		"User=alice",
		"Group=1000",
		"ExecStart=/usr/local/bin/minicrond daemon --config /home/alice/.minicrond/config.toml --data-dir /home/alice/.local/share/minicron",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("user unit missing %q in:\n%s", want, got)
		}
	}
}

func TestConfigBindPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(defaultConfigTOML("127.0.0.1:7555")), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := configBindPort(path); got != 7555 {
		t.Fatalf("bind port = %d, want 7555", got)
	}
	if got := configBindPort(filepath.Join(t.TempDir(), "missing.toml")); got != 0 {
		t.Fatalf("missing config should yield 0, got %d", got)
	}
}

func TestUnitConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicrond@alice.service")
	unit := (&servicePlan{
		unitName:   "minicrond@alice",
		configPath: "/home/alice/.minicrond/config.toml",
		port:       7500,
		runAs:      &user.User{Username: "alice", Uid: "1000", Gid: "1000"},
	}).unit("/usr/local/bin/minicrond")
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := unitConfigPath(path); got != "/home/alice/.minicrond/config.toml" {
		t.Fatalf("unit config path = %q", got)
	}
	unmanaged := filepath.Join(t.TempDir(), "minicrond@bob.service")
	if err := os.WriteFile(unmanaged, []byte("[Service]\nExecStart=/bin/minicrond daemon --config /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := unitConfigPath(unmanaged); got != "" {
		t.Fatalf("unmanaged unit should be ignored, got %q", got)
	}
}
