package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"uuid"

	"github.com/pelletier/go-toml/v2"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
	"github.com/khanhicetea/minicrond/internal/supervisor"
)

type Server struct {
	store     *store.Store
	logs      *logstore.Store
	exec      *executor.Service
	super     *supervisor.Supervisor
	reload    func(context.Context) error
	reconcile func(context.Context) error
	started   time.Time
	version   string
	ready     atomic.Bool
	closing   atomic.Bool
	// tokenHash holds the hex SHA-256 of the active bearer token; the raw
	// token exists only at rotation time.
	tokenHash       atomic.Pointer[string]
	tcp             *http.Server
	unix            *http.Server
	idemMu          sync.Mutex
	tokenMu         sync.Mutex
	tcpEnabled      bool
	basePath        string
	streamSlots     chan struct{}
	alertChannels   func() []config.AlertChannel
	jobDefaults     func() model.Definition
	testAlert       func(context.Context, string) error
	alertQueueDepth func() int
	serveErrors     chan error
}

func (s *Server) currentTokenHash() string {
	if p := s.tokenHash.Load(); p != nil {
		return *p
	}
	return ""
}
func (s *Server) setTokenHash(hash string) { s.tokenHash.Store(&hash) }

//go:embed assets/*
var webAssets embed.FS

type cachedAsset struct {
	body []byte
	gzip []byte
	etag string
}

var cachedAssets = sync.OnceValue(func() map[string]cachedAsset {
	out := make(map[string]cachedAsset)
	entries, err := webAssets.ReadDir("assets")
	if err != nil {
		return out
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (name != "index.html" && !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".css")) {
			continue
		}
		body, err := webAssets.ReadFile("assets/" + name)
		if err != nil {
			continue
		}
		asset := cachedAsset{body: body, etag: `"` + hex.EncodeToString(sha256Sum(body))[:16] + `"`}
		if name != "index.html" {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(body); err == nil {
				err = writer.Close()
			}
			if err == nil {
				asset.gzip = compressed.Bytes()
			}
		}
		out[name] = asset
	}
	return out
})

type localKey struct{}

// Charts emit style attributes, but do not need inline stylesheet blocks.
// The SPA submits forms through fetch and needs its same-origin <base> tag
// for deployments under BASE_PATH.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; script-src-attr 'none'; " +
	"style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; " +
	"base-uri 'self'; object-src 'none'; frame-src 'none'; frame-ancestors 'none'; " +
	"form-action 'none'; worker-src 'none'"

type jobListItem struct {
	model.Definition
	NextFireAt *time.Time `json:"next_fire_at,omitzero"`
}

type peerListener struct {
	net.Listener
	uid uint32
}

func (l peerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(conn)
		if err == nil && (uid == l.uid || uid == 0) {
			return conn, nil
		}
		_ = conn.Close()
	}
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func New(st *store.Store, logs *logstore.Store, ex *executor.Service, sup *supervisor.Supervisor, reload, reconcile func(context.Context) error, version string) *Server {
	return &Server{store: st, logs: logs, exec: ex, super: sup, reload: reload, reconcile: reconcile, started: time.Now(), version: version, streamSlots: make(chan struct{}, 64), tcpEnabled: true, serveErrors: make(chan error, 2)}
}

// Errors reports unexpected listener failures so the daemon can stop cleanly.
func (s *Server) Errors() <-chan error { return s.serveErrors }

func (s *Server) serveFailed(listener string, err error) {
	s.ready.Store(false)
	s.serveErrors <- fmt.Errorf("serve %s HTTP listener: %w", listener, err)
}

func (s *Server) serve(name string, server *http.Server, listener net.Listener) {
	err := fault.Call(func() error { return server.Serve(listener) })
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.serveFailed(name, err)
	}
}

// SetTCPEnabled must be called before InitializeToken or Start.
func (s *Server) SetTCPEnabled(enabled bool) { s.tcpEnabled = enabled }

// SetBasePath configures the public URL prefix before the HTTP listeners start.
func (s *Server) SetBasePath(value string) error {
	value = strings.TrimSuffix(value, "/")
	if value == "" {
		s.basePath = ""
		return nil
	}
	if !strings.HasPrefix(value, "/") || strings.Contains(value, "//") {
		return fmt.Errorf("BASE_PATH must be an absolute URL path")
	}
	for _, segment := range strings.Split(value[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("BASE_PATH must not contain dot segments")
		}
		for _, char := range segment {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("-._~", char)) {
				return fmt.Errorf("BASE_PATH contains an invalid URL path character %q", char)
			}
		}
	}
	s.basePath = value
	return nil
}

// SetAlertChannels provides a redacted view of the live channel registry.
func (s *Server) SetJobDefaults(get func() model.Definition)            { s.jobDefaults = get }
func (s *Server) SetAlertChannels(list func() []config.AlertChannel)    { s.alertChannels = list }
func (s *Server) SetAlertTest(test func(context.Context, string) error) { s.testAlert = test }
func (s *Server) SetAlertQueueDepth(depth func() int)                   { s.alertQueueDepth = depth }

