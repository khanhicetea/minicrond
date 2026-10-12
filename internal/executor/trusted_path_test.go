package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func envValue(entries []string, key string) string {
	value := ""
	for _, entry := range entries {
		if v, ok := strings.CutPrefix(entry, key+"="); ok {
			value = v
		}
	}
	return value
}

func TestConfiguredTrustedLookupDoesNotUseAmbientPATH(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/nologin"); err != nil {
		t.Skip("requires /usr/sbin/nologin")
	}
	t.Setenv("PATH", t.TempDir())
	d := model.Definition{Name: "worker", Kind: model.KindWorker, Argv: []string{"nologin"}, EnvBase: "inherit"}
	if _, _, err := buildCommand(d, model.Run{}); err == nil {
		t.Fatal("legacy lookup unexpectedly found sbin-only program")
	}
	cmd, _, err := buildCommandWithTrustedPath(d, model.Run{}, []string{"/usr/bin", "/usr/sbin", "/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/usr/sbin/nologin" || envValue(cmd.Env, "PATH") != os.Getenv("PATH") {
		t.Fatalf("path=%s env=%v", cmd.Path, cmd.Env)
	}
	// Explicit definition PATH cannot hijack the main argv executable either.
	d.Env = map[string]string{"PATH": "/definitely/untrusted"}
	cmd, _, err = buildCommandWithTrustedPath(d, model.Run{}, []string{"/usr/sbin"})
	if err != nil || cmd.Path != "/usr/sbin/nologin" {
		t.Fatalf("cmd=%v err=%v", cmd, err)
	}
}

func TestTrustedPathEnvironmentAndStandaloneDefaults(t *testing.T) {
	d := model.Definition{Name: "worker", Argv: []string{"true"}, EnvBase: "clean"}
	cmd, _, err := buildCommand(d, model.Run{})
	if err != nil {
		t.Fatal(err)
	}
	if envValue(cmd.Env, "PATH") != "/usr/bin:/bin" {
		t.Fatal("legacy clean PATH changed")
	}
	paths := []string{"/bin", "/usr/bin"}
	cmd, _, err = buildCommandWithTrustedPath(d, model.Run{}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if envValue(cmd.Env, "PATH") != strings.Join(paths, ":") {
		t.Fatalf("PATH=%s", envValue(cmd.Env, "PATH"))
	}
	d.Env = map[string]string{"PATH": "/explicit"}
	cmd, _, err = buildCommandWithTrustedPath(d, model.Run{}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if envValue(cmd.Env, "PATH") != "/explicit" {
		t.Fatal("explicit PATH lost precedence")
	}
	d.Argv = []string{"relative/true"}
	if _, _, err := buildCommandWithTrustedPath(d, model.Run{}, paths); err == nil {
		t.Fatal("relative executable accepted")
	}
}

func TestTrustedPathRevalidatedAtSpawnAndAbsolutePathUnchanged(t *testing.T) {
	d := model.Definition{Argv: []string{"/bin/true"}}
	if _, _, err := buildCommandWithTrustedPath(d, model.Run{}, []string{"/tmp"}); err == nil {
		t.Fatal("unsafe directory not revalidated")
	}
	cmd, _, err := buildCommandWithTrustedPath(d, model.Run{}, []string{"/usr/bin"})
	if err != nil || cmd.Path != "/bin/true" {
		t.Fatalf("absolute program changed: %v %v", cmd, err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "bin")
	if err := os.Symlink("/usr/bin", link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildCommandWithTrustedPath(d, model.Run{}, []string{link}); err == nil {
		t.Fatal("unsafe symlink accepted")
	}
	if os.Geteuid() != 0 {
		d.RunAs = "0"
		if _, _, err := buildCommandWithTrustedPath(d, model.Run{}, []string{"/usr/bin"}); err == nil || !strings.Contains(err.Error(), "run_as requires a root daemon") {
			t.Fatalf("privilege guard changed: %v", err)
		}
	}
}
