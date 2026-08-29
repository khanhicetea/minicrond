package config

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/robfig/cron/v3"

	"github.com/minicron/minicron/internal/logstore"
	"github.com/minicron/minicron/internal/model"
)

type Config struct {
	Path        string             `toml:"-" json:"path"`
	Server      Server             `toml:"server" json:"server"`
	Include     Include            `toml:"include" json:"include"`
	Scheduler   Scheduler          `toml:"scheduler" json:"scheduler"`
	Storage     Storage            `toml:"storage" json:"storage"`
	Logs        Logs               `toml:"logs" json:"logs"`
	Defaults    model.Definition   `toml:"defaults" json:"defaults"`
	Jobs        []model.Definition `toml:"job" json:"jobs"`
	Workers     []model.Definition `toml:"worker" json:"workers"`
	Definitions []model.Definition `toml:"-" json:"definitions"`
}

type Server struct {
	Bind       string `toml:"bind" json:"bind"`
	UnixSocket *bool  `toml:"unix_socket" json:"unix_socket,omitempty"`
}
type Include struct {
	Paths        []string `toml:"paths" json:"paths,omitempty"`
	PruneMissing bool     `toml:"prune_missing" json:"prune_missing"`
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

type includeFile struct {
	Defaults model.Definition   `toml:"defaults"`
	Jobs     []model.Definition `toml:"job"`
	Workers  []model.Definition `toml:"worker"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}$`)
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func Load(path string) (*Config, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := decode(path, &cfg); err != nil {
		return nil, err
	}
	cfg.Path = path
	applyConfigDefaults(&cfg)
	for i := range cfg.Jobs {
		cfg.Jobs[i].Kind = model.KindJob
		cfg.Jobs[i].Authority = "file"
		cfg.Jobs[i].SourceFile = path
		applyDefinitionDefaults(&cfg.Jobs[i], cfg.Defaults)
	}
	for i := range cfg.Workers {
		cfg.Workers[i].Kind = model.KindWorker
		cfg.Workers[i].Authority = "file"
		cfg.Workers[i].SourceFile = path
		applyDefinitionDefaults(&cfg.Workers[i], cfg.Defaults)
	}
	cfg.Definitions = append(cfg.Definitions, cfg.Jobs...)
	cfg.Definitions = append(cfg.Definitions, cfg.Workers...)
	for _, pattern := range cfg.Include.Paths {
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(filepath.Dir(path), pattern)
		}
		matches, globErr := filepath.Glob(pattern)
		if globErr != nil {
			return nil, fmt.Errorf("include %q: %w", pattern, globErr)
		}
		for _, included := range matches {
			var part includeFile
			if err := decode(included, &part); err != nil {
				return nil, err
			}
			for i := range part.Jobs {
				part.Jobs[i].Kind = model.KindJob
				part.Jobs[i].Authority = "file"
				part.Jobs[i].SourceFile = included
				applyDefinitionDefaults(&part.Jobs[i], mergeDefaults(cfg.Defaults, part.Defaults))
			}
			for i := range part.Workers {
				part.Workers[i].Kind = model.KindWorker
				part.Workers[i].Authority = "file"
				part.Workers[i].SourceFile = included
				applyDefinitionDefaults(&part.Workers[i], mergeDefaults(cfg.Defaults, part.Defaults))
			}
			cfg.Definitions = append(cfg.Definitions, part.Jobs...)
			cfg.Definitions = append(cfg.Definitions, part.Workers...)
		}
	}
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ParseImport(content []byte) ([]model.Definition, error) {
	var part includeFile
	dec := toml.NewDecoder(strings.NewReader(string(content)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&part); err != nil {
		return nil, err
	}
	cfg := Config{Scheduler: Scheduler{Timezone: "UTC"}, Logs: Logs{Backend: "file"}}
	applyConfigDefaults(&cfg)
	for i := range part.Jobs {
		part.Jobs[i].Kind = model.KindJob
		part.Jobs[i].Authority = "db"
		part.Jobs[i].SourceFile = "upload"
		applyDefinitionDefaults(&part.Jobs[i], part.Defaults)
	}
	for i := range part.Workers {
		part.Workers[i].Kind = model.KindWorker
		part.Workers[i].Authority = "db"
		part.Workers[i].SourceFile = "upload"
		applyDefinitionDefaults(&part.Workers[i], part.Defaults)
	}
	cfg.Definitions = append(cfg.Definitions, part.Jobs...)
	cfg.Definitions = append(cfg.Definitions, part.Workers...)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return cfg.Definitions, nil
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
	// Every field consumed here must also be merged by mergeDefaults, or
	// include-file defaults silently lose it.
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
	if len(d.SuccessCodes) == 0 {
		d.SuccessCodes = defaults.SuccessCodes
	}
	if len(d.SuccessCodes) == 0 {
		d.SuccessCodes = []int{0}
	}
	d.MaxRestartAttempts = cmp.Or(d.MaxRestartAttempts, defaults.MaxRestartAttempts, 5)
}

// mergeDefaults overlays include-file defaults (b) on top of bootstrap
// defaults (a): explicit values in b win, empty fields fall back to a.
func mergeDefaults(a, b model.Definition) model.Definition {
	b.Shell = cmp.Or(b.Shell, a.Shell)
	b.Timezone = cmp.Or(b.Timezone, a.Timezone)
	b.CatchUp = cmp.Or(b.CatchUp, a.CatchUp)
	b.OnOverlap = cmp.Or(b.OnOverlap, a.OnOverlap)
	b.EnvBase = cmp.Or(b.EnvBase, a.EnvBase)
	b.Grace = cmp.Or(b.Grace, a.Grace)
	b.Timeout = cmp.Or(b.Timeout, a.Timeout)
	b.StopSignal = cmp.Or(b.StopSignal, a.StopSignal)
	b.Restart = cmp.Or(b.Restart, a.Restart)
	b.RestartDelay = cmp.Or(b.RestartDelay, a.RestartDelay)
	b.HealthyAfter = cmp.Or(b.HealthyAfter, a.HealthyAfter)
	b.LogOnFull = cmp.Or(b.LogOnFull, a.LogOnFull)
	if len(b.SuccessCodes) == 0 {
		b.SuccessCodes = a.SuccessCodes
	}
	b.MaxRestartAttempts = cmp.Or(b.MaxRestartAttempts, a.MaxRestartAttempts)
	return b
}

func validate(c *Config) error {
	if _, err := time.LoadLocation(c.Scheduler.Timezone); err != nil {
		return fmt.Errorf("scheduler.timezone: %w", err)
	}
	if c.Logs.Backend != "file" {
		return errors.New("logs.backend: only file is supported in v0.1")
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
	seen := make(map[string]string)
	for i := range c.Definitions {
		d := &c.Definitions[i]
		if !namePattern.MatchString(d.Name) {
			return fmt.Errorf("%s: invalid name %q", d.SourceFile, d.Name)
		}
		if prior := seen[d.Name]; prior != "" {
			return fmt.Errorf("duplicate definition %q in %s and %s", d.Name, prior, d.SourceFile)
		}
		seen[d.Name] = d.SourceFile
		if (d.Command == "") == (len(d.Argv) == 0) {
			return fmt.Errorf("%s: %s: exactly one of command or argv is required", d.SourceFile, d.Name)
		}
		for field, value := range map[string]string{"timeout": d.Timeout, "grace": d.Grace, "restart_delay": d.RestartDelay, "healthy_after": d.HealthyAfter} {
			if _, err := time.ParseDuration(value); err != nil {
				return fmt.Errorf("%s: %s.%s: %w", d.SourceFile, d.Name, field, err)
			}
		}
		if d.Kind == model.KindJob && d.Schedule != "" {
			if err := ValidateSchedule(d.Schedule); err != nil {
				return fmt.Errorf("%s: %s.schedule: %w", d.SourceFile, d.Name, err)
			}
		}
		if d.Timezone == "" {
			d.Timezone = c.Scheduler.Timezone
		}
		if _, err := time.LoadLocation(d.Timezone); err != nil {
			return fmt.Errorf("%s: %s.timezone: %w", d.SourceFile, d.Name, err)
		}
		if d.CatchUp != "none" && d.CatchUp != "latest" {
			return fmt.Errorf("%s: %s.catch_up must be none or latest", d.SourceFile, d.Name)
		}
		if d.OnOverlap != "skip" && d.OnOverlap != "parallel" {
			return fmt.Errorf("%s: %s.on_overlap must be skip or parallel", d.SourceFile, d.Name)
		}
		if d.LogOnFull != "drop_old" && d.LogOnFull != "drop_new" {
			return fmt.Errorf("%s: %s.log_on_full must be drop_old or drop_new", d.SourceFile, d.Name)
		}
		if !validSignal(d.StopSignal) {
			return fmt.Errorf("%s: %s.stop_signal must be one of INT, HUP, QUIT, USR1, USR2, TERM, KILL", d.SourceFile, d.Name)
		}
		if d.KeepFor != "" {
			if keep, err := time.ParseDuration(d.KeepFor); err != nil || keep <= 0 {
				return fmt.Errorf("%s: %s.keep_for must be a positive duration", d.SourceFile, d.Name)
			}
		}
		if d.LogMax != "" {
			if _, err := logstore.ParseBytes(d.LogMax); err != nil {
				return fmt.Errorf("%s: %s.log_max must be a byte size like 64MiB", d.SourceFile, d.Name)
			}
		}
		if d.Kind == model.KindWorker && d.Schedule != "" {
			return fmt.Errorf("%s: worker %s cannot have schedule", d.SourceFile, d.Name)
		}
		if err := validateRunAs(d.RunAs); err != nil {
			return fmt.Errorf("%s: %s.run_as: %w", d.SourceFile, d.Name, err)
		}
	}
	return nil
}

func ValidateDefinition(d *model.Definition) error {
	if d.Kind == "" {
		d.Kind = model.KindJob
	}
	d.Authority = "db"
	d.SourceFile = "api"
	cfg := Config{Scheduler: Scheduler{Timezone: "UTC"}, Logs: Logs{Backend: "file"}, Definitions: []model.Definition{*d}}
	applyConfigDefaults(&cfg)
	applyDefinitionDefaults(&cfg.Definitions[0], model.Definition{})
	if err := validate(&cfg); err != nil {
		return err
	}
	*d = cfg.Definitions[0]
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
func validateRunAs(value string) error {
	if value == "" {
		return nil
	}
	name, _, _ := strings.Cut(value, ":")
	current, err := user.Current()
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 && name != current.Username && name != current.Uid {
		return errors.New("foreign identity requires a root daemon")
	}
	if _, err := strconv.Atoi(name); err == nil {
		_, err = user.LookupId(name)
		return err
	}
	_, err = user.Lookup(name)
	return err
}

func Canonical(d model.Definition) ([]byte, string, error) {
	d.ID, d.Revision = 0, 0
	d.Authority, d.SourceFile = "", ""
	b, err := json.Marshal(d)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:]), nil
}
