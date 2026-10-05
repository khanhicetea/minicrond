package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// userPlan returns a per-user plan whose "home" is a temp dir and whose target
// user is the current user, so ownership changes are valid without root and no
// real privileged path is ever touched.
func userPlan(t *testing.T, port int) (*servicePlan, string) {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	u2 := *u
	u2.HomeDir = home
	return &servicePlan{
		unitName:   "minicrond@" + u.Username,
		configPath: filepath.Join(home, ".minicrond", "config.toml"),
		dataDir:    filepath.Join(home, ".local", "share", "minicron"),
		port:       port,
		runAs:      &u2,
	}, home
}

func setServiceHook(t *testing.T, hook func(stage string)) {
	t.Helper()
	serviceFSHook = hook
	t.Cleanup(func() { serviceFSHook = nil })
}

type dirState struct {
	mode os.FileMode
	uid  uint32
	gid  uint32
	n    int
}

func snapshotDir(t *testing.T, dir string) dirState {
	t.Helper()
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return dirState{fi.Mode(), st.Uid, st.Gid, len(entries)}
}

func TestWriteUserServiceConfigCreatesPrivateDirAndFile(t *testing.T) {
	plan, _ := userPlan(t, 7501)
	if err := writeServiceConfig(plan); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Lstat(filepath.Dir(plan.configPath))
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config dir: %v %v", dirInfo, err)
	}
	fileInfo, err := os.Lstat(plan.configPath)
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", fileInfo, err)
	}
	if got := configBindPort(plan.configPath); got != 7501 {
		t.Fatalf("bind port = %d", got)
	}
}

func TestWriteUserServiceConfigKeepsExistingAndChecksPort(t *testing.T) {
	plan, _ := userPlan(t, 7502)
	if err := writeServiceConfig(plan); err != nil {
		t.Fatal(err)
	}
	if err := writeServiceConfig(plan); err != nil {
		t.Fatalf("rewriting same plan: %v", err)
	}
	other := *plan
	other.port = 7599
	if err := writeServiceConfig(&other); err == nil || !strings.Contains(err.Error(), "already binds port 7502") {
		t.Fatalf("want port mismatch error, got %v", err)
	}
}

func TestWriteUserServiceConfigAllowsAdminSymlinkedHome(t *testing.T) {
	plan, home := userPlan(t, 7503)
	link := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	plan.runAs.HomeDir = link
	plan.configPath = filepath.Join(link, ".minicrond", "config.toml")
	if err := writeServiceConfig(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".minicrond", "config.toml")); err != nil {
		t.Fatal(err)
	}
}

func TestWriteUserServiceConfigRejectsSymlinkedConfigDir(t *testing.T) {
	plan, home := userPlan(t, 7504)
	target := t.TempDir() // stands in for a privileged directory
	if err := os.Chmod(target, 0o750); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, target)
	if err := os.Symlink(target, filepath.Join(home, ".minicrond")); err != nil {
		t.Fatal(err)
	}
	err := writeServiceConfig(plan)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("symlinked config dir accepted: %v", err)
	}
	if after := snapshotDir(t, target); after != before {
		t.Fatalf("symlink target changed: before=%+v after=%+v", before, after)
	}
}

func TestWriteUserServiceConfigRejectsSymlinkedConfigFile(t *testing.T) {
	plan, _ := userPlan(t, 7505)
	dir := filepath.Dir(plan.configPath)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, plan.configPath); err != nil {
		t.Fatal(err)
	}
	if err := writeServiceConfig(plan); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("symlinked config file accepted: %v", err)
	}
	// Dangling link: creation must not write through it either.
	missing := filepath.Join(t.TempDir(), "missing")
	if err := os.Remove(plan.configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(missing, plan.configPath); err != nil {
		t.Fatal(err)
	}
	if err := writeServiceConfig(plan); err == nil {
		t.Fatal("dangling config symlink accepted")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("dangling target was created: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("victim modified: %q", b)
	}
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o640 {
		t.Fatalf("victim mode changed: %v", fi.Mode())
	}
}

func TestWriteUserServiceConfigRejectsNonDirectoryConfigDir(t *testing.T) {
	plan, home := userPlan(t, 7506)
	if err := os.WriteFile(filepath.Join(home, ".minicrond"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeServiceConfig(plan); err == nil {
		t.Fatal("regular file accepted as config dir")
	}
}

func TestWriteUserServiceConfigConfigDirReplacedByRace(t *testing.T) {
	plan, home := userPlan(t, 7507)
	target := t.TempDir()
	before := snapshotDir(t, target)
	moved := filepath.Join(home, ".minicrond.moved")
	setServiceHook(t, func(stage string) {
		if stage != "before-open-config" {
			return
		}
		// The user swaps the validated directory for a symlink mid-install.
		if err := os.Rename(filepath.Join(home, ".minicrond"), moved); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(target, filepath.Join(home, ".minicrond")); err != nil {
			t.Error(err)
		}
	})
	if err := writeServiceConfig(plan); err != nil {
		t.Fatal(err)
	}
	if after := snapshotDir(t, target); after != before {
		t.Fatalf("race redirected the install into the symlink target: before=%+v after=%+v", before, after)
	}
	if _, err := os.Stat(filepath.Join(moved, "config.toml")); err != nil {
		t.Fatalf("config should have gone to the originally opened directory: %v", err)
	}
}

func TestWriteUserServiceConfigConfigFileReplacedByRace(t *testing.T) {
	plan, _ := userPlan(t, 7508)
	victim := filepath.Join(t.TempDir(), "victim")
	setServiceHook(t, func(stage string) {
		if stage != "before-create-config" {
			return
		}
		if err := os.Symlink(victim, plan.configPath); err != nil {
			t.Error(err)
		}
	})
	if err := writeServiceConfig(plan); err == nil {
		t.Fatal("config symlink planted during install was followed")
	}
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Fatalf("victim created through planted symlink: %v", err)
	}
}

func TestWriteUserServiceConfigRejectsPathOutsideHome(t *testing.T) {
	plan, _ := userPlan(t, 7509)
	plan.configPath = filepath.Join(t.TempDir(), "elsewhere", "config.toml")
	if err := writeServiceConfig(plan); err == nil || !strings.Contains(err.Error(), "not inside home") {
		t.Fatalf("outside-home path accepted: %v", err)
	}
}
