package model

import "time"

const (
	KindJob    = "job"
	KindWorker = "worker"
)

type Definition struct {
	ID                 int64             `json:"definition_id,omitzero" toml:"-"`
	Name               string            `json:"name" toml:"name"`
	Kind               string            `json:"kind" toml:"-"`
	Authority          string            `json:"authority,omitempty" toml:"-"`
	Revision           int64             `json:"revision,omitzero" toml:"-"`
	SourceFile         string            `json:"source_file,omitempty" toml:"-"`
	Enabled            *bool             `json:"enabled,omitempty" toml:"enabled"`
	Command            string            `json:"command,omitempty" toml:"command"`
	Argv               []string          `json:"argv,omitempty" toml:"argv"`
	Shell              string            `json:"shell,omitempty" toml:"shell"`
	Schedule           string            `json:"schedule,omitempty" toml:"schedule"`
	Timezone           string            `json:"timezone,omitempty" toml:"timezone"`
	CatchUp            string            `json:"catch_up,omitempty" toml:"catch_up"`
	OnOverlap          string            `json:"on_overlap,omitempty" toml:"on_overlap"`
	RunOnStart         bool              `json:"run_on_start,omitzero" toml:"run_on_start"`
	RunAs              string            `json:"run_as,omitempty" toml:"run_as"`
	WorkingDir         string            `json:"working_dir,omitempty" toml:"working_dir"`
	EnvBase            string            `json:"env_base,omitempty" toml:"env_base"`
	Env                map[string]string `json:"env,omitempty" toml:"env"`
	SecretEnv          map[string]string `json:"secret_env,omitempty" toml:"secret_env"`
	EnvFile            string            `json:"env_file,omitempty" toml:"env_file"`
	Timeout            string            `json:"timeout,omitempty" toml:"timeout"`
	Grace              string            `json:"grace,omitempty" toml:"grace"`
	StopSignal         string            `json:"stop_signal,omitempty" toml:"stop_signal"`
	SuccessCodes       []int             `json:"success_codes,omitempty" toml:"success_codes"`
	KeepRuns           int               `json:"keep_runs,omitzero" toml:"keep_runs"`
	KeepFor            string            `json:"keep_for,omitempty" toml:"keep_for"`
	LogMax             string            `json:"log_max,omitempty" toml:"log_max"`
	LogOnFull          string            `json:"log_on_full,omitempty" toml:"log_on_full"`
	Labels             map[string]string `json:"labels,omitempty" toml:"labels"`
	Autostart          *bool             `json:"autostart,omitempty" toml:"autostart"`
	Restart            string            `json:"restart,omitempty" toml:"restart"`
	RestartDelay       string            `json:"restart_delay,omitempty" toml:"restart_delay"`
	MaxRestartAttempts int               `json:"max_restart_attempts,omitzero" toml:"max_restart_attempts"`
	HealthyAfter       string            `json:"healthy_after,omitempty" toml:"healthy_after"`
	Priority           int               `json:"priority,omitzero" toml:"priority"`
}

func (d Definition) IsEnabled() bool     { return d.Enabled == nil || *d.Enabled }
func (d Definition) DoesAutostart() bool { return d.Autostart == nil || *d.Autostart }

type Run struct {
	ID             string     `json:"run_id"`
	DefinitionID   int64      `json:"definition_id"`
	Job            string     `json:"job"`
	Kind           string     `json:"kind"`
	Revision       int64      `json:"revision"`
	DefinitionHash string     `json:"definition_hash"`
	Status         string     `json:"status"`
	EndReason      string     `json:"end_reason,omitempty"`
	Trigger        string     `json:"trigger"`
	Attempt        int        `json:"attempt"`
	ScheduledFor   *time.Time `json:"scheduled_for,omitempty"`
	MissedCount    int        `json:"missed_count,omitzero"`
	BootID         string     `json:"boot_id,omitempty"`
	PID            int        `json:"pid,omitzero"`
	PGID           int        `json:"pgid,omitzero"`
	ProcessStartID string     `json:"process_start_id,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	Signal         string     `json:"signal,omitempty"`
	QueuedAt       time.Time  `json:"queued_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	LogRef         string     `json:"log_ref,omitempty"`
	LogBytes       int64      `json:"log_bytes"`
	LogTruncated   bool       `json:"log_truncated"`
}

func CanTransition(from, to string) bool {
	switch from {
	case "pending":
		return to == "running" || to == "failed" || to == "stopped" || to == "interrupted" || to == "skipped" || to == "missed"
	case "running":
		return to == "succeeded" || to == "failed" || to == "timeout" || to == "stopped" || to == "interrupted"
	default:
		return false
	}
}

var TerminalStatuses = map[string]bool{
	"succeeded": true, "failed": true, "timeout": true, "stopped": true,
	"interrupted": true, "skipped": true, "missed": true,
}
