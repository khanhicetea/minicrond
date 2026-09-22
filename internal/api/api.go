package api

import (
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
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
	// tokenHash holds the hex SHA-256 of the active bearer token; the raw
	// token exists only at rotation time.
	tokenHash   atomic.Pointer[string]
	tcp         *http.Server
	unix        *http.Server
	idemMu      sync.Mutex
	tokenMu     sync.Mutex
	streamSlots chan struct{}
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
	etag string
}

var cachedAssets = sync.OnceValue(func() map[string]cachedAsset {
	out := make(map[string]cachedAsset)
	for _, name := range []string{"app.js", "style.css", "index.html"} {
		body, err := webAssets.ReadFile("assets/" + name)
		if err != nil {
			continue
		}
		out[name] = cachedAsset{body: body, etag: `"` + hex.EncodeToString(sha256Sum(body))[:16] + `"`}
	}
	return out
})

type localKey struct{}

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
	return &Server{store: st, logs: logs, exec: ex, super: sup, reload: reload, reconcile: reconcile, started: time.Now(), version: version, streamSlots: make(chan struct{}, 64)}
}
func (s *Server) InitializeToken(ctx context.Context) (string, error) {
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
func (s *Server) Start(bind, socket string) error {
	mux := s.routes()
	s.tcp = &http.Server{Addr: bind, Handler: s.middleware(mux, false), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return err
	}
	// Acquire all listeners before serving, so a partial startup cannot leave
	// an HTTP server running against resources the caller has already closed.
	if socket != "" {
		if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = ln.Close()
			return fmt.Errorf("remove stale Unix socket: %w", err)
		}
		unixListener, err := net.Listen("unix", socket)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("listen on Unix socket: %w", err)
		}
		if err = os.Chmod(socket, 0o600); err != nil {
			_ = unixListener.Close()
			_ = ln.Close()
			return fmt.Errorf("set Unix socket permissions: %w", err)
		}
		s.unix = &http.Server{Handler: s.middleware(mux, true), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		go func() {
			if err := s.unix.Serve(peerListener{Listener: unixListener, uid: uint32(os.Geteuid())}); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("Unix HTTP server stopped", "error", err)
				s.ready.Store(false)
			}
		}()
	}
	go func() {
		if err := s.tcp.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("TCP HTTP server stopped", "error", err)
			s.ready.Store(false)
		}
	}()
	return nil
}
func (s *Server) Shutdown(ctx context.Context) error {
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		if len(r.URL.RequestURI()) > 2048 {
			writeError(w, 414, "request_too_large", "URL exceeds 2 KiB")
			return
		}
		if local {
			r = r.WithContext(context.WithValue(r.Context(), localKey{}, true))
		}
		// Only API data endpoints require the bearer token. The SPA shell
		// (any UI path), bundled assets, and health probes are public: they
		// contain no secrets, and the SPA authenticates its own API calls.
		isAPI := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/openapi.json"
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
	m.HandleFunc("POST /api/v1/workers/{name}/start", s.workerStart)
	m.HandleFunc("POST /api/v1/workers/{name}/stop", s.workerStop)
	m.HandleFunc("POST /api/v1/workers/{name}/restart", s.workerRestart)
	m.HandleFunc("GET /api/v1/metrics/runs", s.runMetrics)
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
	writeJSON(w, 200, map[string]any{"version": s.version, "schema_version": store.SchemaVersion, "uptime_s": int64(time.Since(s.started).Seconds()), "capabilities": capabilities, "token_fingerprint": fingerprint(s.currentTokenHash())})
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
		internal(w, err)
		return
	}
	nextByDefinition, err := s.store.ScheduleNextBatch(r.Context())
	if err != nil {
		internal(w, err)
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
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	d, hash, err := s.store.Definition(r.Context(), r.PathValue("name"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "not_found", "definition not found")
		return
	}
	if err != nil {
		internal(w, err)
		return
	}
	w.Header().Set("ETag", strconv.FormatInt(d.Revision, 10))
	response := map[string]any{"definition": d, "hash": hash, "active_runs": s.exec.Active(d.Name)}
	if d.Kind == model.KindJob && d.IsEnabled() && d.Schedule != "" {
		if next, err := s.store.ScheduleNext(r.Context(), d.ID); err == nil && !next.IsZero() {
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
	if err := config.ValidateDefinition(&d); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	var expected int64
	if raw := r.Header.Get("If-Match"); raw != "" {
		var err error
		expected, err = strconv.ParseInt(strings.Trim(raw, `"`), 10, 64)
		if err != nil || expected <= 0 {
			writeError(w, 400, "invalid_precondition", "If-Match must be a positive revision")
			return
		}
	}
	saved, err := s.store.PutDefinition(r.Context(), d, expected, "api")
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, 412, "revision_conflict", err.Error())
		default:
			writeError(w, 422, "validation_failed", err.Error())
		}
		return
	}
	if err := s.reconcile(context.WithoutCancel(r.Context())); err != nil {
		writeError(w, 500, "reconcile_failed", err.Error())
		return
	}
	writeJSON(w, 200, saved)
}
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteDefinition(r.Context(), r.PathValue("name"), "api"); err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if err := s.reconcile(context.WithoutCancel(r.Context())); err != nil {
		internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

func (s *Server) enable(w http.ResponseWriter, r *http.Request) {
	enabled := strings.HasSuffix(r.URL.Path, "/enable")
	if err := s.store.SetEnabled(r.Context(), r.PathValue("name"), enabled); err != nil {
		writeError(w, 404, "not_found", err.Error())
		return
	}
	if err := s.reconcile(context.WithoutCancel(r.Context())); err != nil {
		internal(w, err)
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
				return model.Run{}, false, &httpError{500, "internal_error", getErr.Error()}
			}
			return existing, true, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return model.Run{}, false, &httpError{409, "idempotency_conflict", err.Error()}
		}
	}
	d, hash, err := s.store.Definition(ctx, name)
	if err != nil {
		return model.Run{}, false, &httpError{404, "not_found", "definition not found"}
	}
	if d.Kind == model.KindWorker {
		return model.Run{}, false, &httpError{409, "trigger_rejected", "workers must be controlled through worker lifecycle endpoints"}
	}
	if key != "" {
		run, replayed, err = s.exec.TriggerIdempotent(ctx, d, hash, "manual", nil, executor.IdempotencyRequest{Principal: "admin", Operation: "trigger", Key: key, RequestHash: requestHash})
	} else {
		run, err = s.exec.Trigger(ctx, d, hash, "manual", nil)
	}
	if err != nil {
		code := "trigger_rejected"
		if strings.Contains(err.Error(), "idempotency") {
			code = "idempotency_conflict"
		}
		return model.Run{}, false, &httpError{409, code, err.Error()}
	}
	return run, replayed, nil
}

