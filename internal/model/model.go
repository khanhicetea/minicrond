package model

import (
	"maps"
	"slices"
	"time"
)

//go:generate sh -c "cd ../.. && go tool tygo generate --config tygo.yaml"

type Kind = string

const (
	KindJob    Kind = "job"
	KindWorker Kind = "worker"
)

type Definition struct {
	ID        int64    `json:"definition_id,omitzero" toml:"-"`
	Name      string   `json:"name" toml:"name"`
	Kind      Kind     `json:"kind" toml:"-"`
	Revision  int64    `json:"revision,omitzero" toml:"-"`
	Source    string   `json:"source,omitempty" toml:"-"`
	Enabled   *bool    `json:"enabled,omitempty" toml:"enabled"`
	Command   string   `json:"command,omitempty" toml:"command"`
	Argv      []string `json:"argv,omitempty" toml:"argv"`
	Shell     string   `json:"shell,omitempty" toml:"shell"`
	Schedule  string   `json:"schedule,omitempty" toml:"schedule"`
	Timezone  string   `json:"timezone,omitempty" toml:"timezone"`
	CatchUp   string   `json:"catch_up,omitempty" toml:"catch_up"`
	OnOverlap string   `json:"on_overlap,omitempty" toml:"on_overlap"`
	Retries   int      `json:"retries,omitzero" toml:"retries"`
	// RetryDelay is measured in seconds.
	RetryDelay int               `json:"retry_delay,omitzero" toml:"retry_delay"`
	RunOnStart bool              `json:"run_on_start,omitzero" toml:"run_on_start"`
	RunAs      string            `json:"run_as,omitempty" toml:"run_as"`
	WorkingDir string            `json:"working_dir,omitempty" toml:"working_dir"`
	EnvBase    string            `json:"env_base,omitempty" toml:"env_base"`
	Env        map[string]string `json:"env,omitempty" toml:"env"`
	SecretEnv  map[string]string `json:"secret_env,omitempty" toml:"secret_env"`
	EnvFile    string            `json:"env_file,omitempty" toml:"env_file"`
	// Timeout and Grace are measured in seconds.
	Timeout      int    `json:"timeout,omitzero" toml:"timeout"`
	Grace        int    `json:"grace,omitzero" toml:"grace"`
	StopSignal   string `json:"stop_signal,omitempty" toml:"stop_signal"`
	SuccessCodes []int  `json:"success_codes,omitempty" toml:"success_codes"`
	KeepRuns     int    `json:"keep_runs,omitzero" toml:"keep_runs"`
	// KeepFor is measured in days. Zero uses the storage default.
	KeepFor int `json:"keep_for,omitzero" toml:"keep_for"`
	// LogMax is measured in MiB. Zero uses the default 100 MiB.
	LogMax    int               `json:"log_max,omitzero" toml:"log_max"`
	LogOnFull string            `json:"log_on_full,omitempty" toml:"log_on_full"`
	Labels    map[string]string `json:"labels,omitempty" toml:"labels"`
	Alerts    []string          `json:"alerts,omitempty" toml:"alerts"`
	Autostart *bool             `json:"autostart,omitempty" toml:"autostart"`
	Restart   string            `json:"restart,omitempty" toml:"restart"`
	// RestartDelay and HealthyAfter are measured in seconds.
	RestartDelay       int `json:"restart_delay,omitzero" toml:"restart_delay"`
	MaxRestartAttempts int `json:"max_restart_attempts,omitzero" toml:"max_restart_attempts"`
	HealthyAfter       int `json:"healthy_after,omitzero" toml:"healthy_after"`
	Priority           int `json:"priority,omitzero" toml:"priority"`
	// NextFireAt is populated by the scheduler for API responses only.
	NextFireAt *time.Time `json:"next_fire_at,omitzero" toml:"-"`
}