func (s *Server) validateAlerts(defs []model.Definition) error {
	if s.alertChannels == nil {
		return nil
	}
	available := make(map[string]bool)
	for _, ch := range s.alertChannels() {
		available[ch.Name] = true
	}
	for _, def := range defs {
		seen := make(map[string]bool)
		for _, name := range def.Alerts {
			if !available[name] {
				return fmt.Errorf("%s.alerts: unknown channel %q", def.Name, name)
			}
			if seen[name] {
				return fmt.Errorf("%s.alerts: duplicate channel %q", def.Name, name)
			}
			seen[name] = true
		}
	}
	return nil
}

func (s *Server) InitializeToken(ctx context.Context) (string, error) {
	if !s.tcpEnabled {
		return "", nil
	}
	hash, err := s.store.Meta(ctx, "token_hash")
	if err == nil {
		s.setTokenHash(hash)
		return "", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return s.RotateToken(ctx)
}
func (s *Server) RotateToken(ctx context.Context) (string, error) {
	if !s.tcpEnabled {
		return "", errors.New("bearer tokens are disabled while TCP is disabled")
	}
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	if err := s.store.SetMeta(ctx, "token_hash", hash); err != nil {
		return "", err
	}
	s.setTokenHash(hash)
	return token, nil
}
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// BeginShutdown rejects new API operations while existing handlers finish.
func (s *Server) BeginShutdown() {
	s.closing.Store(true)
	s.ready.Store(false)
}

func (s *Server) Start(bind, socket string) error {
	if !s.tcpEnabled && socket == "" {
		return errors.New("no HTTP listener enabled")
	}
	mux := s.routes()
	var ln net.Listener
	if s.tcpEnabled {
		s.tcp = &http.Server{Addr: bind, Handler: s.middleware(mux, false), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: responseWriteTimeout, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		var err error
		ln, err = net.Listen("tcp", bind)
		if err != nil {
			return err
		}
	}
	// Acquire all listeners before serving, so a partial startup cannot leave
	// an HTTP server running against resources the caller has already closed.
	if socket != "" {
		if err := removeStaleUnixSocket(socket); err != nil {
			if ln != nil {
				_ = ln.Close()
			}
			return err
		}
		unixListener, err := net.Listen("unix", socket)
		if err != nil {
			if ln != nil {
				_ = ln.Close()
			}
			return fmt.Errorf("listen on Unix socket: %w", err)
		}
		if err = os.Chmod(socket, 0o600); err != nil {
			_ = unixListener.Close()
			if ln != nil {
				_ = ln.Close()
			}
			return fmt.Errorf("set Unix socket permissions: %w", err)
		}
		s.unix = &http.Server{Handler: s.middleware(mux, true), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: responseWriteTimeout, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		go s.serve("unix", s.unix, peerListener{Listener: unixListener, uid: uint32(os.Geteuid())})
	}
	if ln != nil {
		go s.serve("tcp", s.tcp, ln)
	}
	return nil
}

func removeStaleUnixSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Unix socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("Unix socket path %s exists and is not a socket", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("Unix socket path %s is already in use", path)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("probe Unix socket path: %w", dialErr)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale Unix socket: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.BeginShutdown()
	var errs []error
	for _, server := range []*http.Server{s.tcp, s.unix} {
		if server == nil {
			continue
		}
		if err := server.Shutdown(ctx); err != nil {
			// Shutdown leaves active connections open when its context expires.
			// Do not keep serving against a database the daemon is about to close.
			errs = append(errs, err, server.Close())
		}
	}
	return errors.Join(errs...)
}
func (s *Server) middleware(next http.Handler, local bool) http.Handler {
	allowIframe := true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MINICRON_ALLOW_IFRAME"))) {
	case "", "0", "false", "f", "no", "n", "off":
		allowIframe = false
	}
	csp := contentSecurityPolicy
	if allowIframe {
		csp = strings.Replace(csp, "frame-ancestors 'none'; ", "", 1)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := &trackedResponse{ResponseWriter: w}
		if flusher, ok := w.(http.Flusher); ok {
			w = &trackedFlushResponse{trackedResponse: response, flusher: flusher}
		} else {
			w = response
		}
		defer func() {
			if value := recover(); value != nil {
				if err, ok := value.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(value)
				}
				err := &fault.PanicError{Value: value, Stack: debug.Stack()}
				slog.Error("HTTP handler panicked", "method", r.Method, "path", r.URL.Path, "error", err, "stack", string(err.Stack))
				if response.started {
					panic(http.ErrAbortHandler)
				}
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !allowIframe {
			w.Header().Set("X-Frame-Options", "DENY")
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", csp)
		if len(r.URL.RequestURI()) > 2048 {
			writeError(w, 414, "request_too_large", "URL exceeds 2 KiB")
			return
		}
		if s.basePath != "" {
			if r.URL.Path == s.basePath {
				target := s.basePath + "/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusMovedPermanently)
				return
			}
			if !strings.HasPrefix(r.URL.Path, s.basePath+"/") {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = strings.TrimPrefix(r.URL.Path, s.basePath)
			r.URL.RawPath = ""
		}
		if local {
			r = r.WithContext(context.WithValue(r.Context(), localKey{}, true))
		}
		// Only API data endpoints require the bearer token. The SPA shell
		// (any UI path), bundled assets, and health probes are public: they
		// contain no secrets, and the SPA authenticates its own API calls.
		isAPI := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/openapi.json"
		if isAPI && s.closing.Load() {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "daemon is shutting down")
			return
		}
		if isAPI && !local {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			sum := sha256.Sum256([]byte(provided))
			expected, _ := hex.DecodeString(s.currentTokenHash())
			if len(expected) != len(sum) || subtle.ConstantTimeCompare(sum[:], expected) != 1 {
				writeError(w, 401, "unauthorized", "valid bearer token required")
				return
			}
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// Unwrap preserves http.ResponseController support through response tracking.
type trackedResponse struct {
	http.ResponseWriter
	started bool
}

func (w *trackedResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *trackedResponse) WriteHeader(status int) {
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.started = true
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *trackedResponse) Write(body []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(body)
}

type trackedFlushResponse struct {
	*trackedResponse
	flusher http.Flusher
}

func (w *trackedFlushResponse) Flush() {
	w.started = true
	w.flusher.Flush()
}
func (s *Server) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok\n")) })
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			writeError(w, 503, "not_ready", "daemon is not ready")
			return
		}
		w.Write([]byte("ready\n"))
	})
	openAPI := sync.OnceValue(func() []byte { return OpenAPIContract(s.version) })
	m.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(openAPI())
	})
	m.HandleFunc("GET /api/v1/daemon", s.daemon)
	m.HandleFunc("POST /api/v1/daemon/reload", s.reloadHandler)
	m.HandleFunc("GET /api/v1/jobs", s.jobs)
	m.HandleFunc("POST /api/v1/jobs", s.putJob)
	m.HandleFunc("GET /api/v1/jobs/{name}", s.job)
	m.HandleFunc("PUT /api/v1/jobs/{name}", s.putJob)
	m.HandleFunc("DELETE /api/v1/jobs/{name}", s.deleteJob)
	m.HandleFunc("POST /api/v1/jobs/{name}/trigger", s.trigger)
	m.HandleFunc("POST /api/v1/jobs/{name}/enable", s.enable)
	m.HandleFunc("POST /api/v1/jobs/{name}/disable", s.enable)
	m.HandleFunc("GET /api/v1/workers/states", s.workerStates)
	m.HandleFunc("POST /api/v1/workers/{name}/start", s.workerStart)
	m.HandleFunc("POST /api/v1/workers/{name}/stop", s.workerStop)
	m.HandleFunc("POST /api/v1/workers/{name}/restart", s.workerRestart)
	m.HandleFunc("GET /api/v1/metrics/runs", s.runMetrics)
	m.HandleFunc("GET /api/v1/metrics/alerts", s.alertMetrics)
	m.HandleFunc("GET /api/v1/alert-channels", s.listAlertChannels)
	m.HandleFunc("POST /api/v1/alert-channels/{name}/test", s.testAlertChannel)
	m.HandleFunc("GET /api/v1/runs/{id}/alerts", s.runAlerts)
	m.HandleFunc("GET /api/v1/runs", s.runs)
	m.HandleFunc("GET /api/v1/runs/{id}", s.run)
	m.HandleFunc("POST /api/v1/runs/{id}/stop", s.stop)
	m.HandleFunc("GET /api/v1/runs/{id}/log", s.log)
	m.HandleFunc("GET /api/v1/runs/{id}/log/raw", s.raw)
	m.HandleFunc("GET /api/v1/runs/{id}/log/stream", s.stream)
	m.HandleFunc("POST /api/v1/token/rotate", s.rotate)
	m.HandleFunc("GET /api/v1/export", s.export)
	m.HandleFunc("POST /api/v1/import/preview", s.importPreview)
	m.HandleFunc("POST /api/v1/import/apply", s.importApply)
	m.HandleFunc("GET /assets/{name}", s.asset)
	// SPA shell for the root and every client-side route (path-based routing:
	// /jobs, /runs/{id}, /metrics, ... all serve index.html and let the
	// router take over). Unknown /api paths stay JSON 404s.
	m.HandleFunc("GET /{$}", s.ui)
	m.HandleFunc("GET /{path...}", s.ui)
	return m
}
func (s *Server) daemon(w http.ResponseWriter, r *http.Request) {
	capabilities := []string{"local", "file-logs", "sqlite-log-archive"}
	if os.Geteuid() == 0 {
		capabilities = append(capabilities, "run-as")
	}
	info := map[string]any{"version": s.version, "schema_version": store.SchemaVersion, "uptime_s": int64(time.Since(s.started).Seconds()), "capabilities": capabilities, "tcp_enabled": s.tcpEnabled}
	if s.tcpEnabled {
		info["token_fingerprint"] = fingerprint(s.currentTokenHash())
	}
	writeJSON(w, 200, info)
}
func (s *Server) reloadHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.reload(r.Context()); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"reloaded": true})
}
func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	defs, err := s.store.Definitions(r.Context())
	if err != nil {
		internal(w, r, err)
		return
	}
	nextByDefinition, err := s.store.ScheduleNextBatch(r.Context())
	if err != nil {
		internal(w, r, err)
		return
	}
	items := make([]jobListItem, 0, len(defs))
	for _, d := range defs {
		item := jobListItem{Definition: d}
		if next, ok := nextByDefinition[d.ID]; ok && d.Kind == model.KindJob && d.IsEnabled() && d.Schedule != "" {
			item.NextFireAt = &next
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (s *Server) workerStates(w http.ResponseWriter, r *http.Request) {
	defs, err := s.store.Definitions(r.Context())
	if err != nil {
		internal(w, r, err)
		return
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		if d.Kind == model.KindWorker {
			names = append(names, d.Name)
		}
	}
	writeJSON(w, 200, map[string]any{"items": s.super.States(names)})
}
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	d, hash, err := s.store.Definition(r.Context(), r.PathValue("name"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "not_found", "definition not found")
		return
	}
	if err != nil {
		internal(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.FormatInt(d.Revision, 10))
	response := map[string]any{"definition": d, "hash": hash, "active_runs": s.exec.Active(d.Name)}
	if d.Kind == model.KindJob && d.IsEnabled() && d.Schedule != "" {
		next, err := s.store.ScheduleNext(r.Context(), d.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			internal(w, r, fmt.Errorf("read definition %s next fire: %w", d.Name, err))
			return
		}
		if err == nil && !next.IsZero() {
			response["next_fire_at"] = next
		}
	}
	if d.Kind == model.KindWorker {
		response["worker_state"] = s.super.State(d.Name)
	}
	writeJSON(w, 200, response)
}
func (s *Server) putJob(w http.ResponseWriter, r *http.Request) {
	var d model.Definition
	if err := decodeJSON(r.Body, &d); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	if name := r.PathValue("name"); name != "" {
		d.Name = name
	}
	if d.Kind == "" {
		d.Kind = model.KindJob
	}
	d.Source = ""
	var defaults model.Definition
	if s.jobDefaults != nil {
		defaults = s.jobDefaults()
	}
	if err := config.ValidateDefinition(&d, defaults); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	if err := s.validateAlerts([]model.Definition{d}); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	var expected int64
	createOnly := r.Header.Get("If-None-Match") != ""
	if createOnly && r.Header.Get("If-None-Match") != "*" {
		writeError(w, 400, "invalid_precondition", "If-None-Match must be *")
		return
	}
	if createOnly && r.Header.Get("If-Match") != "" {
		writeError(w, 400, "invalid_precondition", "If-Match and If-None-Match cannot be combined")
		return
	}
	if raw := r.Header.Get("If-Match"); raw != "" {
		var err error
		expected, err = strconv.ParseInt(strings.Trim(raw, `"`), 10, 64)
		if err != nil || expected <= 0 {
			writeError(w, 400, "invalid_precondition", "If-Match must be a positive revision")
			return
		}
	}
	var saved model.Definition
	var err error
	if createOnly {
		saved, err = s.store.CreateDefinition(r.Context(), d, "api")
	} else {
		saved, err = s.store.PutDefinition(r.Context(), d, expected, "api")
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrReadOnly):
			writeError(w, 403, "read_only", err.Error())
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, 412, "revision_conflict", err.Error())
		default:
			internal(w, r, fmt.Errorf("save definition %s: %w", d.Name, err))
		}
		return
	}
	if err := s.reconcile(context.WithoutCancel(r.Context())); err != nil {
		internal(w, r, fmt.Errorf("reconcile saved definition %s: %w", d.Name, err))
		return
	}
	writeJSON(w, 200, saved)
}
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteDefinition(r.Context(), r.PathValue("name"), "api"); err != nil {
		if errors.Is(err, store.ErrReadOnly) {
			writeError(w, 403, "read_only", err.Error())
			return
		}
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
			writeError(w, 404, "not_found", "definition not found")
		} else {
			internal(w, r, fmt.Errorf("delete definition %s: %w", r.PathValue("name"), err))
		}
		return
	}
	if err := s.reconcile(context.WithoutCancel(r.Context())); err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