func (s *Server) trigger(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	requestHash := fmt.Sprintf("%x", sha256.Sum256([]byte(r.PathValue("name"))))
	run, replayed, herr := s.beginTrigger(r.Context(), r.PathValue("name"), key, requestHash)
	if herr != nil {
		writeError(w, herr.status, herr.code, herr.msg)
		return
	}
	if replayed {
		writeJSON(w, 200, run)
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		run = s.waitForRun(r.Context(), run, time.Duration(timeoutSeconds(r.URL.Query().Get("timeout")))*time.Second)
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
	return run
}
func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	d, _, err := s.store.Definition(r.Context(), r.PathValue("name"))
	if err != nil || d.Kind != model.KindWorker {
		writeError(w, 404, "not_found", "worker not found")
		return
	}
	s.super.StartDefinition(d)
	writeJSON(w, 202, map[string]bool{"starting": true})
}
func (s *Server) workerStop(w http.ResponseWriter, r *http.Request) {
	if err := s.super.Stop(r.PathValue("name")); err != nil && !errors.Is(err, os.ErrNotExist) {
		internal(w, err)
		return
	}
	writeJSON(w, 202, map[string]bool{"held": true})
}
func (s *Server) workerRestart(w http.ResponseWriter, r *http.Request) {
	d, _, err := s.store.Definition(r.Context(), r.PathValue("name"))
	if err != nil || d.Kind != model.KindWorker {
		writeError(w, 404, "not_found", "worker not found")
		return
	}
	s.super.Restart(d)
	writeJSON(w, 202, map[string]bool{"restarting": true})
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
		internal(w, err)
		return
	}
	writeJSON(w, 200, stats)
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}
	runs, err := s.store.Runs(r.Context(), r.URL.Query().Get("job"), limit)
	if err != nil {
		internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": runs})
}
func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.Run(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "not_found", "run not found")
		return
	}
	if err != nil {
		internal(w, err)
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
		internal(w, err)
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
	if err := s.logs.RawContext(r.Context(), r.PathValue("id"), w); err != nil && r.Context().Err() == nil {
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
	controller := http.NewResponseController(w)
	for {
		backlog, err := s.logs.ReadContext(r.Context(), r.PathValue("id"), after, 5000)
		if err != nil {
			internal(w, err)
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
	if local, _ := r.Context().Value(localKey{}).(bool); !local {
		writeError(w, 403, "local_only", "token rotation requires the Unix socket")
		return
	}
	token, err := s.RotateToken(r.Context())
	if err != nil {
		internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"token": token, "fingerprint": fingerprint(s.currentTokenHash())})
}
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	defs, err := s.store.Definitions(r.Context())
	if err != nil {
		internal(w, err)
		return
	}
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
		internal(w, err)
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
	defs, err := config.ParseImport([]byte(request.Content))
	if err != nil {
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
	defs, err := config.ParseImport([]byte(request.Content))
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	if err = s.store.ImportDefinitions(r.Context(), defs, "api:import"); err != nil {
		internal(w, err)
		return
	}
	if err = s.reconcile(r.Context()); err != nil {
		internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"content_hash": actual, "applied": len(defs)})
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "app.js" && name != "style.css" {
		http.NotFound(w, r)
		return
	}
	asset, ok := cachedAssets()[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body := asset.body
	if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	// Assets ship under fixed filenames, so freshness must be revalidated on
	// every load: max-age would let the browser keep serving a stale bundle
	// for up to an hour after the daemon is rebuilt. no-cache + content-hash
	// ETag gives cheap 304 revalidation and instant pickup of new builds.
	etag := asset.etag
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
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
	_, _ = w.Write(asset.body)
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
			internal(w, err)
		}
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{apiError{code, message, nil}})
}
func internal(w http.ResponseWriter, err error) { writeError(w, 500, "internal_error", err.Error()) }
func writeSSE(w io.Writer, event string, id uint64, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, b)
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
