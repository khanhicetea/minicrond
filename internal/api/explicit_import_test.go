package api

import (
	"encoding/json"
	"testing"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/model"
)

func withJobDefaults(s *Server, defaults model.Definition) {
	s.SetJobDefaults(func() model.Definition { return defaults })
}

func definitionField(t *testing.T, body map[string]any, key string) (float64, bool) {
	t.Helper()
	def, ok := body["definition"].(map[string]any)
	if !ok {
		t.Fatalf("no definition in %v", body)
	}
	v, present := def[key]
	if !present {
		return 0, false
	}
	return v.(float64), true
}

// A12: omitted inherits the daemon defaults, explicit 0 overrides them, and
// the distinction survives GET -> PUT and export -> import.
func TestAPIExplicitZeroOverridesDefaultsAndRoundTrips(t *testing.T) {
	s, _, _ := setup(t)
	withJobDefaults(s, model.Definition{Timeout: 60, Retries: 3})

	if rec := call(s, true, "PUT", "/api/v1/jobs/explicit", "", `{"name":"explicit","command":"true","schedule":"@every 1h","timeout":0,"retries":0}`, nil); rec.Code != 200 {
		t.Fatalf("put explicit: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(s, true, "PUT", "/api/v1/jobs/omitted", "", `{"name":"omitted","command":"true","schedule":"@every 1h"}`, nil); rec.Code != 200 {
		t.Fatalf("put omitted: %d %s", rec.Code, rec.Body.String())
	}
	got := decode(t, call(s, true, "GET", "/api/v1/jobs/explicit", "", "", nil))
	if v, ok := definitionField(t, got, "timeout"); !ok || v != 0 {
		t.Fatalf("explicit timeout=0 must be reported: %v", got["definition"])
	}
	if v, ok := definitionField(t, got, "retries"); !ok || v != 0 {
		t.Fatalf("explicit retries=0 must be reported: %v", got["definition"])
	}
	inherited := decode(t, call(s, true, "GET", "/api/v1/jobs/omitted", "", "", nil))
	if v, _ := definitionField(t, inherited, "timeout"); v != 60 {
		t.Fatalf("omitted timeout must inherit 60: %v", inherited["definition"])
	}
	if v, _ := definitionField(t, inherited, "retries"); v != 3 {
		t.Fatalf("omitted retries must inherit 3: %v", inherited["definition"])
	}

	// Edit round trip: send the GET body back unchanged.
	rec := call(s, true, "GET", "/api/v1/jobs/explicit", "", "", nil)
	var envelope struct {
		Definition json.RawMessage `json:"definition"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var edit map[string]any
	if err := json.Unmarshal(envelope.Definition, &edit); err != nil {
		t.Fatal(err)
	}
	delete(edit, "definition_id")
	delete(edit, "next_fire_at")
	edit["command"] = "true # edited"
	body, _ := json.Marshal(edit)
	if rec := call(s, true, "PUT", "/api/v1/jobs/explicit", "", string(body), nil); rec.Code != 200 {
		t.Fatalf("edit round trip: %d %s", rec.Code, rec.Body.String())
	}
	got = decode(t, call(s, true, "GET", "/api/v1/jobs/explicit", "", "", nil))
	if v, ok := definitionField(t, got, "timeout"); !ok || v != 0 {
		t.Fatalf("edit re-inherited the default timeout: %v", got["definition"])
	}
	if v, ok := definitionField(t, got, "retries"); !ok || v != 0 {
		t.Fatalf("edit re-inherited the default retries: %v", got["definition"])
	}

	// JSON export carries the override.
	exported := decode(t, call(s, true, "GET", "/api/v1/export?format=json", "", "", nil))
	for _, item := range exported["definitions"].([]any) {
		def := item.(map[string]any)
		switch def["name"] {
		case "explicit":
			if v, ok := def["timeout"]; !ok || v.(float64) != 0 {
				t.Fatalf("export lost explicit zero: %v", def)
			}
		case "omitted":
			if def["timeout"].(float64) != 60 {
				t.Fatalf("export changed inherited value: %v", def)
			}
		}
	}

	// TOML export -> import into a fresh registry keeps the override.
	toml := call(s, true, "GET", "/api/v1/export", "", "", nil).Body.String()
	defs, err := config.ParseImport([]byte(toml), model.Definition{Timeout: 60, Retries: 3})
	if err != nil {
		t.Fatalf("re-import export: %v\n%s", err, toml)
	}
	for _, d := range defs {
		if d.Name == "explicit" && (d.Timeout != 0 || d.Retries != 0) {
			t.Fatalf("export/import turned explicit zero into %d/%d", d.Timeout, d.Retries)
		}
		if d.Name == "omitted" && (d.Timeout != 60 || d.Retries != 3) {
			t.Fatalf("export/import changed inherited values: %+v", d)
		}
	}
}

func TestAPIImportExplicitZeroOverridesDefaults(t *testing.T) {
	s, _, _ := setup(t)
	withJobDefaults(s, model.Definition{Timeout: 60, Retries: 3})
	content := "[[job]]\nname='zero'\ncommand='true'\nschedule='@every 1h'\ntimeout=0\nretries=0\n[[job]]\nname='dflt'\ncommand='true'\nschedule='@every 1h'\n"
	req, _ := json.Marshal(importRequest{Content: content})
	preview := decode(t, call(s, true, "POST", "/api/v1/import/preview", "", string(req), nil))
	hash := preview["content_hash"].(string)
	if rec := call(s, true, "POST", "/api/v1/import/apply", "", previewBodyWithHash(content, hash), nil); rec.Code != 200 {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	zero := decode(t, call(s, true, "GET", "/api/v1/jobs/zero", "", "", nil))
	if v, ok := definitionField(t, zero, "timeout"); !ok || v != 0 {
		t.Fatalf("imported explicit timeout=0 became %v", zero["definition"])
	}
	dflt := decode(t, call(s, true, "GET", "/api/v1/jobs/dflt", "", "", nil))
	if v, _ := definitionField(t, dflt, "timeout"); v != 60 {
		t.Fatalf("imported omitted timeout must inherit: %v", dflt["definition"])
	}
}
