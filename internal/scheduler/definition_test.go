package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestSameDefinitionCoversEveryField(t *testing.T) {
	var zero model.Definition
	typ := reflect.TypeFor[model.Definition]()
	for i := range typ.NumField() {
		t.Run(typ.Field(i).Name, func(t *testing.T) {
			var changed model.Definition
			field := reflect.ValueOf(&changed).Elem().Field(i)
			switch field.Kind() {
			case reflect.String:
				field.SetString("changed")
			case reflect.Int, reflect.Int64:
				field.SetInt(1)
			case reflect.Bool:
				field.SetBool(true)
			case reflect.Slice:
				field.Set(reflect.MakeSlice(field.Type(), 0, 0))
			case reflect.Map:
				field.Set(reflect.MakeMap(field.Type()))
			case reflect.Pointer:
				field.Set(reflect.New(field.Type().Elem()))
			default:
				t.Fatalf("add coverage for field type %v", field.Type())
			}
			if sameDefinition(&zero, &changed) || sameDefinition(&changed, &zero) {
				t.Fatal("changed field was ignored")
			}
		})
	}
}

func TestSameDefinitionComparesCollectionAndPointerValues(t *testing.T) {
	makeDefinition := func() model.Definition {
		enabled, autostart := true, false
		next := time.Date(2026, 9, 30, 0, 0, 0, 0, time.FixedZone("zone", 3600))
		return model.Definition{
			Enabled: &enabled, Autostart: &autostart, NextFireAt: &next,
			Argv: []string{"echo", "hello"}, SuccessCodes: []int{0, 2}, Alerts: []string{"ops"},
			Env: map[string]string{"key": "value"}, SecretEnv: map[string]string{"key": "env:SECRET"},
			Labels: map[string]string{"key": "value"},
		}
	}
	a, b := makeDefinition(), makeDefinition()
	if !sameDefinition(&a, &b) {
		t.Fatal("equal values at distinct addresses were treated as changed")
	}
	changes := []func(*model.Definition){
		func(d *model.Definition) { *d.Enabled = false },
		func(d *model.Definition) { *d.Autostart = true },
		func(d *model.Definition) { *d.NextFireAt = d.NextFireAt.Add(time.Minute) },
		func(d *model.Definition) { d.Argv[1] = "world" },
		func(d *model.Definition) { d.SuccessCodes[1] = 3 },
		func(d *model.Definition) { d.Alerts[0] = "other" },
		func(d *model.Definition) { d.Env["key"] = "other" },
		func(d *model.Definition) { d.SecretEnv["key"] = "env:OTHER" },
		func(d *model.Definition) { d.Labels["key"] = "other" },
	}
	for i, change := range changes {
		b = makeDefinition()
		change(&b)
		if sameDefinition(&a, &b) {
			t.Errorf("change %d was ignored", i)
		}
	}
}