func (s *Server) enable(w http.ResponseWriter, r *http.Request) {
	enabled := strings.HasSuffix(r.URL.Path, "/enable")
	if err := s.store.SetEnabled(r.Context(), r.PathValue("name"), enabled); err != nil {
		if errors.Is(err, store.ErrReadOnly) {
			writeError(w, 403, "read_only", err.Error())
			return
		}
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
			writeError(w, 404, "not_found", "definition not found")
		} else {
			internal(w, r, fmt.Errorf("set definition %s enabled=%t: %w", r.PathValue("name"), enabled, err))
		}
		return
	}
	if err := s.reconcile(context.WithoutCancel(r.Context())); err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"enabled": enabled})
}

// httpError pairs the stable error envelope with its HTTP status so
// helpers can return typed failures.
type httpError struct {
	status int
	code   string
	msg    string
	cause  error
}

func (e *httpError) Error() string { return e.msg }

// beginTrigger runs the idempotency check, the trigger itself, and the key
// save while holding idemMu, so concurrent requests sharing a key cannot
// double-fire. It never waits for the run: the mutex is released long before
// any wait=true polling starts.
func (s *Server) beginTrigger(ctx context.Context, name, key, requestHash string) (run model.Run, replayed bool, herr *httpError) {
	s.idemMu.Lock()
	defer s.idemMu.Unlock()
	if key != "" {
		if id, err := s.store.IdempotentRun(ctx, "admin", "trigger", key, requestHash); err == nil {
			existing, getErr := s.store.Run(ctx, id)
			if getErr != nil {
				return model.Run{}, false, triggerInternal(fmt.Errorf("read replayed run %s: %w", id, getErr))
			}
			return existing, true, nil
		} else if errors.Is(err, store.ErrIdempotencyConflict) || errors.Is(err, store.ErrIdempotencyKeyExists) {
			return model.Run{}, false, &httpError{status: 409, code: "idempotency_conflict", msg: err.Error()}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return model.Run{}, false, triggerInternal(fmt.Errorf("read trigger idempotency key: %w", err))
		}
	}
	d, hash, err := s.store.Definition(ctx, name)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
		return model.Run{}, false, &httpError{status: 404, code: "not_found", msg: "definition not found"}
	}
	if err != nil {
		return model.Run{}, false, triggerInternal(fmt.Errorf("read triggered definition %s: %w", name, err))
	}
	if d.Source == "config" {
		return model.Run{}, false, &httpError{status: 403, code: "read_only", msg: "config-owned definition is view only"}
	}
	if d.Kind == model.KindWorker {
		return model.Run{}, false, &httpError{status: 409, code: "trigger_rejected", msg: "workers must be controlled through worker lifecycle endpoints"}
	}
	if key != "" {
		run, replayed, err = s.exec.TriggerIdempotent(ctx, d, hash, "manual", nil, executor.IdempotencyRequest{Principal: "admin", Operation: "trigger", Key: key, RequestHash: requestHash})
	} else {
		run, err = s.exec.Trigger(ctx, d, hash, "manual", nil)
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrIdempotencyConflict), errors.Is(err, store.ErrIdempotencyKeyExists):
			return model.Run{}, false, &httpError{status: 409, code: "idempotency_conflict", msg: err.Error()}
		case errors.Is(err, executor.ErrDisabled):
			return model.Run{}, false, &httpError{status: 409, code: "trigger_rejected", msg: err.Error()}
		case errors.Is(err, executor.ErrShutdown):
			return model.Run{}, false, &httpError{status: 503, code: "not_ready", msg: "daemon is shutting down"}
		default:
			return model.Run{}, false, triggerInternal(fmt.Errorf("trigger definition %s: %w", name, err))
		}
	}
	return run, replayed, nil
}

