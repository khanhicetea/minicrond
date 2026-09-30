package scheduler

import (
	"maps"
	"reflect"
	"slices"

	"github.com/khanhicetea/minicrond/internal/model"
)

// sameDefinition avoids reflecting over and boxing entire definitions on reload.
// Nil collections remain distinct from empty ones, matching the previous
// comparison. Optional values are compared by value rather than address.
func sameDefinition(a, b *model.Definition) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Kind == b.Kind &&
		a.Revision == b.Revision && a.Source == b.Source &&
		sameBool(a.Enabled, b.Enabled) && a.Command == b.Command &&
		a.Shell == b.Shell && a.Schedule == b.Schedule && a.Timezone == b.Timezone &&
		a.CatchUp == b.CatchUp && a.OnOverlap == b.OnOverlap &&
		a.Retries == b.Retries && a.RetryDelay == b.RetryDelay &&
		a.RunOnStart == b.RunOnStart && a.RunAs == b.RunAs &&
		a.WorkingDir == b.WorkingDir && a.EnvBase == b.EnvBase && a.EnvFile == b.EnvFile &&
		a.Timeout == b.Timeout && a.Grace == b.Grace && a.StopSignal == b.StopSignal &&
		a.KeepRuns == b.KeepRuns && a.KeepFor == b.KeepFor &&
		a.LogMax == b.LogMax && a.LogOnFull == b.LogOnFull &&
		sameBool(a.Autostart, b.Autostart) && a.Restart == b.Restart &&
		a.RestartDelay == b.RestartDelay && a.MaxRestartAttempts == b.MaxRestartAttempts &&
		a.HealthyAfter == b.HealthyAfter && a.Priority == b.Priority &&
		(a.Argv == nil) == (b.Argv == nil) && slices.Equal(a.Argv, b.Argv) &&
		(a.SuccessCodes == nil) == (b.SuccessCodes == nil) && slices.Equal(a.SuccessCodes, b.SuccessCodes) &&
		(a.Alerts == nil) == (b.Alerts == nil) && slices.Equal(a.Alerts, b.Alerts) &&
		(a.Env == nil) == (b.Env == nil) && maps.Equal(a.Env, b.Env) &&
		(a.SecretEnv == nil) == (b.SecretEnv == nil) && maps.Equal(a.SecretEnv, b.SecretEnv) &&
		(a.Labels == nil) == (b.Labels == nil) && maps.Equal(a.Labels, b.Labels) &&
		sameNextFire(a, b)
}

func sameBool(a, b *bool) bool {
	return a == b || (a != nil && b != nil && *a == *b)
}

func sameNextFire(a, b *model.Definition) bool {
	// API-only timestamps are normally nil here. Retain the full comparison
	// of time.Time's location and monotonic data when callers populate them.
	return a.NextFireAt == b.NextFireAt ||
		(a.NextFireAt != nil && b.NextFireAt != nil && reflect.DeepEqual(a.NextFireAt, b.NextFireAt))
}
