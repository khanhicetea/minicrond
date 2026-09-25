package executor

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestNumericUIDWithoutPasswdRunsJobsAndWorkers(t *testing.T) {
	previous := lookupCurrentUser
	lookupCurrentUser = func() (*user.User, error) { return nil, errors.New("no passwd entry") }
	t.Cleanup(func() { lookupCurrentUser = previous })
	t.Setenv("HOME", "/root")
	t.Setenv("USER", "root")
	t.Setenv("LOGNAME", "root")
	home := t.TempDir()
	job := model.Definition{
		Name: "numeric-job", Kind: model.KindJob,
		Argv: []string{"/usr/bin/env"}, Timezone: "UTC", EnvBase: "inherit",
		Env: map[string]string{"HOME": home, "PATH": "/usr/local/bin:/usr/bin:/bin"},
	}
	cmd, label, err := buildCommand(job, model.Run{ID: "test", Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if label != strconv.Itoa(os.Geteuid()) || cmd.SysProcAttr.Credential != nil || cmd.Dir != home {
		t.Fatalf("numeric identity: label=%q credential=%v dir=%q", label, cmd.SysProcAttr.Credential, cmd.Dir)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "TZ=UTC"} {
		if !strings.Contains(string(out), want+"\n") {
			t.Fatalf("missing expected job environment field %q", want)
		}
	}
	if strings.Contains(string(out), "USER=root") || strings.Contains(string(out), "LOGNAME=root") {
		t.Fatal("inherited root identity leaked into job")
	}

	worker := model.Definition{
		Name: "numeric-worker", Kind: model.KindWorker, Command: `printf '%s|%s|%s' "$HOME" "$PWD" "$TZ"`,
		Shell: "/bin/sh", Timezone: "UTC", EnvBase: "clean", WorkingDir: home,
		Env: map[string]string{"HOME": home},
	}
	cmd, _, err = buildCommand(worker, model.Run{ID: "worker", Trigger: "start"})
	if err != nil {
		t.Fatal(err)
	}
	out, err = cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != home+"|"+home+"|UTC" {
		t.Fatalf("worker environment = %q", out)
	}

	job.Env = nil
	job.EnvBase = "inherit"
	job.WorkingDir = home
	cmd, _, err = buildCommand(job, model.Run{ID: "no-home", Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "HOME=") || strings.HasPrefix(entry, "USER=") || strings.HasPrefix(entry, "LOGNAME=") {
			t.Fatalf("unknown identity inherited: %q", entry)
		}
	}
	job.WorkingDir = "~/work"
	if _, _, err := buildCommand(job, model.Run{}); err == nil || !strings.Contains(err.Error(), "HOME is unavailable") {
		t.Fatalf("home-relative path without HOME: %v", err)
	}
	job.Env = map[string]string{"HOME": filepath.Join(home, "sub")}
	cmd, _, err = buildCommand(job, model.Run{})
	if err != nil || cmd.Dir != filepath.Join(home, "sub", "work") {
		t.Fatalf("explicit HOME was not used: %v, %v", cmd, err)
	}
}

func TestPasswdIdentityStillUsesKnownHome(t *testing.T) {
	previous := lookupCurrentUser
	home := t.TempDir()
	lookupCurrentUser = func() (*user.User, error) {
		return &user.User{Uid: strconv.Itoa(os.Geteuid()), Gid: strconv.Itoa(os.Getegid()), Username: "known", HomeDir: home}, nil
	}
	t.Cleanup(func() { lookupCurrentUser = previous })
	cred, actualHome, label, err := identity("")
	if err != nil || cred != nil || actualHome != home || label != "known" {
		t.Fatalf("passwd identity: %v, %q, %q, %v", cred, actualHome, label, err)
	}
}
