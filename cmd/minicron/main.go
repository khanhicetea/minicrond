package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/minicron/minicron/internal/config"
	"github.com/minicron/minicron/internal/daemon"
)

var version = "0.1.0-dev"
var commit = "unknown"

//go:embed minicron.schema.json
var configSchema []byte

func main() {
	if err := run(); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}
func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		return runDaemon(nil)
	}
	switch args[0] {
	case "daemon":
		return runDaemon(args[1:])
	case "init":
		return initConfig(args[1:])
	case "validate":
		return validate(args[1:])
	case "list":
		return getPrint("/api/v1/jobs")
	case "status":
		return getPrint("/api/v1/daemon")
	case "reload":
		return postPrint("/api/v1/daemon/reload", nil)
	case "run":
		return trigger(args[1:])
	case "logs":
		return logs(args[1:])
	case "export":
		return exportConfig(args[1:])
	case "import":
		return importConfig(args[1:])
	case "token":
		return token(args[1:])
	case "schema":
		_, _ = os.Stdout.Write(append(configSchema, '\n'))
		return nil
	case "version":
		fmt.Printf("minicron %s (%s) %s/%s\n", version, commit, runtime.GOOS, runtime.GOARCH)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	configPath := fs.String("config", env("MINICRON_CONFIG", "minicron.toml"), "bootstrap TOML path")
	dataDir := fs.String("data-dir", env("MINICRON_DATA", defaultDataDir()), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	d := &daemon.Daemon{ConfigPath: *configPath, DataDir: *dataDir, Version: version}
	go func() {
		for range hup {
			if err := d.Reload(context.Background()); err != nil {
				slog.Error("reload failed", "error", err)
			}
		}
	}()
	return d.Run(ctx)
}
func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	path := fs.String("config", env("MINICRON_CONFIG", "minicron.toml"), "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*path); err == nil {
		return fmt.Errorf("%s already exists", *path)
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o700); err != nil && filepath.Dir(*path) != "." {
		return err
	}
	content := `[server]
bind = "127.0.0.1:7423"
unix_socket = true

[include]
paths = ["jobs/*.toml"]

[scheduler]
timezone = "UTC"
max_concurrent_runs = 32

[logs]
backend = "file"
worker_flush_interval = "15m"
db_prune_at = "03:30"
db_keep_for = "720h"

[defaults]
shell = "/bin/sh"
env_base = "clean"
grace = "10s"
`
	if err := os.WriteFile(*path, []byte(content), 0o600); err != nil {
		return err
	}
	jobs := filepath.Join(filepath.Dir(*path), "jobs")
	if err := os.MkdirAll(jobs, 0o700); err != nil {
		return err
	}
	example := `[[job]]
name = "hello"
schedule = "@every 1h"
argv = ["/bin/echo", "hello from minicron"]
catch_up = "none"
on_overlap = "skip"
`
	if err := os.WriteFile(filepath.Join(jobs, "hello.toml"), []byte(example), 0o600); err != nil {
		return err
	}
	fmt.Printf("created %s\n", *path)
	return nil
}
func validate(args []string) error {
	path := env("MINICRON_CONFIG", "minicron.toml")
	if len(args) > 0 {
		path = args[0]
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Printf("valid: %d definitions\n", len(cfg.Definitions))
	return nil
}
func trigger(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: minicron run NAME [--wait]")
	}
	wait := false
	for _, a := range args[1:] {
		if a == "--wait" {
			wait = true
		}
	}
	path := "/api/v1/jobs/" + args[0] + "/trigger"
	if wait {
		path += "?wait=true"
	}
	return postPrint(path, nil)
}
func logs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: minicron logs RUN_ID [--follow]")
	}
	follow := false
	for _, a := range args[1:] {
		if a == "--follow" || a == "-f" {
			follow = true
		}
	}
	var after uint64
	for {
		var response struct {
			Items []struct {
				Sequence uint64 `json:"sequence"`
				Stream   byte   `json:"stream"`
				Payload  string `json:"payload"`
			} `json:"items"`
		}
		if err := requestJSON("GET", fmt.Sprintf("/api/v1/runs/%s/log?after=%d&limit=1000", args[0], after), nil, &response); err != nil {
			return err
		}
		for _, f := range response.Items {
			b, _ := base64.StdEncoding.DecodeString(f.Payload)
			if f.Stream == 2 {
				fmt.Print("[err] ")
			}
			fmt.Println(string(b))
			after = f.Sequence
		}
		if !follow {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}
func token(args []string) error {
	if len(args) == 0 || args[0] != "--rotate" {
		return fmt.Errorf("the current token cannot be recovered; use minicron token --rotate")
	}
	return postPrint("/api/v1/token/rotate", nil)
}
func importConfig(args []string) error {
	if len(args) < 2 || (args[0] != "--link" && args[0] != "--copy") {
		return fmt.Errorf("usage: minicron import --link|--copy PATH")
	}
	path, err := filepath.Abs(args[1])
	if err != nil {
		return err
	}
	if args[0] == "--link" {
		cfgPath := env("MINICRON_CONFIG", "minicron.toml")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		cfg.Include.Paths = append(cfg.Include.Paths, path)
		b, err := toml.Marshal(cfg)
		if err != nil {
			return err
		}
		tmp := cfgPath + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if _, err = config.Load(tmp); err != nil {
			os.Remove(tmp)
			return err
		}
		if err = os.Rename(tmp, cfgPath); err != nil {
			return err
		}
		return postPrint("/api/v1/daemon/reload", nil)
	}
	tmpDir, err := os.MkdirTemp("", "minicron-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bootstrap := filepath.Join(tmpDir, "minicron.toml")
	if err = os.WriteFile(bootstrap, []byte("[include]\npaths = ["+strconv.Quote(path)+"]\n"), 0o600); err != nil {
		return err
	}
	cfg, err := config.Load(bootstrap)
	if err != nil {
		return err
	}
	for _, d := range cfg.Definitions {
		d.Authority = "db"
		d.SourceFile = ""
		var out any
		if err = requestJSON("POST", "/api/v1/jobs", d, &out); err != nil {
			return err
		}
	}
	fmt.Printf("copied %d definitions\n", len(cfg.Definitions))
	return nil
}
func exportConfig(args []string) error {
	format := "toml"
	for i, arg := range args {
		if arg == "--format" && i+1 < len(args) {
			format = args[i+1]
		}
	}
	base := strings.TrimSuffix(env("MINICRON_URL", "http://minicron"), "/")
	req, err := http.NewRequest("GET", base+"/api/v1/export?format="+format, nil)
	if err != nil {
		return err
	}
	if token := os.Getenv("MINICRON_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %s: %s", resp.Status, string(b))
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func getPrint(path string) error            { return requestPrint("GET", path, nil) }
func postPrint(path string, body any) error { return requestPrint("POST", path, body) }
func requestPrint(method, path string, body any) error {
	var out any
	if err := requestJSON(method, path, body, &out); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}
func requestJSON(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	base := env("MINICRON_URL", "http://minicron")
	req, err := http.NewRequest(method, strings.TrimSuffix(base, "/")+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := client()
	if token := os.Getenv("MINICRON_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %s: %s", resp.Status, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
func client() *http.Client {
	if os.Getenv("MINICRON_URL") != "" {
		return http.DefaultClient
	}
	socket := filepath.Join(env("MINICRON_DATA", defaultDataDir()), "minicron.sock")
	tr := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: tr}
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "minicron")
}
func usage() {
	fmt.Println("minicron: trustworthy local job scheduler\ncommands: daemon init validate list run logs reload import export status token version")
}
