package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"sync"
	"syscall"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/daemon"
	"github.com/khanhicetea/minicrond/internal/fault"
)

var version = "0.2.0-dev"
var commit = "unknown"

//go:embed minicron.schema.json
var configSchema []byte

func main() {
	if err := run(); err != nil {
		attrs := []any{"error", err}
		if panicErr, ok := errors.AsType[*fault.PanicError](err); ok {
			attrs = append(attrs, "stack", string(panicErr.Stack))
		}
		slog.Error("command failed", attrs...)
		os.Exit(1)
	}
}
func run() error {
	defer closeClient()
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
		return writeOutput("%s\n", configSchema)
	case "version":
		return writeOutput("minicrond %s (%s) %s/%s\n", version, commit, runtime.GOOS, runtime.GOARCH)
	case "help", "--help", "-h":
		return usage()
	default:
		return errors.Join(fmt.Errorf("unknown command %q", args[0]), usage())
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
	reloadErrors := make(chan error, 1)
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := fault.Call(func() error { return d.Reload(ctx) }); err != nil {
					if _, ok := errors.AsType[*fault.PanicError](err); ok {
						reloadErrors <- fmt.Errorf("reload daemon: %w", err)
						stop()
						return
					}
					slog.Error("reload failed", "error", err)
				}
			}
		}
	}()
	defer func() {
		stop()
		<-reloadDone
	}()
	runErr := d.Run(ctx)
	stop()
	<-reloadDone
	select {
	case err := <-reloadErrors:
		return errors.Join(runErr, err)
	default:
		return runErr
	}
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
	err = errors.Join(err, f.Close())
	if err != nil {
		return errors.Join(fmt.Errorf("write initial config: %w", err), os.Remove(*path))
	}
	return writeOutput("created %s\n", *path)
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
	return writeOutput("valid configuration\n")
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
	return followLogs(args[0], follow, sleepFor)
}

// followPollInterval is the pause between polls once a followed log has been
// caught up. Display lag of 1–3 s is acceptable (ADR-8); a tight loop is not.
const followPollInterval = 2 * time.Second

// sleepFor is the production wait used between follow polls.
func sleepFor(d time.Duration) error {
	time.Sleep(d)
	return nil
}

// followLogs prints a run's log frames in pages. Without follow it returns once
// the stored log is exhausted and never polls. With follow it waits
// followPollInterval via wait between polls; wait returning an error stops it.
func followLogs(runID string, follow bool, wait func(time.Duration) error) error {
	var after uint64
	for {
		var response struct {
			Items []struct {
				Sequence uint64 `json:"sequence"`
				Stream   byte   `json:"stream"`
				Payload  string `json:"payload"`
			} `json:"items"`
		}
		if err := requestJSON("GET", fmt.Sprintf("/api/v1/runs/%s/log?after=%d&limit=1000", runID, after), nil, &response); err != nil {
			return err
		}
		for _, f := range response.Items {
			b, err := base64.StdEncoding.DecodeString(f.Payload)
			if err != nil {
				return fmt.Errorf("decode log frame %d: %w", f.Sequence, err)
			}
			prefix := ""
			if f.Stream == 2 {
				prefix = "[err] "
			}
			if err := writeOutput("%s%s\n", prefix, b); err != nil {
				return err
			}
			after = f.Sequence
		}
		if len(response.Items) > 0 {
			continue
		}
		if !follow {
			return nil
		}
		if err := wait(followPollInterval); err != nil {
			return err
		}
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
	return writeOutput("imported %d definitions\n", result.Applied)
}
func exportConfig(args []string) (err error) {
	format := "toml"
	for i, arg := range args {
		if arg == "--format" && i+1 < len(args) {
			format = args[i+1]
		}
	}
	base := apiBaseURL()
	req, err := http.NewRequest("GET", base+"/api/v1/export?format="+format, nil)
	if err != nil {
		return fmt.Errorf("create export request: %w", err)
	}
	if token := os.Getenv("MINICRON_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("send export request: %w", err)
	}
	defer closeResponse(resp.Body, &err)
	if resp.StatusCode >= 300 {
		return responseError(resp)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil {
		return fmt.Errorf("write exported configuration: %w", err)
	}
	return nil
}

func getPrint(path string) error            { return requestPrint("GET", path, nil) }
func postPrint(path string, body any) error { return requestPrint("POST", path, body) }
func requestPrint(method, path string, body any) error {
	var out any
	if err := requestJSON(method, path, body, &out); err != nil {
		return err
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("format response: %w", err)
	}
	return writeOutput("%s\n", b)
}
func requestJSON(method, path string, body, out any) (err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	base := apiBaseURL()
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return fmt.Errorf("create %s request: %w", method, err)
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
		return fmt.Errorf("send %s request: %w", method, err)
	}
	defer closeResponse(resp.Body, &err)
	if resp.StatusCode >= 300 {
		return responseError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// maxDrainBytes bounds how much unread response is discarded so the
// connection can be reused for the next request.
const maxDrainBytes = 64 << 10

func closeResponse(body io.ReadCloser, result *error) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
	if err := body.Close(); err != nil {
		*result = errors.Join(*result, fmt.Errorf("close response body: %w", err))
	}
}

func responseError(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	statusErr := fmt.Errorf("HTTP %s: %s", resp.Status, string(body))
	if err != nil {
		return errors.Join(statusErr, fmt.Errorf("read error response body: %w", err))
	}
	return statusErr
}

func writeOutput(format string, args ...any) error {
	if _, err := fmt.Fprintf(os.Stdout, format, args...); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}

// The CLI keeps one client (and therefore one transport and connection pool)
// per command invocation, keyed by its target so tests that change the
// environment get a fresh one. run() closes its idle connections on exit.
var cliHTTP struct {
	mu     sync.Mutex
	key    string
	client *http.Client
}

func client() *http.Client {
	baseURL := os.Getenv("MINICRON_URL")
	socket := filepath.Join(env("MINICRON_DATA", defaultDataDir()), "minicron.sock")
	key := "unix:" + socket
	if baseURL != "" {
		key = "url:" + baseURL
	}
	cliHTTP.mu.Lock()
	defer cliHTTP.mu.Unlock()
	if cliHTTP.client != nil {
		if cliHTTP.key == key {
			return cliHTTP.client
		}
		cliHTTP.client.CloseIdleConnections()
	}
	var tr *http.Transport
	if baseURL != "" {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		tr = &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
	}
	// Requests are sequential, so a couple of idle connections is plenty.
	tr.MaxIdleConns = 2
	tr.MaxIdleConnsPerHost = 2
	tr.IdleConnTimeout = 30 * time.Second
	cliHTTP.key = key
	cliHTTP.client = &http.Client{Transport: tr, Timeout: 5 * time.Minute}
	return cliHTTP.client
}

// closeClient closes idle connections of the shared CLI client and drops it.
func closeClient() {
	cliHTTP.mu.Lock()
	defer cliHTTP.mu.Unlock()
	if cliHTTP.client != nil {
		cliHTTP.client.CloseIdleConnections()
		cliHTTP.client = nil
	}
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
func usage() error {
	return writeOutput("minicrond: trustworthy local job scheduler\ncommands: daemon init validate list run logs reload import crontab export status token service version\nservice: install/uninstall systemd units (root daemon or per-user daemons; run as root)\n")
}
