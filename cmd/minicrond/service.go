package main

// The service command provisions the default config file and the systemd
// unit for a minicrond daemon. It must run as root: it writes under /etc
// and /var, and user installs chown files inside other users' homes.
//
// Root daemon: config /etc/minicrond/config.toml, unit "minicrond"
//              (runs as root, default port 7423).
// User daemon: config ~USER/.minicrond/config.toml, unit "minicrond@USER"
//              (runs as USER via User=/Group=). --port is required so every
//              daemon on the host binds a unique TCP port.

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const serviceUnitHeader = "# managed by minicrond service install"

const (
	systemUnitName   = "minicrond"
	systemConfigPath = "/etc/minicrond/config.toml"
	systemDataDir    = "/var/lib/minicron"
	systemUnitDir    = "/etc/systemd/system"
	defaultAPIPort   = 7423
)

type servicePlan struct {
	unitName   string
	configPath string
	dataDir    string
	port       int
	runAs      *user.User // nil for the root daemon
}

func runService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: minicrond service install|uninstall [--user NAME] [--port N] [--force]")
	}
	if os.Geteuid() != 0 {
		return errors.New("the service command must be run as root")
	}
	switch args[0] {
	case "install":
		return serviceInstall(args[1:])
	case "uninstall":
		return serviceUninstall(args[1:])
	default:
		return fmt.Errorf("unknown service command %q (expected install or uninstall)", args[0])
	}
}

func serviceInstall(args []string) error {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	userName := fs.String("user", "", "install a per-user daemon running as this user (default: the root daemon)")
	port := fs.Int("port", 0, "API TCP port (required with --user; defaults to 7423 for the root daemon)")
	force := fs.Bool("force", false, "replace an existing unit file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	execPath, err := currentBinary()
	if err != nil {
		return err
	}
	plan, err := newServicePlan(*userName, *port)
	if err != nil {
		return err
	}
	if err := checkPortConflicts(plan); err != nil {
		return err
	}
	if err := writeServiceConfig(plan); err != nil {
		return err
	}
	unitPath := filepath.Join(systemUnitDir, plan.unitName+".service")
	if err := writeUnitFile(unitPath, plan.unit(execPath), *force); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--now", plan.unitName); err != nil {
		return err
	}
	fmt.Printf("installed %s\n", unitPath)
	fmt.Printf("config %s (bind 127.0.0.1:%d), data %s\n", plan.configPath, plan.port, plan.dataDir)
	if plan.runAs == nil {
		fmt.Printf("CLI note: local commands need MINICRON_DATA=%s (or MINICRON_URL/MINICRON_TOKEN over HTTP)\n", systemDataDir)
	} else {
		fmt.Printf("the daemon runs as %s; run CLI commands as that user\n", plan.runAs.Username)
	}
	return nil
}

