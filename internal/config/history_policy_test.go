package config

import (
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestOnHistoryUnavailable(t *testing.T) {
	const job = "[[job]]\nname='billing'\ncommand='true'\nschedule='@every 1m'\n"
	defs, err := ParseImport([]byte(job))
	if err != nil || defs[0].OnHistoryUnavailable != "" {
		t.Fatalf("default = %+v, %v; want empty (run)", defs, err)
	}
	for _, value := range []string{"run", "skip"} {
		defs, err := ParseImport([]byte(job + "on_history_unavailable='" + value + "'\n"))
		if err != nil || defs[0].OnHistoryUnavailable != value {
			t.Fatalf("%s: %+v, %v", value, defs, err)
		}
	}
	if _, err := ParseImport([]byte(job + "on_history_unavailable='never'\n")); err == nil {
		t.Fatal("an unknown value must be rejected")
	}
	if _, err := ParseImport([]byte("[[worker]]\nname='w'\ncommand='true'\non_history_unavailable='skip'\n")); err == nil {
		t.Fatal("a worker setting must be rejected")
	}

	// Job defaults apply to jobs only; an explicit job value wins.
	defaults := model.Definition{OnHistoryUnavailable: "skip"}
	defs, err = ParseImport([]byte(job+"[[job]]\nname='cleanup'\ncommand='true'\nschedule='@every 1m'\non_history_unavailable='run'\n[[worker]]\nname='w'\ncommand='true'\n"), defaults)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range defs {
		got[d.Name] = d.OnHistoryUnavailable
	}
	if got["billing"] != "skip" || got["cleanup"] != "run" || got["w"] != "" {
		t.Fatalf("with defaults = %v", got)
	}
}
