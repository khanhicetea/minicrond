package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/pelletier/go-toml/v2"
)

func TestParseCrontab(t *testing.T) {
	original := []byte("# keep\nPATH=/custom/bin\nSHELL=/bin/bash\n0 2 * * * echo hello  world\n\n@daily /bin/date\n")
	entries, err := parseCrontab(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].line != 4 || entries[1].line != 6 {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if entries[0].job.Schedule != "0 2 * * *" || entries[0].job.Command != "echo hello  world" || entries[0].job.Shell != "/bin/bash" || entries[0].job.Env["PATH"] != "/custom/bin" {
		t.Fatalf("unexpected first job: %+v", entries[0].job)
	}
	bundle, err := toml.Marshal(struct {
		Jobs []model.Definition `toml:"job"`
	}{Jobs: []model.Definition{entries[0].job, entries[1].job}})
	if err != nil {
		t.Fatal(err)
	}
	if defs, err := config.ParseImport(bundle); err != nil || len(defs) != 2 {
		t.Fatalf("import preview would fail: %v, definitions: %+v", err, defs)
	}
	if entries[0].job.Name == entries[1].job.Name {
		t.Fatal("job names collide")
	}
	updated := commentCrontab(original, entries)
	want := []byte("# keep\nPATH=/custom/bin\nSHELL=/bin/bash\n# minicrond imported: 0 2 * * * echo hello  world\n\n# minicrond imported: @daily /bin/date\n")
	if !bytes.Equal(updated, want) {
		t.Fatalf("commented crontab = %q, want %q", updated, want)
	}
	if again, err := parseCrontab(updated); err != nil || len(again) != 0 {
		t.Fatalf("commented jobs still active: %+v, %v", again, err)
	}
}

func TestCrontabCommandTargetsUser(t *testing.T) {
	if got := crontabCommand("alice", "-l").Args; !slices.Equal(got, []string{"crontab", "-u", "alice", "-l"}) {
		t.Fatalf("list args: %q", got)
	}
	if got := crontabCommand("alice", "-").Args; !slices.Equal(got, []string{"crontab", "-u", "alice", "-"}) {
		t.Fatalf("install args: %q", got)
	}
	if got := crontabCommand("", "-l").Args; !slices.Equal(got, []string{"crontab", "-l"}) {
		t.Fatalf("current user args: %q", got)
	}
}

func TestParseCrontabRejectsUnsafeEntries(t *testing.T) {
	for _, input := range []string{
		"@reboot echo hi\n", "* * * * * echo foo%bar\n", "CRON_TZ=Europe/Berlin\n0 1 * * * date\n", "bad entry\n",
	} {
		t.Run(strings.TrimSpace(input), func(t *testing.T) {
			if _, err := parseCrontab([]byte(input)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