func triggerInternal(err error) *httpError {
	return &httpError{status: 500, code: "internal_error", msg: "internal server error", cause: err}
}

func (s *Server) trigger(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	requestHash := fmt.Sprintf("%x", sha256.Sum256([]byte(r.PathValue("name"))))
	run, replayed, herr := s.beginTrigger(r.Context(), r.PathValue("name"), key, requestHash)
	if herr != nil {
		if herr.cause != nil {
			internal(w, r, herr.cause)
		} else {
			writeError(w, herr.status, herr.code, herr.msg)
		}
		return
	}
	if replayed {
		writeJSON(w, 200, run)
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		timeout := time.Duration(timeoutSeconds(r.URL.Query().Get("timeout"))) * time.Second
		// The server write timeout is armed when the request is read; a wait
		// longer than it would otherwise lose the response after the run ends.
		extendWriteDeadline(w, timeout+responseWriteTimeout)
		run = s.waitForRun(r.Context(), run, timeout)
		if model.Terminal(run.Status) {
			writeJSON(w, 200, run)
			return
		}
	}
	writeJSON(w, 202, run)
}

// waitForRun waits for an active run to finalize its logs and terminal state.
// A completed run is no longer active, so read the persisted state directly.
func (s *Server) waitForRun(ctx context.Context, run model.Run, timeout time.Duration) model.Run {
	if done := s.exec.Wait(run.ID); done != nil {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-ctx.Done():
			return run
		case <-timer.C:
			return run
		}
	}
	current, err := s.store.Run(ctx, run.ID)
	if err == nil && model.Terminal(current.Status) {
		return current
	}
	if err != nil && ctx.Err() == nil {
		slog.Error("read completed trigger run failed", "run", run.ID, "error", err)
	}
	return run
}
func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	d, _, err := s.store.Definition(r.Context(), r.PathValue("name"))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) || (err == nil && d.Kind != model.KindWorker) {
		writeError(w, 404, "not_found", "worker not found")
		return
	}
	if err != nil {
		internal(w, r, fmt.Errorf("read worker definition %s: %w", r.PathValue("name"), err))
		return
	}
	if d.Source == "config" {
		writeError(w, 403, "read_only", "config-owned definition is view only")
		return
	}
	s.super.StartDefinition(d)
	writeJSON(w, 202, map[string]bool{"starting": true})
}
func (s *Server) workerStop(w http.ResponseWriter, r *http.Request) {
	d, _, err := s.store.Definition(r.Context(), r.PathValue("name"))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) || (err == nil && d.Kind != model.KindWorker) {
		writeError(w, 404, "not_found", "worker not found")
		return
	}
	if err != nil {
		internal(w, r, fmt.Errorf("read worker definition %s: %w", r.PathValue("name"), err))
		return
	}
	if d.Source == "config" {
		writeError(w, 403, "read_only", "config-owned definition is view only")
		return
	}
	if err := s.super.Stop(r.PathValue("name")); err != nil && !errors.Is(err, os.ErrNotExist) {
		internal(w, r, err)
		return
	}
	writeJSON(w, 202, map[string]bool{"held": true})
}
func (s *Server) workerRestart(w http.ResponseWriter, r *http.Request) {
	d, _, err := s.store.Definition(r.Context(), r.PathValue("name"))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) || (err == nil && d.Kind != model.KindWorker) {
		writeError(w, 404, "not_found", "worker not found")
		return
	}
	if err != nil {
		internal(w, r, fmt.Errorf("read worker definition %s: %w", r.PathValue("name"), err))
		return
	}
	if d.Source == "config" {
		writeError(w, 403, "read_only", "config-owned definition is view only")
		return
	}
	s.super.Restart(d)
	writeJSON(w, 202, map[string]bool{"restarting": true})
}