func serviceUninstall(args []string) error {
	fs := flag.NewFlagSet("service uninstall", flag.ContinueOnError)
	userName := fs.String("user", "", "uninstall the per-user daemon of this user (default: the root daemon)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	unitName, configPath, dataDir := systemUnitName, systemConfigPath, systemDataDir
	if *userName != "" {
		if err := validateUserName(*userName); err != nil {
			return err
		}
		u, err := user.Lookup(*userName)
		if err != nil {
			return fmt.Errorf("unknown user %q", *userName)
		}
		unitName = systemUnitName + "@" + u.Username
		configPath = filepath.Join(u.HomeDir, ".minicrond", "config.toml")
		dataDir = filepath.Join(u.HomeDir, ".local", "share", "minicron")
	}
	unitPath := filepath.Join(systemUnitDir, unitName+".service")
	if _, err := os.Stat(unitPath); err != nil {
		return fmt.Errorf("no unit file at %s", unitPath)
	}
	if err := systemctl("disable", "--now", unitName); err != nil {
		fmt.Printf("warning: could not disable/stop %s: %v\n", unitName, err)
	}
	if err := os.Remove(unitPath); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	// Best effort; the unit no longer exists, so discard systemctl's noise.
	_ = exec.Command("systemctl", "reset-failed", unitName).Run()
	fmt.Printf("removed %s\n", unitPath)
	fmt.Printf("kept config %s and data %s; delete them manually to purge\n", configPath, dataDir)
	return nil
}

func newServicePlan(userName string, port int) (*servicePlan, error) {
	if userName == "" {
		if port == 0 {
			port = defaultAPIPort
		}
		if err := validatePort(port); err != nil {
			return nil, err
		}
		return &servicePlan{
			unitName:   systemUnitName,
			configPath: systemConfigPath,
			dataDir:    systemDataDir,
			port:       port,
		}, nil
	}
	if err := validateUserName(userName); err != nil {
		return nil, err
	}
	u, err := user.Lookup(userName)
	if err != nil {
		return nil, fmt.Errorf("unknown user %q", userName)
	}
	if u.Uid == "0" {
		return nil, errors.New("--user must name a non-root user; install the root daemon without --user")
	}
	if port == 0 {
		return nil, errors.New("--port is required for a user daemon so each daemon binds a unique TCP port")
	}
	if err := validatePort(port); err != nil {
		return nil, err
	}
	if u.HomeDir == "" || !filepath.IsAbs(u.HomeDir) {
		return nil, fmt.Errorf("user %q has no absolute home directory", userName)
	}
	return &servicePlan{
		unitName:   systemUnitName + "@" + u.Username,
		configPath: filepath.Join(u.HomeDir, ".minicrond", "config.toml"),
		dataDir:    filepath.Join(u.HomeDir, ".local", "share", "minicron"),
		port:       port,
		runAs:      u,
	}, nil
}

// unit renders the systemd unit file for the plan.
func (p *servicePlan) unit(execPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n[Unit]\n", serviceUnitHeader)
	if p.runAs == nil {
		b.WriteString("Description=minicrond job scheduler (system)\n")
	} else {
		fmt.Fprintf(&b, "Description=minicrond job scheduler (user %s)\n", p.runAs.Username)
	}
	b.WriteString("After=network.target\n\n[Service]\nType=simple\n")
	if p.runAs != nil {
		fmt.Fprintf(&b, "User=%s\nGroup=%s\n", p.runAs.Username, p.runAs.Gid)
	}
	fmt.Fprintf(&b, "ExecStart=%s daemon --config %s --data-dir %s\n", execPath, p.configPath, p.dataDir)
	b.WriteString("Restart=on-failure\nRestartSec=5s\n\n[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}

// writeServiceConfig creates the default config with the planned bind port.
// An existing config is kept as-is.
func writeServiceConfig(p *servicePlan) error {
	if _, err := os.Stat(p.configPath); err == nil {
		if existing := configBindPort(p.configPath); existing != 0 && existing != p.port {
			return fmt.Errorf("config %s already binds port %d but %d was requested; edit or remove the config, or retry with --port %d", p.configPath, existing, p.port, existing)
		}
		fmt.Printf("config %s already exists; keeping it\n", p.configPath)
		return nil
	}
	dir := filepath.Dir(p.configPath)
	dirPerm := os.FileMode(0o755)
	if p.runAs != nil {
		dirPerm = 0o700
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	bind := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
	if err := os.WriteFile(p.configPath, []byte(defaultConfigTOML(bind)), 0o600); err != nil {
		return err
	}
	if p.runAs != nil {
		uid, err := strconv.Atoi(p.runAs.Uid)
		if err != nil {
			return err
		}
		gid, err := strconv.Atoi(p.runAs.Gid)
		if err != nil {
			return err
		}
		for _, path := range []string{dir, p.configPath} {
			if err := os.Chown(path, uid, gid); err != nil {
				return err
			}
		}
	}
	fmt.Printf("created %s\n", p.configPath)
	return nil
}

func writeUnitFile(path, content string, force bool) error {
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) == content {
			fmt.Printf("unit %s already up to date\n", path)
			return nil
		}
		if !force {
			return fmt.Errorf("%s already exists with different content; use --force to replace it", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// checkPortConflicts rejects a plan whose port is already claimed by another
// installed minicrond daemon, or by an unrelated listener on the loopback
// bind address. Reinstalling the same (running) unit is allowed.
func checkPortConflicts(p *servicePlan) error {
	claimed, err := installedDaemonPorts(p.unitName)
	if err != nil {
		return err
	}
	for unit, port := range claimed {
		if port == p.port {
			return fmt.Errorf("port %d is already used by the %s daemon; pass a different --port", p.port, unit)
		}
	}
	if unitIsActive(p.unitName) {
		return nil // reinstalling over our own running daemon
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port)))
	if err != nil {
		return fmt.Errorf("port %d is already in use on 127.0.0.1; pass a different --port", p.port)
	}
	return ln.Close()
}

// installedDaemonPorts scans installed minicrond unit files and returns the
// API port claimed by each daemon, read from the config referenced in its
// ExecStart line. Units named excludeUnit are skipped.
func installedDaemonPorts(excludeUnit string) (map[string]int, error) {
	ports := map[string]int{}
	matches, err := filepath.Glob(filepath.Join(systemUnitDir, "minicrond*.service"))
	if err != nil {
		return nil, err
	}
	for _, unitPath := range matches {
		unit := strings.TrimSuffix(filepath.Base(unitPath), ".service")
		if unit == excludeUnit {
			continue
		}
		configPath := unitConfigPath(unitPath)
		if configPath == "" {
			continue
		}
		if port := configBindPort(configPath); port > 0 {
			ports[unit] = port
		}
	}
	return ports, nil
}

// unitConfigPath extracts the --config path from a managed unit's ExecStart.
func unitConfigPath(unitPath string) string {
	b, err := os.ReadFile(unitPath)
	if err != nil || !strings.Contains(string(b), serviceUnitHeader) {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		for i, field := range fields {
			if field == "--config" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	return ""
}

var bindPattern = regexp.MustCompile(`(?m)^\s*bind\s*=\s*"([^"]+)"`)

// configBindPort reads server.bind from a config file and returns its port
// (0 when the file is missing or has no usable bind).
func configBindPort(configPath string) int {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return 0
	}
	m := bindPattern.FindSubmatch(b)
	if m == nil {
		return 0
	}
	_, port, err := net.SplitHostPort(string(m[1]))
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return p
}

var userNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*\$?$`)

func validateUserName(name string) error {
	if !userNamePattern.MatchString(name) {
		return fmt.Errorf("invalid user name %q", name)
	}
	return nil
}

func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d is out of range (1-65535)", port)
	}
	return nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func unitIsActive(unitName string) bool {
	out, err := exec.Command("systemctl", "is-active", unitName).Output()
	return err == nil && strings.EqualFold(strings.TrimSpace(string(out)), "active")
}

func currentBinary() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Abs(path)
}
