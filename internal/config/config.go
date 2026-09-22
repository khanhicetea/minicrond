package config

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/robfig/cron/v3"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
)

type Config struct {
	Server        Server         `toml:"server" json:"server"`
	Scheduler     Scheduler      `toml:"scheduler" json:"scheduler"`
	Storage       Storage        `toml:"storage" json:"storage"`
	Logs          Logs           `toml:"logs" json:"logs"`
	AlertChannels []AlertChannel `toml:"alert_channel" json:"alert_channels,omitempty"`
}

type Server struct {
	Bind                string `toml:"bind" json:"bind"`
	UnixSocket          *bool  `toml:"unix_socket" json:"unix_socket,omitempty"`
	AllowInsecureRemote bool   `toml:"allow_insecure_remote" json:"allow_insecure_remote,omitempty"`
}
type Scheduler struct {
	Timezone          string `toml:"timezone" json:"timezone"`
	MaxConcurrentRuns int    `toml:"max_concurrent_runs" json:"max_concurrent_runs"`
}
type Storage struct {
	KeepRunsDefault int    `toml:"keep_runs_default" json:"keep_runs_default"`
	KeepForDefault  string `toml:"keep_for_default" json:"keep_for_default"`
	AuditKeep       int    `toml:"audit_keep" json:"audit_keep"`
}
type AlertChannel struct {
	Name                string `toml:"name" json:"name"`
	Type                string `toml:"type" json:"type"`
	BotToken            string `toml:"bot_token" json:"-"`
	ChatID              string `toml:"chat_id" json:"chat_id"`
	DisableNotification bool   `toml:"disable_notification" json:"disable_notification"`
}

type Logs struct {
	Backend string `toml:"backend" json:"backend"`
	MaxLine string `toml:"max_line" json:"max_line"`
	// WorkerFlushInterval is how often logs of still-running runs (workers)
	// are sealed in the file buffer and copied into the SQLite log archive.
	WorkerFlushInterval string `toml:"worker_flush_interval" json:"worker_flush_interval"`
	// DBKeepFor prunes archived logs older than this duration from the log
	// database during the daily DBPruneAt sweep.
	DBKeepFor string `toml:"db_keep_for" json:"db_keep_for"`
	// DBPruneAt is the daily local time ("HH:MM") the prune sweep runs.
	DBPruneAt string `toml:"db_prune_at" json:"db_prune_at"`
}

