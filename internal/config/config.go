package config

import (
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
}
func applyDefinitionDefaults(d *model.Definition, defaults model.Definition) {
	if d.Shell == "" {
		d.Shell = first(defaults.Shell, "/bin/sh")
	}
	if d.Timezone == "" {
		d.Timezone = defaults.Timezone
	}
	if d.CatchUp == "" {
		d.CatchUp = first(defaults.CatchUp, "none")
	}
	if d.OnOverlap == "" {
		d.OnOverlap = first(defaults.OnOverlap, "skip")
	}
	if d.EnvBase == "" {
		d.EnvBase = first(defaults.EnvBase, "clean")
	}
	if d.Grace == "" {
		d.Grace = first(defaults.Grace, "10s")
	}
	if d.Timeout == "" {
		d.Timeout = first(defaults.Timeout, "0")
	}
	if d.StopSignal == "" {
		d.StopSignal = first(defaults.StopSignal, "SIGTERM")
	}
	if len(d.SuccessCodes) == 0 {
		d.SuccessCodes = []int{0}
	}
	if d.Restart == "" {
		d.Restart = "always"
	}
	if d.RestartDelay == "" {
		d.RestartDelay = "5s"
	}
	if d.MaxRestartAttempts == 0 {
		d.MaxRestartAttempts = 5
	}
	if d.HealthyAfter == "" {
		d.HealthyAfter = "30s"
	}
	if d.LogOnFull == "" {
		d.LogOnFull = "drop_old"
	}
}
func mergeDefaults(a, b model.Definition) model.Definition {
	if b.Shell == "" {
		b.Shell = a.Shell
	}
	if b.Grace == "" {
		b.Grace = a.Grace
	}
	if b.Timeout == "" {
		b.Timeout = a.Timeout
	}
	if len(b.SuccessCodes) == 0 {
		b.SuccessCodes = a.SuccessCodes
	}
	return b
}
func first(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func validate(c *Config) error {
	if _, err := time.LoadLocation(c.Scheduler.Timezone); err != nil {
		return fmt.Errorf("scheduler.timezone: %w", err)
	}
	if c.Logs.Backend != "file" {
		return errors.New("logs.backend: only file is supported in v0.1")
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
