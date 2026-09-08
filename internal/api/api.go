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
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/minicron/minicron/internal/config"
	"github.com/minicron/minicron/internal/executor"
	"github.com/minicron/minicron/internal/logstore"
	"github.com/minicron/minicron/internal/model"
	"github.com/minicron/minicron/internal/store"
	"github.com/minicron/minicron/internal/supervisor"
)

type Server struct {
	store   *store.Store
	logs    *logstore.Store
	exec    *executor.Service
	super   *supervisor.Supervisor
	reload  func(context.Context) error
	started time.Time
	version string
	ready   atomic.Bool
	// tokenHash holds the hex SHA-256 of the active bearer token; the raw
	// token exists only at rotation time.
	tokenHash atomic.Pointer[string]
	tcp       *http.Server
	unix      *http.Server
	idemMu    sync.Mutex
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

func New(st *store.Store, logs *logstore.Store, ex *executor.Service, sup *supervisor.Supervisor, reload func(context.Context) error, version string) *Server {
	return &Server{store: st, logs: logs, exec: ex, super: sup, reload: reload, started: time.Now(), version: version}
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
	s.tcp = &http.Server{Addr: bind, Handler: s.middleware(mux, false), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 0, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
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
		s.unix = &http.Server{Handler: s.middleware(mux, true), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		go s.unix.Serve(peerListener{Listener: unixListener, uid: uint32(os.Geteuid())})
	}
	go s.tcp.Serve(ln)
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
	items := make([]jobListItem, 0, len(defs))
	for _, d := range defs {
		item := jobListItem{Definition: d}
		if d.Kind == model.KindJob && d.IsEnabled() && d.Schedule != "" {
			if next, err := s.store.ScheduleNext(r.Context(), d.ID); err == nil && !next.IsZero() {
				item.NextFireAt = &next
			}
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
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
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
	expected, _ := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	saved, err := s.store.PutDefinition(r.Context(), d, expected, "api")
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAuthorityConflict):
			writeError(w, 409, "authority_conflict", err.Error())
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, 412, "revision_conflict", err.Error())
		default:
			writeError(w, 422, "validation_failed", err.Error())
		}
		return
	}
	if err := s.reload(r.Context()); err != nil {
		writeError(w, 500, "reconcile_failed", err.Error())
		return
	}
	writeJSON(w, 200, saved)
}
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteDefinition(r.Context(), r.PathValue("name"), "api"); err != nil {
		status, code := 404, "not_found"
		if errors.Is(err, store.ErrAuthorityConflict) {
			status, code = 409, "authority_conflict"
		}
		writeError(w, status, code, err.Error())
		return
	}
	if err := s.reload(r.Context()); err != nil {
		internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

func (s *Server) enable(w http.ResponseWriter, r *http.Request) {
	enabled := strings.HasSuffix(r.URL.Path, "/enable")
	if err := s.store.SetEnabled(r.Context(), r.PathValue("name"), enabled); err != nil {
		writeError(w, 409, "authority_conflict", err.Error())
		return
	}
	if err := s.reload(r.Context()); err != nil {
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
	run, err = s.exec.Trigger(ctx, d, hash, "manual", nil)
	if err != nil {
		return model.Run{}, false, &httpError{409, "trigger_rejected", err.Error()}
	}
	if key != "" {
		if err := s.store.SaveIdempotency(ctx, "admin", "trigger", key, requestHash, run.ID); err != nil {
			return model.Run{}, false, &httpError{500, "internal_error", err.Error()}
		}
	}
	return run, false, nil
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
		seconds := timeoutSeconds(r.URL.Query().Get("timeout"))
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		for time.Now().Before(deadline) {
			current, err := s.store.Run(r.Context(), run.ID)
			if err == nil && model.TerminalStatuses[current.Status] {
				writeJSON(w, 200, current)
				return
			}
			select {
			case <-r.Context().Done(): // client went away; stop burning a slot
				writeJSON(w, 202, run)
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		writeJSON(w, 202, run)
		return
	}
	writeJSON(w, 202, run)
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
	_ = s.super.Stop(d.Name)
	time.AfterFunc(200*time.Millisecond, func() { s.super.StartDefinition(d) })
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
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 1000
	}
	frames, err := s.logs.Read(r.PathValue("id"), after, limit)
	if err != nil {
		internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": frames})
}
func (s *Server) raw(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("id")+`.log"`)
	if err := s.logs.Raw(r.PathValue("id"), w); err != nil {
		internal(w, err)
	}
}
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
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
	backlog, err := s.logs.Read(r.PathValue("id"), after, 5000)
	if err != nil {
		internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for _, f := range backlog {
		writeSSE(w, "line", f.Sequence, f)
		after = f.Sequence
	}
	fmt.Fprint(w, "event: backlog_done\ndata: {}\n\n")
	flusher.Flush()
	if live == nil {
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
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
				select {
				case <-dropped:
					fmt.Fprintf(w, "event: dropped\ndata: {\"after\":%d}\n\n", after)
				default:
					fmt.Fprint(w, "event: done\ndata: {}\n\n")
				}
				flusher.Flush()
				return
			}
			if f.Sequence > after {
				writeSSE(w, "line", f.Sequence, f)
				after = f.Sequence
				flusher.Flush()
			}
		case <-dropped:
			fmt.Fprintf(w, "event: dropped\ndata: {\"after\":%d}\n\n", after)
			flusher.Flush()
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
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
		d.Authority = ""
		d.SourceFile = ""
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
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
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
	for _, d := range defs {
		if existing, _, lookupErr := s.store.Definition(r.Context(), d.Name); lookupErr == nil && existing.Authority == "file" {
			writeError(w, 409, "authority_conflict", d.Name+" is managed by a file")
			return
		} else if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			internal(w, lookupErr)
			return
		}
	}
	if err = s.store.CopyDefinitions(r.Context(), defs, "api:import"); err != nil {
		internal(w, err)
		return
	}
	if err = s.reload(r.Context()); err != nil {
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
	body, err := webAssets.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	// Assets ship under fixed filenames, so freshness must be revalidated on
	// every load: max-age would let the browser keep serving a stale bundle
	// for up to an hour after the daemon is rebuilt. no-cache + content-hash
	// ETag gives cheap 304 revalidation and instant pickup of new builds.
	etag := `"` + hex.EncodeToString(sha256Sum(body))[:16] + `"`
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
	body, err := webAssets.ReadFile("assets/index.html")
	if err != nil {
		internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Always revalidate the SPA shell so it picks up new asset bundles.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{apiError{code, message, nil}})
}
func internal(w http.ResponseWriter, err error) { writeError(w, 500, "internal_error", err.Error()) }
func writeSSE(w io.Writer, event string, id uint64, value any) {
	b, _ := json.Marshal(value)
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, b)
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