type importBundle struct {
	Defaults model.Definition   `toml:"defaults"`
	Jobs     []model.Definition `toml:"job"`
	Workers  []model.Definition `toml:"worker"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}$`)
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func Load(path string) (*Config, error) {
	var cfg Config
	if err := decode(path, &cfg); err != nil {
		return nil, err
	}
	applyConfigDefaults(&cfg)
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ParseImport(content []byte) ([]model.Definition, error) {
	var part importBundle
	dec := toml.NewDecoder(strings.NewReader(string(content)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&part); err != nil {
		return nil, err
	}
	cfg := Config{Scheduler: Scheduler{Timezone: "UTC"}, Logs: Logs{Backend: "file"}}
	applyConfigDefaults(&cfg)
	for i := range part.Jobs {
		part.Jobs[i].Kind = model.KindJob
		applyDefinitionDefaults(&part.Jobs[i], part.Defaults)
	}
	for i := range part.Workers {
		part.Workers[i].Kind = model.KindWorker
		applyDefinitionDefaults(&part.Workers[i], part.Defaults)
	}
	definitions := append(part.Jobs, part.Workers...)
	if err := validateDefinitions(definitions, cfg.Scheduler.Timezone); err != nil {
		return nil, err
	}
	return definitions, nil
}

func decode(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	dec := toml.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func applyConfigDefaults(c *Config) {
	if c.Server.Bind == "" {
		c.Server.Bind = "127.0.0.1:7423"
	}
	if c.Scheduler.Timezone == "" {
		c.Scheduler.Timezone = "UTC"
	}
	if c.Scheduler.MaxConcurrentRuns == 0 {
		c.Scheduler.MaxConcurrentRuns = 32
	}
	if c.Storage.KeepRunsDefault == 0 {
		c.Storage.KeepRunsDefault = 200
	}
	if c.Storage.AuditKeep == 0 {
		c.Storage.AuditKeep = 10000
	}
	if c.Storage.KeepForDefault == "" {
		c.Storage.KeepForDefault = "720h"
	}
	if c.Logs.Backend == "" {
		c.Logs.Backend = "file"
	}
	if c.Logs.MaxLine == "" {
		c.Logs.MaxLine = "256KiB"
	}
	if c.Logs.WorkerFlushInterval == "" {
		c.Logs.WorkerFlushInterval = "15m"
	}
	if c.Logs.DBKeepFor == "" {
		c.Logs.DBKeepFor = "720h"
	}
	if c.Logs.DBPruneAt == "" {
		c.Logs.DBPruneAt = "03:30"
	}
}
func applyDefinitionDefaults(d *model.Definition, defaults model.Definition) {
	// Bundle defaults fill omitted definition fields; built-in defaults apply last.
	d.Shell = cmp.Or(d.Shell, defaults.Shell, "/bin/sh")
	d.Timezone = cmp.Or(d.Timezone, defaults.Timezone)
	d.CatchUp = cmp.Or(d.CatchUp, defaults.CatchUp, "none")
	d.OnOverlap = cmp.Or(d.OnOverlap, defaults.OnOverlap, "skip")
	d.EnvBase = cmp.Or(d.EnvBase, defaults.EnvBase, "clean")
	d.Grace = cmp.Or(d.Grace, defaults.Grace, "10s")
	d.Timeout = cmp.Or(d.Timeout, defaults.Timeout, "0")
	d.StopSignal = cmp.Or(d.StopSignal, defaults.StopSignal, "SIGTERM")
	d.Restart = cmp.Or(d.Restart, defaults.Restart, "always")
	d.RestartDelay = cmp.Or(d.RestartDelay, defaults.RestartDelay, "5s")
	d.HealthyAfter = cmp.Or(d.HealthyAfter, defaults.HealthyAfter, "30s")
	d.LogOnFull = cmp.Or(d.LogOnFull, defaults.LogOnFull, "drop_old")
	if len(d.Alerts) == 0 {
		d.Alerts = defaults.Alerts
	}
	if len(d.SuccessCodes) == 0 {
		d.SuccessCodes = defaults.SuccessCodes
	}
	if len(d.SuccessCodes) == 0 {
		d.SuccessCodes = []int{0}
	}
	d.MaxRestartAttempts = cmp.Or(d.MaxRestartAttempts, defaults.MaxRestartAttempts, 5)
}

func validateConfig(c *Config) error {
	host, _, err := net.SplitHostPort(c.Server.Bind)
	if err != nil {
		return fmt.Errorf("server.bind: %w", err)
	}
	ip := net.ParseIP(host)
	if !c.Server.AllowInsecureRemote && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("server.bind: non-loopback plaintext HTTP requires allow_insecure_remote=true")
	}
	if c.Scheduler.MaxConcurrentRuns < 1 || c.Scheduler.MaxConcurrentRuns > 1024 {
		return errors.New("scheduler.max_concurrent_runs: must be between 1 and 1024")
	}
	if c.Storage.KeepRunsDefault < 1 || c.Storage.AuditKeep < 1 {
		return errors.New("storage retention counts must be positive")
	}
	if d, err := time.ParseDuration(c.Storage.KeepForDefault); err != nil || d <= 0 {
		return errors.New("storage.keep_for_default: must be a positive duration")
	}
	if _, err := time.LoadLocation(c.Scheduler.Timezone); err != nil {
		return fmt.Errorf("scheduler.timezone: %w", err)
	}
	if c.Logs.Backend != "file" {
		return errors.New("logs.backend: only file is supported in v0.1")
	}
	if n, err := logstore.ParseBytes(c.Logs.MaxLine); err != nil || n < 1 || n > 16<<20 {
		return errors.New("logs.max_line: must be between 1B and 16MiB")
	}
	if d, err := time.ParseDuration(c.Logs.WorkerFlushInterval); err != nil || d < time.Second {
		return errors.New("logs.worker_flush_interval: must be a duration of at least 1s")
	}
	if d, err := time.ParseDuration(c.Logs.DBKeepFor); err != nil || d <= 0 {
		return errors.New("logs.db_keep_for: must be a positive duration")
	}
	if err := ValidateClock(c.Logs.DBPruneAt); err != nil {
		return fmt.Errorf("logs.db_prune_at: %w", err)
	}
	channels := make(map[string]bool, len(c.AlertChannels))
	for _, channel := range c.AlertChannels {
		if !namePattern.MatchString(channel.Name) {
			return fmt.Errorf("alert_channel: invalid name %q", channel.Name)
		}
		if channels[channel.Name] {
			return fmt.Errorf("duplicate alert channel %q", channel.Name)
		}
		channels[channel.Name] = true
		if channel.Type != "telegram" {
			return fmt.Errorf("alert channel %q: unsupported type %q", channel.Name, channel.Type)
		}
		if channel.ChatID == "" {
			return fmt.Errorf("alert channel %q: chat_id is required", channel.Name)
		}
		if name, ok := strings.CutPrefix(channel.BotToken, "env:"); ok {
			if name == "" {
				return fmt.Errorf("alert channel %q: bot_token env name is required", channel.Name)
			}
		} else if path, ok := strings.CutPrefix(channel.BotToken, "file:"); !ok || !filepath.IsAbs(path) {
			return fmt.Errorf("alert channel %q: bot_token must be env:NAME or file:/absolute/path", channel.Name)
		}
	}
	return nil
}

func validateDefinitions(definitions []model.Definition, schedulerTimezone string) error {
	seen := make(map[string]bool)
	for i := range definitions {
		d := &definitions[i]
		if !namePattern.MatchString(d.Name) {
			return fmt.Errorf("invalid definition name %q", d.Name)
		}
		if seen[d.Name] {
			return fmt.Errorf("duplicate definition %q", d.Name)
		}
		seen[d.Name] = true
		if (d.Command == "") == (len(d.Argv) == 0) {
			return fmt.Errorf("%s: exactly one of command or argv is required", d.Name)
		}
		for field, value := range map[string]string{"timeout": d.Timeout, "grace": d.Grace, "restart_delay": d.RestartDelay, "healthy_after": d.HealthyAfter} {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", d.Name, field, err)
			}
			if parsed < 0 || (field != "timeout" && parsed == 0) {
				return fmt.Errorf("%s.%s: must be positive (timeout may be zero)", d.Name, field)
			}
		}
		if d.EnvBase != "clean" && d.EnvBase != "inherit" {
			return fmt.Errorf("%s.env_base must be clean or inherit", d.Name)
		}
		if d.Restart != "always" && d.Restart != "on-failure" && d.Restart != "never" {
			return fmt.Errorf("%s.restart must be always, on-failure, or never", d.Name)
		}
		if d.MaxRestartAttempts < 1 || d.MaxRestartAttempts > 1000 {
			return fmt.Errorf("%s.max_restart_attempts must be between 1 and 1000", d.Name)
		}
		for _, code := range d.SuccessCodes {
			if code < 0 || code > 255 {
				return fmt.Errorf("%s.success_codes values must be between 0 and 255", d.Name)
			}
		}
		if d.KeepRuns < 0 {
			return fmt.Errorf("%s.keep_runs must be nonnegative", d.Name)
		}
		if d.Priority != 0 {
			return fmt.Errorf("%s.priority is not supported", d.Name)
		}
		if d.RunOnStart {
			return fmt.Errorf("%s.run_on_start is not supported", d.Name)
		}
		if d.Kind == model.KindJob && d.Schedule != "" {
			if err := ValidateSchedule(d.Schedule); err != nil {
				return fmt.Errorf("%s.schedule: %w", d.Name, err)
			}
		}
		if d.Timezone == "" {
			d.Timezone = schedulerTimezone
		}
		if _, err := time.LoadLocation(d.Timezone); err != nil {
			return fmt.Errorf("%s.timezone: %w", d.Name, err)
		}
		if d.CatchUp != "none" && d.CatchUp != "latest" {
			return fmt.Errorf("%s.catch_up must be none or latest", d.Name)
		}
		if d.OnOverlap != "skip" && d.OnOverlap != "parallel" {
			return fmt.Errorf("%s.on_overlap must be skip or parallel", d.Name)
		}
		if d.LogOnFull != "drop_old" && d.LogOnFull != "drop_new" {
			return fmt.Errorf("%s.log_on_full must be drop_old or drop_new", d.Name)
		}
		if !validSignal(d.StopSignal) {
			return fmt.Errorf("%s.stop_signal must be one of INT, HUP, QUIT, USR1, USR2, TERM, KILL", d.Name)
		}
		if d.KeepFor != "" {
			if keep, err := time.ParseDuration(d.KeepFor); err != nil || keep <= 0 {
				return fmt.Errorf("%s.keep_for must be a positive duration", d.Name)
			}
		}
		if d.LogMax != "" {
			n, err := logstore.ParseBytes(d.LogMax)
			if err != nil || n <= 0 || n > 1<<40 {
				return fmt.Errorf("%s.log_max must be between 1B and 1TiB", d.Name)
			}
		}
		if d.EnvFile != "" && !filepath.IsAbs(d.EnvFile) {
			return fmt.Errorf("%s.env_file must be an absolute path", d.Name)
		}
		for key, ref := range d.SecretEnv {
			if key == "" || !validSecretRef(ref) {
				return fmt.Errorf("%s.secret_env.%s must be env:NAME or file:/absolute/path", d.Name, key)
			}
		}
		if d.Kind == model.KindWorker && d.Schedule != "" {
			return fmt.Errorf("worker %s cannot have schedule", d.Name)
		}
		if err := validateRunAs(d.RunAs); err != nil {
			return fmt.Errorf("%s.run_as: %w", d.Name, err)
		}
	}
	return nil
}

func ValidateDefinition(d *model.Definition) error {
	if d.Kind == "" {
		d.Kind = model.KindJob
	}
	cfg := Config{Scheduler: Scheduler{Timezone: "UTC"}, Logs: Logs{Backend: "file"}}
	applyConfigDefaults(&cfg)
	applyDefinitionDefaults(d, model.Definition{})
	definitions := []model.Definition{*d}
	if err := validateDefinitions(definitions, cfg.Scheduler.Timezone); err != nil {
		return err
	}
	*d = definitions[0]
	return nil
}

var clockPattern = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// validSignal reports whether stop_signal names a signal the executor can
// send. Values may omit the SIG prefix.
func validSignal(v string) bool {
	switch strings.TrimPrefix(v, "SIG") {
	case "INT", "HUP", "QUIT", "USR1", "USR2", "TERM", "KILL":
		return true
	}
	return false
}

// ValidateClock accepts a daily local time of the form "HH:MM" (24h).
func ValidateClock(value string) error {
	if !clockPattern.MatchString(value) {
		return errors.New("must be HH:MM (00:00-23:59)")
	}
	return nil
}

func ValidateSchedule(value string) error {
	if rest, ok := strings.CutPrefix(value, "@every "); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return err
		}
		if d < time.Second {
			return errors.New("minimum @every interval is 1s")
		}
		return nil
	}
	_, err := cronParser.Parse(value)
	return err
}
func validSecretRef(ref string) bool {
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		return name != "" && !strings.ContainsAny(name, "=\x00")
	}
	path, ok := strings.CutPrefix(ref, "file:")
	return ok && filepath.IsAbs(path)
}

func validateRunAs(value string) error {
	if err := validateRunAsPrivilege(value, os.Geteuid()); err != nil {
		return err
	}
	if value == "" {
		return nil
	}
	name, _, _ := strings.Cut(value, ":")
	if _, err := strconv.Atoi(name); err == nil {
		_, err = user.LookupId(name)
		return err
	}
	_, err := user.Lookup(name)
	return err
}

func validateRunAsPrivilege(value string, euid int) error {
	if value != "" && euid != 0 {
		return errors.New("requires a root daemon")
	}
	return nil
}

func Canonical(d model.Definition) ([]byte, string, error) {
	d.ID, d.Revision = 0, 0
	b, err := json.Marshal(d)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:]), nil
}