func (s *Server) listAlertChannels(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0)
	if s.alertChannels != nil {
		for _, ch := range s.alertChannels() {
			items = append(items, map[string]any{"name": ch.Name, "type": ch.Type, "batch_window": ch.BatchWindow})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) testAlertChannel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	found := false
	if s.alertChannels != nil {
		for _, channel := range s.alertChannels() {
			found = found || channel.Name == name
		}
	}
	if !found || s.testAlert == nil {
		writeError(w, 404, "not_found", "alert channel not found")
		return
	}
	if err := s.testAlert(r.Context(), name); err != nil {
		logHTTPFailure(r, fmt.Errorf("send test alert to channel %s: %w", name, err))
		writeError(w, 502, "delivery_failed", "alert delivery failed")
		return
	}
	writeJSON(w, 200, map[string]bool{"sent": true})
}

func (s *Server) runAlerts(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.Run(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "not_found", "run not found")
		} else {
			internal(w, r, err)
		}
		return
	}
	items, err := s.store.RunAlerts(r.Context(), r.PathValue("id"))
	if err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) alertMetrics(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.AlertMetrics(r.Context())
	if err != nil {
		internal(w, r, err)
		return
	}
	depth := 0
	if s.alertQueueDepth != nil {
		depth = s.alertQueueDepth()
	}
	writeJSON(w, 200, map[string]any{"counts": counts, "queue_depth": depth})
}

