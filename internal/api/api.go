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
	store     *store.Store
	logs      *logstore.Store
	exec      *executor.Service
	super     *supervisor.Supervisor
	reload    func(context.Context) error
	started   time.Time
	version   string
	schema    []byte
	ready     atomic.Bool
	tokenHash atomic.Value
	tcp       *http.Server
	unix      *http.Server
	idemMu    sync.Mutex
}

//go:embed assets/*
var webAssets embed.FS

type localKey struct{}
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

func New(st *store.Store, logs *logstore.Store, ex *executor.Service, sup *supervisor.Supervisor, reload func(context.Context) error, version string, _ []byte) *Server {
	s := &Server{store: st, logs: logs, exec: ex, super: sup, reload: reload, started: time.Now(), version: version, schema: OpenAPIContract(version)}
	s.tokenHash.Store("")
	return s
}
func (s *Server) InitializeToken(ctx context.Context) (string, error) {
	hash, err := s.store.Meta(ctx, "token_hash")
	if err == nil {
		s.tokenHash.Store(hash)
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
	s.tokenHash.Store(hash)
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
	go s.tcp.Serve(ln)
	if socket != "" {
		os.Remove(socket)
		ln, err := net.Listen("unix", socket)
		if err != nil {
			s.tcp.Close()
			return err
		}
		if err = os.Chmod(socket, 0o600); err != nil {
			return err
		}
		s.unix = &http.Server{Handler: s.middleware(mux, true), ReadHeaderTimeout: 5 * time.Second}
		go s.unix.Serve(peerListener{Listener: ln, uid: uint32(os.Geteuid())})
	}
	return nil
}
func (s *Server) Shutdown(ctx context.Context) error {
	var errs []error
	if s.tcp != nil {
		errs = append(errs, s.tcp.Shutdown(ctx))
	}
	if s.unix != nil {
		errs = append(errs, s.unix.Shutdown(ctx))
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
			expected, _ := hex.DecodeString(s.tokenHash.Load().(string))
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
	m.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.schema)
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
	writeJSON(w, 200, map[string]any{"version": s.version, "schema_version": store.SchemaVersion, "uptime_s": int64(time.Since(s.started).Seconds()), "capabilities": []string{"local", "file-logs"}, "token_fingerprint": fingerprint(s.tokenHash.Load().(string))})
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
	writeJSON(w, 200, map[string]any{"items": defs})
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
		code := "validation_failed"
		status := 422
		if strings.Contains(err.Error(), "authority conflict") {
			code = "authority_conflict"
			status = 409
		}
		if strings.Contains(err.Error(), "revision conflict") {
			code = "revision_conflict"
			status = 412
		}
		writeError(w, status, code, err.Error())
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
		status := 404
		code := "not_found"
		if strings.Contains(err.Error(), "authority conflict") {
			status = 409
			code = "authority_conflict"
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

func (s *Server) trigger(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	requestHash := fmt.Sprintf("%x", sha256.Sum256([]byte(r.PathValue("name"))))
	if key != "" {
		s.idemMu.Lock()
		defer s.idemMu.Unlock()
		if id, err := s.store.IdempotentRun(r.Context(), "admin", "trigger", key, requestHash); err == nil {
			existing, getErr := s.store.Run(r.Context(), id)
			if getErr != nil {
				internal(w, getErr)
				return
			}
			writeJSON(w, 200, existing)
			return
		} else if !errors.Is(err, sql.ErrNoRows) {
			writeError(w, 409, "idempotency_conflict", err.Error())
			return
		}
	}
	d, hash, err := s.store.Definition(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, 404, "not_found", "definition not found")
		return
	}
	run, err := s.exec.Trigger(r.Context(), d, hash, "manual", nil)
	if err != nil {
		writeError(w, 409, "trigger_rejected", err.Error())
		return
	}
	if key != "" {
		if err := s.store.SaveIdempotency(r.Context(), "admin", "trigger", key, requestHash, run.ID); err != nil {
			internal(w, err)
			return
		}
	}
	if r.URL.Query().Get("wait") == "true" {
		deadline := time.Now().Add(min(parseSeconds(r.URL.Query().Get("timeout")), 240) * time.Second)
		for time.Now().Before(deadline) {
			current, e := s.store.Run(r.Context(), run.ID)
			if e == nil && model.TerminalStatuses[current.Status] {
				writeJSON(w, 200, current)
				return
			}
			time.Sleep(100 * time.Millisecond)
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
	writeJSON(w, 200, map[string]string{"token": token, "fingerprint": fingerprint(s.tokenHash.Load().(string))})
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
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body)
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
func parseSeconds(v string) time.Duration {
	n, _ := strconv.Atoi(v)
	if n <= 0 {
		n = 120
	}
	return time.Duration(n)
}
func fingerprint(hash string) string {
	if len(hash) < 12 {
		return hash
	}
	return hash[:12]
}
