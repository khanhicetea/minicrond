package store

import (
	"errors"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestConfigDefinitionsOwnNamesAndSyncChanges(t *testing.T) {
	st, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	def := model.Definition{Name: "internal", Kind: model.KindJob, Command: "true", Schedule: "@every 1m"}
	if err := st.SyncConfigDefinitions(t.Context(), []model.Definition{def}); err != nil {
		t.Fatal(err)
	}
	first, _, err := st.Definition(t.Context(), def.Name)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != "config" || first.Revision != 1 {
		t.Fatalf("first sync = %+v", first)
	}
	if err := st.SyncConfigDefinitions(t.Context(), []model.Definition{def}); err != nil {
		t.Fatal(err)
	}
	second, _, err := st.Definition(t.Context(), def.Name)
	if err != nil || second.Revision != first.Revision {
		t.Fatalf("idempotent sync = %+v, %v", second, err)
	}
	if _, err := st.PutDefinition(t.Context(), def, first.Revision, "api"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("edit = %v", err)
	}
	if err := st.SetEnabled(t.Context(), def.Name, false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("disable = %v", err)
	}
	if err := st.DeleteDefinition(t.Context(), def.Name, "api"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("delete = %v", err)
	}
	if err := st.ImportDefinitions(t.Context(), []model.Definition{def}, "api:import"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("import = %v", err)
	}
	def.Command = "echo changed"
	if err := st.SyncConfigDefinitions(t.Context(), []model.Definition{def}); err != nil {
		t.Fatal(err)
	}
	changed, _, err := st.Definition(t.Context(), def.Name)
	if err != nil || changed.ID != first.ID || changed.Revision != first.Revision+1 {
		t.Fatalf("changed = %+v, %v", changed, err)
	}
	if err := st.SyncConfigDefinitions(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	defs, err := st.Definitions(t.Context())
	if err != nil || len(defs) != 0 {
		t.Fatalf("removed definitions = %+v, %v", defs, err)
	}
	if err := st.SyncConfigDefinitions(t.Context(), []model.Definition{def}); err != nil {
		t.Fatal(err)
	}
	restored, _, err := st.Definition(t.Context(), def.Name)
	if err != nil || restored.ID != first.ID {
		t.Fatalf("restored = %+v, %v", restored, err)
	}
}

func TestConfigDefinitionCollisionIsAtomic(t *testing.T) {
	st, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	registered := model.Definition{Name: "taken", Kind: model.KindJob, Command: "true"}
	if _, err := st.PutDefinition(t.Context(), registered, 0, "api"); err != nil {
		t.Fatal(err)
	}
	defs := []model.Definition{{Name: "new", Kind: model.KindJob, Command: "true"}, registered}
	if err := st.SyncConfigDefinitions(t.Context(), defs); err == nil {
		t.Fatal("expected name collision")
	}
	items, err := st.Definitions(t.Context())
	if err != nil || len(items) != 1 || items[0].Name != "taken" || items[0].Source == "config" {
		t.Fatalf("partial sync = %+v, %v", items, err)
	}
}