func (s *Server) runMetrics(w http.ResponseWriter, r *http.Request) {
	rangeKey := r.URL.Query().Get("range")
	window := map[string]time.Duration{
		"15m": 15 * time.Minute,
		"1h":  time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}[rangeKey]
	if window == 0 {
		window = time.Hour
	}
	buckets, _ := strconv.Atoi(r.URL.Query().Get("buckets"))
	if buckets == 0 {
		buckets = 48
	}
	now := time.Now()
	stats, err := s.store.RunMetrics(r.Context(), now.Add(-window), now, buckets)
	if err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, 200, stats)
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}
	limit = min(max(limit, 1), 500)
	filter := r.URL.Query().Get("filter")
	switch filter {
	case "", "failed", "scheduled", "manual", "active":
	default:
		writeError(w, 400, "invalid_filter", "unknown run filter")
		return
	}
	runs, err := s.store.RunsPage(r.Context(), r.URL.Query().Get("job"), limit+1, r.URL.Query().Get("before"), filter)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "not_found", "run cursor not found")
		return
	}
	if err != nil {
		internal(w, r, err)
		return
	}
	nextBefore := ""
	if len(runs) > limit {
		runs = runs[:limit]
		nextBefore = runs[len(runs)-1].ID
	}
	writeJSON(w, 200, map[string]any{"items": runs, "next_before": nextBefore})
}
func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.Run(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "not_found", "run not found")
		return
	}
	if err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, 200, run)
}
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := s.exec.Stop(r.PathValue("id")); err != nil {
		writeError(w, 404, "not_found", "active run not found")
		return
	}
	writeJSON(w, 202, map[string]bool{"stopping": true})
}
func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	if !s.requireRun(w, r, r.PathValue("id")) {
		return
	}
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 1000
	}
	frames, err := s.logs.ReadContext(r.Context(), r.PathValue("id"), after, limit)
	if err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": frames})
}
func (s *Server) raw(w http.ResponseWriter, r *http.Request) {
	if !s.requireRun(w, r, r.PathValue("id")) {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("id")+`.log"`)
	// Downloads of any size may take longer than the server write timeout as
	// long as the client keeps reading.
	if err := s.logs.RawContext(r.Context(), r.PathValue("id"), &progressWriter{w: w}); err != nil && r.Context().Err() == nil {
		slog.Error("raw log response failed", "run", r.PathValue("id"), "error", err)
	}
}
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if !s.requireRun(w, r, r.PathValue("id")) {
		return
	}
	select {
	case s.streamSlots <- struct{}{}:
		defer func() { <-s.streamSlots }()
	default:
		writeError(w, http.StatusServiceUnavailable, "stream_capacity", "too many active log streams")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "stream_unavailable", "streaming unsupported")
		return
	}
	after, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if q, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64); q > after {
		after = q
	}
	var live <-chan logstore.Frame
	var dropped <-chan struct{}
	var unsubscribe func()
	if active := s.logs.Active(r.PathValue("id")); active != nil {
		live, dropped, unsubscribe = active.Subscribe(after)
		defer unsubscribe()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	reader := s.logs.NewStreamReader(r.PathValue("id"))
	defer reader.Close()
	controller := http.NewResponseController(w)
	for {
		backlog, err := reader.ReadContext(r.Context(), after, 5000)
		if err != nil {
			internal(w, r, err)
			return
		}
		if len(backlog) == 0 {
			break
		}
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		for _, f := range backlog {
			if err := writeSSE(w, "line", f.Sequence, f); err != nil {
				return
			}
			after = f.Sequence
		}
		flusher.Flush()
	}
	reader.Close()
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, "event: backlog_done\ndata: {}\n\n"); err != nil {
		return
	}
	flusher.Flush()
	if live == nil {
		_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
		flusher.Flush()
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-live:
			if !ok {
				_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
				caseDropped := false
				select {
				case <-dropped:
					caseDropped = true
				default:
				}
				if caseDropped {
					_, _ = fmt.Fprintf(w, "event: dropped\ndata: {\"after\":%d}\n\n", after)
				} else {
					_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
				}
				flusher.Flush()
				return
			}
			if f.Sequence > after {
				_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if err := writeSSE(w, "line", f.Sequence, f); err != nil {
					return
				}
				after = f.Sequence
				flusher.Flush()
			}
		case <-dropped:
			_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprintf(w, "event: dropped\ndata: {\"after\":%d}\n\n", after); err != nil {
				return
			}
			flusher.Flush()
			return
		case <-heartbeat.C:
			_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
func (s *Server) rotate(w http.ResponseWriter, r *http.Request) {
	if !s.tcpEnabled {
		writeError(w, 404, "not_available", "bearer tokens are disabled while TCP is disabled")
		return
	}
	if local, _ := r.Context().Value(localKey{}).(bool); !local {
		writeError(w, 403, "local_only", "token rotation requires the Unix socket")
		return
	}
	token, err := s.RotateToken(r.Context())
	if err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]string{"token": token, "fingerprint": fingerprint(s.currentTokenHash())})
}
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	defs, err := s.store.Definitions(r.Context())
	if err != nil {
		internal(w, r, err)
		return
	}
	editable := defs[:0]
	for _, d := range defs {
		if d.Source != "config" {
			editable = append(editable, d)
		}
	}
	defs = editable
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, 200, map[string]any{"definitions": defs})
		return
	}
	bundle := struct {
		Jobs    []model.Definition `toml:"job"`
		Workers []model.Definition `toml:"worker"`
	}{}
	for _, d := range defs {
		d.ID = 0
		d.Revision = 0
		if d.Kind == model.KindWorker {
			bundle.Workers = append(bundle.Workers, d)
		} else {
			bundle.Jobs = append(bundle.Jobs, d)
		}
	}
	body, err := toml.Marshal(bundle)
	if err != nil {
		internal(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/toml")
	w.Header().Set("Content-Disposition", `attachment; filename="minicron-export.toml"`)
	w.Write(body)
}

type importRequest struct {
	Content string `json:"content"`
	Hash    string `json:"hash,omitempty"`
}

func (s *Server) importPreview(w http.ResponseWriter, r *http.Request) {
	var request importRequest
	if err := decodeJSON(r.Body, &request); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	var defaults model.Definition
	if s.jobDefaults != nil {
		defaults = s.jobDefaults()
	}
	defs, err := config.ParseImport([]byte(request.Content), defaults)
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	if err := s.validateAlerts(defs); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	sum := sha256.Sum256([]byte(request.Content))
	writeJSON(w, 200, map[string]any{"content_hash": hex.EncodeToString(sum[:]), "definitions": defs})
}
func (s *Server) importApply(w http.ResponseWriter, r *http.Request) {
	var request importRequest
	if err := decodeJSON(r.Body, &request); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	sum := sha256.Sum256([]byte(request.Content))
	actual := hex.EncodeToString(sum[:])
	if request.Hash == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(request.Hash)) != 1 {
		writeError(w, 409, "content_hash_mismatch", "apply content does not match preview")
		return
	}
	var defaults model.Definition
	if s.jobDefaults != nil {
		defaults = s.jobDefaults()
	}
	defs, err := config.ParseImport([]byte(request.Content), defaults)
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	if err := s.validateAlerts(defs); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	if err = s.store.ImportDefinitions(r.Context(), defs, "api:import"); err != nil {
		if errors.Is(err, store.ErrReadOnly) {
			writeError(w, 403, "read_only", err.Error())
			return
		}
		internal(w, r, err)
		return
	}
	if err = s.reconcile(r.Context()); err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"content_hash": actual, "applied": len(defs)})
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".css") {
		http.NotFound(w, r)
		return
	}
	asset, ok := cachedAssets()[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	// Vite emits content-hashed filenames; once fetched they remain valid.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Vary", "Accept-Encoding")
	body := asset.body
	etag := asset.etag
	if len(asset.gzip) > 0 && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body = asset.gzip
		etag = strings.TrimSuffix(etag, `"`) + `-gzip"`
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

func acceptsGzip(header string) bool {
	for _, encoding := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(encoding), ";")
		if strings.TrimSpace(parts[0]) != "gzip" {
			continue
		}
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && key == "q" {
				quality, err := strconv.ParseFloat(value, 64)
				return err == nil && quality > 0
			}
		}
		return true
	}
	return false
}

