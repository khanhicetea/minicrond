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
	"strings"
	"syscall"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/daemon"
)

var version = "0.2.0-dev"
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
	case "crontab":
		return crontab(args[1:])
	case "token":
		return token(args[1:])
	case "service":
		return runService(args[1:])
	case "schema":
		_, _ = os.Stdout.Write(append(configSchema, '\n'))
		return nil
	case "version":
		fmt.Printf("minicrond %s (%s) %s/%s\n", version, commit, runtime.GOOS, runtime.GOARCH)
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
	defer signal.Stop(hup)
	d := &daemon.Daemon{ConfigPath: *configPath, DataDir: *dataDir, Version: version}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := d.Reload(context.Background()); err != nil {
					slog.Error("reload failed", "error", err)
				}
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
	if err := os.MkdirAll(filepath.Dir(*path), 0o700); err != nil && filepath.Dir(*path) != "." {
		return err
	}
	content := defaultConfigTOML("127.0.0.1:7423")
	f, err := os.OpenFile(*path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(f, content); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(*path)
		return err
	}
	fmt.Printf("created %s\n", *path)
	return nil
}
func defaultConfigTOML(bind string) string {
	return fmt.Sprintf(`[server]
bind = %q
tcp_enabled = true
unix_socket = true

[scheduler]
timezone = "UTC"
max_concurrent_runs = 32

[logs]
backend = "file"
worker_flush_interval = 15 # minutes
db_prune_at = "03:30"
db_keep_for = 30 # days
`, bind)
}
func validate(args []string) error {
	path := env("MINICRON_CONFIG", "minicron.toml")
	if len(args) > 0 {
		path = args[0]
	}
	_, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Println("valid configuration")
	return nil
}
func trigger(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: minicrond run NAME [--wait]")
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
		return fmt.Errorf("usage: minicrond logs RUN_ID [--follow]")
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
		if len(response.Items) > 0 {
			continue
		}
		if !follow {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}
func token(args []string) error {
	if len(args) == 0 || args[0] != "--rotate" {
		return fmt.Errorf("the current token cannot be recovered; use minicrond token --rotate")
	}
	return postPrint("/api/v1/token/rotate", nil)
}
func importConfig(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: minicrond import PATH")
	}
	content, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	request := map[string]string{"content": string(content)}
	var preview struct {
		ContentHash string `json:"content_hash"`
	}
	if err := requestJSON("POST", "/api/v1/import/preview", request, &preview); err != nil {
		return err
	}
	request["hash"] = preview.ContentHash
	var result struct {
		Applied int `json:"applied"`
	}
	if err := requestJSON("POST", "/api/v1/import/apply", request, &result); err != nil {
		return err
	}
	fmt.Printf("imported %d definitions\n", result.Applied)
	return nil
}
func exportConfig(args []string) error {
	format := "toml"
	for i, arg := range args {
		if arg == "--format" && i+1 < len(args) {
			format = args[i+1]
		}
	}
	base := apiBaseURL()
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
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
	base := apiBaseURL()
	req, err := http.NewRequest(method, base+path, reader)
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("HTTP %s: %s", resp.Status, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
func client() *http.Client {
	if os.Getenv("MINICRON_URL") != "" {
		return &http.Client{Timeout: 5 * time.Minute}
	}
	socket := filepath.Join(env("MINICRON_DATA", defaultDataDir()), "minicron.sock")
	tr := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: tr, Timeout: 5 * time.Minute}
}
func apiBaseURL() string {
	if url := os.Getenv("MINICRON_URL"); url != "" {
		return strings.TrimSuffix(url, "/")
	}
	return "http://minicron" + strings.TrimSuffix(os.Getenv("BASE_PATH"), "/")
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
	fmt.Println("minicrond: trustworthy local job scheduler\ncommands: daemon init validate list run logs reload import crontab export status token service version")
	fmt.Println("service: install/uninstall systemd units (root daemon or per-user daemons; run as root)")
}
