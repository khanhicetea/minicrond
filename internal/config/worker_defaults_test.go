package config

import (
	"github.com/khanhicetea/minicrond/internal/model"
	"testing"
)

func TestWorkerDefaultsPrecedenceAndRoundTrip(t *testing.T) {
	defaults := model.Definition{EnvBase: "inherit", WorkingDir: "~/app", Timeout: 60, Retries: 3}
	defs, err := ParseImport([]byte("[[worker]]\nname='queue'\ncommand='true'\ntimeout=0\n"), defaults)
	if err != nil {
		t.Fatal(err)
	}
	d := defs[0]
	if d.EnvBase != "inherit" || d.WorkingDir != "~/app" || d.Timeout != 0 || d.Retries != 0 {
		t.Fatalf("worker: %+v", d)
	}
	round, err := ValidateDefinitionInput(EditableInput(d, defaults), defaults)
	if err != nil || round.Timeout != 0 {
		t.Fatalf("round trip: %+v, %v", round, err)
	}
	defs, err = ParseImport([]byte("[defaults]\nworking_dir='/bundle'\n[[worker]]\nname='queue'\ncommand='true'\nenv_base='clean'\n"), defaults)
	if err != nil {
		t.Fatal(err)
	}
	if defs[0].EnvBase != "clean" || defs[0].WorkingDir != "/bundle" {
		t.Fatalf("precedence: %+v", defs[0])
	}
}