func sha256Sum(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, 404, "not_found", "unknown API endpoint")
		return
	}
	asset, ok := cachedAssets()["index.html"]
	if !ok {
		writeError(w, 500, "internal_error", "embedded UI is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Always revalidate the SPA shell so it picks up new asset bundles.
	w.Header().Set("Cache-Control", "no-cache")
	base := s.basePath + "/"
	page := strings.Replace(string(asset.body), `<base href="/" />`, `<base href="`+base+`" />`, 1)
	// Vite emits relative links for the entry script and stylesheet.
	// Put those HTML links under the asset route; the <base> tag
	// resolves them under BASE_PATH on every client-side route.
	page = strings.ReplaceAll(page, `src="./`, `src="assets/`)
	page = strings.ReplaceAll(page, `href="./`, `href="assets/`)
	_, _ = io.WriteString(w, page)
}
func (s *Server) requireRun(w http.ResponseWriter, r *http.Request, id string) bool {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != strings.ToLower(id) {
		writeError(w, 400, "invalid_run_id", "run ID must be a canonical UUID")
		return false
	}
	if _, err := s.store.Run(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "not_found", "run not found")
		} else {
			internal(w, r, err)
		}
		return false
	}
	return true
}

// responseWriteTimeout bounds each response write. Long-lived responses
// extend it as they make progress instead of disabling it.
var responseWriteTimeout = 30 * time.Second

