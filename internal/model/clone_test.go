package model

import (
	"testing"
	"time"
)

func TestDefinitionCloneOwnsMutableFields(t *testing.T) {
	enabled, autostart := true, true
	next := time.Now()
	original := Definition{
		Name: "worker", Enabled: &enabled, Autostart: &autostart, NextFireAt: &next,
		Argv: []string{"echo", "original"}, SuccessCodes: []int{0}, Alerts: []string{"ops"},
		Env: map[string]string{"MODE": "original"}, SecretEnv: map[string]string{"TOKEN": "env:ORIGINAL"},
		Labels: map[string]string{"team": "original"},
	}
	clone := original.Clone()
	clone.Argv[1] = "changed"
	clone.SuccessCodes[0] = 1
	clone.Alerts[0] = "changed"
	clone.Env["MODE"] = "changed"
	clone.SecretEnv["TOKEN"] = "env:CHANGED"
	clone.Labels["team"] = "changed"
	*clone.Enabled = false
	*clone.Autostart = false
	*clone.NextFireAt = next.Add(time.Hour)
	if original.Argv[1] != "original" || original.SuccessCodes[0] != 0 || original.Alerts[0] != "ops" ||
		original.Env["MODE"] != "original" || original.SecretEnv["TOKEN"] != "env:ORIGINAL" || original.Labels["team"] != "original" ||
		!*original.Enabled || !*original.Autostart || !original.NextFireAt.Equal(next) {
		t.Fatalf("mutating clone changed original: %+v", original)
	}
	if clone.Name != original.Name {
		t.Fatalf("clone lost scalar fields: %+v", clone)
	}
}

func TestDefinitionClonePreservesNilFields(t *testing.T) {
	clone := (Definition{}).Clone()
	if clone.Argv != nil || clone.SuccessCodes != nil || clone.Alerts != nil || clone.Env != nil || clone.SecretEnv != nil ||
		clone.Labels != nil || clone.Enabled != nil || clone.Autostart != nil || clone.NextFireAt != nil {
		t.Fatalf("clone changed nil fields: %+v", clone)
	}
}

func TestRunCloneOwnsOptionalValues(t *testing.T) {
	scheduled, started, ended := time.Now(), time.Now(), time.Now()
	code := 0
	original := Run{ID: "run", ScheduledFor: &scheduled, StartedAt: &started, EndedAt: &ended, ExitCode: &code}
	clone := original.Clone()
	*clone.ScheduledFor = scheduled.Add(time.Hour)
	*clone.StartedAt = started.Add(time.Hour)
	*clone.EndedAt = ended.Add(time.Hour)
	*clone.ExitCode = 1
	if !original.ScheduledFor.Equal(scheduled) || !original.StartedAt.Equal(started) || !original.EndedAt.Equal(ended) || *original.ExitCode != 0 {
		t.Fatalf("mutating clone changed original: %+v", original)
	}
	if clone.ID != original.ID {
		t.Fatalf("clone lost scalar fields: %+v", clone)
	}
	zero := (Run{}).Clone()
	if zero.ScheduledFor != nil || zero.StartedAt != nil || zero.EndedAt != nil || zero.ExitCode != nil {
		t.Fatalf("clone changed nil fields: %+v", zero)
	}
}