// Clone creates an independent snapshot of a definition's mutable fields.
// Callers can retain the snapshot while another owner updates the original.
func (d Definition) Clone() Definition {
	d.Argv = slices.Clone(d.Argv)
	d.SuccessCodes = slices.Clone(d.SuccessCodes)
	d.Alerts = slices.Clone(d.Alerts)
	d.Env = maps.Clone(d.Env)
	d.SecretEnv = maps.Clone(d.SecretEnv)
	d.Labels = maps.Clone(d.Labels)
	if d.Enabled != nil {
		value := *d.Enabled
		d.Enabled = &value
	}
	if d.Autostart != nil {
		value := *d.Autostart
		d.Autostart = &value
	}
	if d.NextFireAt != nil {
		value := *d.NextFireAt
		d.NextFireAt = &value
	}
	return d
}

func (d Definition) IsEnabled() bool     { return d.Enabled == nil || *d.Enabled }
func (d Definition) DoesAutostart() bool { return d.Autostart == nil || *d.Autostart }

// ResourceUsage is kernel exit accounting, not a whole-process-tree total.
// CPU times are microseconds; peak RSS is bytes (not a combined tree peak).
type ResourceUsage struct {
	UserCPUUS    int64 `json:"user_cpu_us"`
	SystemCPUUS  int64 `json:"system_cpu_us"`
	PeakRSSBytes int64 `json:"peak_rss_bytes"`
}

type Run struct {
	ID             string         `json:"run_id"`
	DefinitionID   int64          `json:"definition_id"`
	Job            string         `json:"job"`
	Kind           string         `json:"kind"`
	Revision       int64          `json:"revision"`
	DefinitionHash string         `json:"definition_hash"`
	Status         string         `json:"status"`
	EndReason      string         `json:"end_reason,omitempty"`
	Trigger        string         `json:"trigger"`
	Attempt        int            `json:"attempt"`
	ParentRunID    string         `json:"parent_run_id,omitempty"`
	ScheduledFor   *time.Time     `json:"scheduled_for,omitempty"`
	MissedCount    int            `json:"missed_count,omitzero"`
	BootID         string         `json:"boot_id,omitempty"`
	PID            int            `json:"pid,omitzero"`
	PGID           int            `json:"pgid,omitzero"`
	ProcessStartID string         `json:"process_start_id,omitempty"`
	ExitCode       *int           `json:"exit_code,omitempty"`
	Signal         string         `json:"signal,omitempty"`
	QueuedAt       time.Time      `json:"queued_at"`
	StartedAt      *time.Time     `json:"started_at,omitempty"`
	EndedAt        *time.Time     `json:"ended_at,omitempty"`
	LogRef         string         `json:"log_ref,omitempty"`
	LogBytes       int64          `json:"log_bytes"`
	LogTruncated   bool           `json:"log_truncated"`
	ResourceUsage  *ResourceUsage `json:"resource_usage,omitempty"`
}

// Clone creates an independent snapshot of a run's optional values.
func (r Run) Clone() Run {
	if r.ResourceUsage != nil {
		value := *r.ResourceUsage
		r.ResourceUsage = &value
	}
	if r.ScheduledFor != nil {
		value := *r.ScheduledFor
		r.ScheduledFor = &value
	}
	if r.ExitCode != nil {
		value := *r.ExitCode
		r.ExitCode = &value
	}
	if r.StartedAt != nil {
		value := *r.StartedAt
		r.StartedAt = &value
	}
	if r.EndedAt != nil {
		value := *r.EndedAt
		r.EndedAt = &value
	}
	return r
}

// TerminalStatuses reports whether a run status is final. Run transitions
// themselves are enforced by the store's guarded UPDATE statements.
func Terminal(status string) bool { return TerminalStatuses[status] }

var TerminalStatuses = map[string]bool{
	"succeeded": true, "failed": true, "timeout": true, "stopped": true,
	"interrupted": true, "skipped": true, "missed": true,
}