func extendWriteDeadline(w http.ResponseWriter, d time.Duration) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("extending response write deadline failed", "error", err)
	}
}

// progressWriter renews the write deadline while a streamed body is being
// written, at most a few times per timeout period, so only a stalled client
// times out.
type progressWriter struct {
	w        http.ResponseWriter
	extended time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	if now := time.Now(); now.Sub(p.extended) >= min(time.Second, responseWriteTimeout/4) {
		extendWriteDeadline(p.w, responseWriteTimeout)
		p.extended = now
	}
	return p.w.Write(b)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write JSON response failed", "error", err)
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{apiError{code, message, nil}})
}
func internal(w http.ResponseWriter, r *http.Request, err error) {
	logHTTPFailure(r, err)
	writeError(w, 500, "internal_error", "internal server error")
}

func logHTTPFailure(r *http.Request, err error) {
	attrs := []any{"method", r.Method, "path", r.URL.Path, "error", err}
	if panicErr, ok := errors.AsType[*fault.PanicError](err); ok {
		attrs = append(attrs, "stack", string(panicErr.Stack))
	}
	slog.Error("HTTP request failed", attrs...)
}
func writeSSE(w io.Writer, event string, id uint64, value any) error {
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: ", id, event); err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func decodeJSON(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON documents are not allowed")
		}
		return err
	}
	return nil
}

// timeoutSeconds parses the ?timeout= query parameter of a waited trigger,
// bounded to 240s and defaulting to 120s.
func timeoutSeconds(v string) int {
	n, _ := strconv.Atoi(v)
	if n <= 0 {
		n = 120
	}
	return min(n, 240)
}
func fingerprint(hash string) string {
	if len(hash) < 12 {
		return hash
	}
	return hash[:12]
}
