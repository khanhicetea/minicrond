package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

// A12: an explicit zero overrides a nonzero default; an omitted key inherits.
func TestParseImportExplicitZeroOverridesDefaults(t *testing.T) {
	global := model.Definition{Timeout: 60, Retries: 3}
	content := `
[[job]]
name = "explicit"
command = "true"
timeout = 0
retries = 0

[[job]]
name = "omitted"
command = "true"

[[job]]
name = "set"
command = "true"
timeout = 5
retries = 1
`
	defs, err := ParseImport([]byte(content), global)
	if err != nil {
		t.Fatal(err)
	}
	if defs[0].Timeout != 0 || defs[0].Retries != 0 {
		t.Fatalf("explicit zero was replaced by defaults: timeout=%d retries=%d", defs[0].Timeout, defs[0].Retries)
	}
	if defs[1].Timeout != 60 || defs[1].Retries != 3 {
		t.Fatalf("omitted fields must inherit: timeout=%d retries=%d", defs[1].Timeout, defs[1].Retries)
	}
	if defs[2].Timeout != 5 || defs[2].Retries != 1 {
		t.Fatalf("explicit values lost: timeout=%d retries=%d", defs[2].Timeout, defs[2].Retries)
	}
}

func TestParseImportExplicitZeroOverridesBundleDefaults(t *testing.T) {
	content := `
[defaults]
timeout = 30
retries = 2

[[job]]
name = "explicit"
command = "true"
timeout = 0
retries = 0

[[job]]
name = "omitted"
command = "true"

[[worker]]
name = "w-explicit"
command = "true"
timeout = 0

[[worker]]
name = "w-omitted"
command = "true"
`
	for _, global := range [][]model.Definition{nil, {{Timeout: 60, Retries: 3}}} {
		defs, err := ParseImport([]byte(content), global...)
		if err != nil {
			t.Fatal(err)
		}
		if defs[0].Timeout != 0 || defs[0].Retries != 0 {
			t.Fatalf("explicit zero lost (global=%v): %+v", global != nil, defs[0])
		}
		if defs[1].Timeout != 30 || defs[1].Retries != 2 {
			t.Fatalf("omitted job must inherit bundle defaults (global=%v): %+v", global != nil, defs[1])
		}
		if defs[2].Timeout != 0 || defs[3].Timeout != 30 {
			t.Fatalf("worker timeouts: explicit=%d omitted=%d", defs[2].Timeout, defs[3].Timeout)
		}
	}
}

func TestLoadExplicitZeroOverridesConfigDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	mustWrite(t, path, `
[defaults]
timeout = 60
retries = 3

[[init]]
name = "prepare"
command = "true"
timeout = 0

[[init]]
name = "prepare-default"
command = "true"

[[job]]
name = "explicit"
command = "true"
schedule = "@every 1m"
timeout = 0
retries = 0

[[job]]
name = "omitted"
command = "true"
schedule = "@every 1m"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Init[0].Timeout != 0 || cfg.Init[1].Timeout != 60 || cfg.Init[1].Retries != 0 {
		t.Fatalf("init timeouts: %+v %+v", cfg.Init[0], cfg.Init[1])
	}
	if cfg.Jobs[0].Timeout != 0 || cfg.Jobs[0].Retries != 0 {
		t.Fatalf("explicit zero lost: %+v", cfg.Jobs[0])
	}
	if cfg.Jobs[1].Timeout != 60 || cfg.Jobs[1].Retries != 3 {
		t.Fatalf("omitted fields must inherit: %+v", cfg.Jobs[1])
	}
}

func TestValidateDefinitionInputExplicitZero(t *testing.T) {
	defaults := model.Definition{Timeout: 60, Retries: 3}
	parse := func(body string) model.Definition {
		t.Helper()
		var in DefinitionInput
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			t.Fatal(err)
		}
		d, err := ValidateDefinitionInput(in, defaults)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := parse(`{"name":"a","command":"true","timeout":0,"retries":0}`); d.Timeout != 0 || d.Retries != 0 {
		t.Fatalf("explicit zero lost: %+v", d)
	}
	if d := parse(`{"name":"a","command":"true"}`); d.Timeout != 60 || d.Retries != 3 {
		t.Fatalf("omitted must inherit: %+v", d)
	}
	// The legacy entry point cannot see presence and keeps inheriting.
	d := model.Definition{Name: "a", Command: "true"}
	if err := ValidateDefinition(&d, defaults); err != nil || d.Timeout != 60 {
		t.Fatalf("ValidateDefinition: %+v %v", d, err)
	}
}

// The editable form must survive GET -> PUT: explicit zeros are spelled out
// whenever they would otherwise be re-read as inheritance.
func TestEditableInputRoundTripsExplicitZero(t *testing.T) {
	defaults := model.Definition{Timeout: 60, Retries: 3}
	in := DefinitionInput{Definition: model.Definition{Name: "a", Command: "true"}}
	zero := 0
	in.Timeout, in.Retries = &zero, &zero
	saved, err := ValidateDefinitionInput(in, defaults)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(EditableInput(saved, defaults))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"timeout":0`) || !strings.Contains(string(body), `"retries":0`) {
		t.Fatalf("explicit zeros not emitted: %s", body)
	}
	var back DefinitionInput
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	again, err := ValidateDefinitionInput(back, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if again.Timeout != 0 || again.Retries != 0 {
		t.Fatalf("round trip changed the definition: %+v", again)
	}
	// With no nonzero default there is nothing to preserve and the stored
	// JSON stays byte-identical to earlier versions.
	plain := EditableInput(model.Definition{Name: "a", Command: "true", Kind: model.KindJob})
	if plain.Timeout != nil || plain.Retries != nil {
		t.Fatalf("unneeded explicit zeros emitted: %+v", plain)
	}
}
