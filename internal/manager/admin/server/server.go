package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/logbuffer"
	"github.com/ylallemant/synergia/internal/manager/acme"
	"github.com/ylallemant/synergia/internal/manager/admin/auth"
	"github.com/ylallemant/synergia/internal/manager/cache"
	"github.com/ylallemant/synergia/internal/manager/store"
)

//go:embed static/*
var staticFS embed.FS

var dashboardTmpl = template.Must(template.ParseFS(staticFS, "static/index.html"))
var oidcTmpl = template.Must(template.ParseFS(staticFS, "static/oidc.html"))
var workersTmpl = template.Must(template.ParseFS(staticFS, "static/workers.html"))
var inferenceTmpl = template.Must(template.ParseFS(staticFS, "static/inference.html"))
var logsTmpl = template.Must(template.ParseFS(staticFS, "static/logs.html"))

// Server serves the admin dashboard and additional admin API routes.
type Server struct {
	mu             sync.RWMutex
	addr           string
	apiKey         string
	managerVersion string
	store          *store.Store
	cache          *cache.Cache
	insecure       bool
	tlsCertFile    string
	tlsKeyFile     string
	auth           *auth.Auth
	mux            *http.ServeMux
	server         *http.Server
	logBuf         *logbuffer.Buffer
	logFilePath    string
}

// New creates a new admin dashboard server.
func New(addr, apiKey, managerVersion string, s *store.Store, c *cache.Cache, insecure bool, tlsCertFile, tlsKeyFile string, a *auth.Auth) *Server {
	srv := &Server{
		addr:           addr,
		apiKey:         apiKey,
		managerVersion: managerVersion,
		store:          s,
		cache:          c,
		insecure:       insecure,
		tlsCertFile:    tlsCertFile,
		tlsKeyFile:     tlsKeyFile,
		auth:           a,
		mux:         http.NewServeMux(),
	}

	// ACME HTTP-01 challenge passthrough — must be registered BEFORE the
	// `/` catch-all so the longest-prefix match short-circuits the auth
	// wrapper in dashboardHandler. Without this, cert-manager (and any
	// other ACME client routing through this listener) gets 401 from the
	// dashboard's auth check and certificate issuance fails.
	acme.RegisterPassthrough(srv.mux)

	// Serve static CSS
	staticSub, _ := fs.Sub(staticFS, "static")
	srv.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Auth routes
	srv.mux.HandleFunc("/login", srv.auth.LoginHandler)
	srv.mux.HandleFunc("/logout", srv.auth.LogoutHandler)
	if srv.auth.Config.OIDCEnabled {
		srv.mux.HandleFunc("/auth/oidc/login", srv.auth.OIDCLoginHandler)
		srv.mux.HandleFunc("/auth/oidc/callback", srv.auth.OIDCCallbackHandler)
	}

	// Dashboard and settings pages
	srv.mux.HandleFunc("/", srv.auth.RequireAuth(srv.dashboardHandler))
	srv.mux.HandleFunc("/admin/workers", srv.auth.RequireAuth(srv.workersHandler))
	srv.mux.HandleFunc("/admin/inference", srv.auth.RequireAuth(srv.inferenceHandler))
	srv.mux.HandleFunc("/admin/oidc", srv.auth.RequireAuth(srv.oidcHandler))
	srv.mux.HandleFunc("/admin/logs", srv.auth.RequireAuth(srv.logsHandler))

	// Log streaming APIs
	srv.mux.HandleFunc("/api/manager/logs", srv.auth.RequireAuth(srv.handleManagerLogs))
	srv.mux.HandleFunc("/api/manager/log-level", srv.auth.RequireAuth(srv.handleManagerLogLevel))
	srv.mux.HandleFunc("/api/manager/logs-file", srv.auth.RequireAuth(srv.handleManagerLogsFile))

	return srv
}

// SetLogBuffer wires the ring-buffer and optional log file path into the server.
// Call this after New() and before Run(). Safe to skip — the Logs page will
// simply show no entries if not called.
func (s *Server) SetLogBuffer(buf *logbuffer.Buffer, filePath string) {
	s.logBuf = buf
	s.logFilePath = filePath
}

// HandleFunc registers an additional handler on the admin mux.
func (s *Server) HandleFunc(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
}

// HandleFuncAdmin registers a handler on the admin mux protected by session or
// Bearer token. Browser logins use the session cookie; automated / test access
// uses Authorization: Bearer <apiKey>.
func (s *Server) HandleFuncAdmin(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, s.auth.RequireAuthOrBearerFn(s.getAPIKey, handler))
}

// SetAPIKey updates the Bearer token used for admin API authentication.
func (s *Server) SetAPIKey(key string) {
	s.mu.Lock()
	s.apiKey = key
	s.mu.Unlock()
}

func (s *Server) getAPIKey() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.apiKey
}

// Run starts the admin server. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	s.server = &http.Server{
		Addr:         s.addr,
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	log.Info().Str("addr", s.addr).Bool("tls", !s.insecure).Msg("admin server starting")
	var err error
	if s.insecure {
		err = s.server.ListenAndServe()
	} else {
		err = s.server.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("admin server error")
	}
}

type pageData struct {
	APIKey         string
	ManagerVersion string
}

// inferenceHandler serves GET /admin/inference — the inference settings page.
func (s *Server) inferenceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := inferenceTmpl.Execute(w, pageData{APIKey: s.apiKey, ManagerVersion: s.managerVersion}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// workersHandler serves GET /admin/workers — the worker authentication settings page.
func (s *Server) workersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := workersTmpl.Execute(w, pageData{APIKey: s.apiKey, ManagerVersion: s.managerVersion}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

type oidcPageData = pageData

// oidcHandler serves GET /admin/oidc — the OIDC settings page.
func (s *Server) oidcHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := oidcTmpl.Execute(w, pageData{APIKey: s.apiKey, ManagerVersion: s.managerVersion}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// dashboardHandler serves GET / on the admin port.
func (s *Server) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := s.collectData()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) collectData() dashboardData {
	var data dashboardData
	data.APIKey = s.apiKey
	data.ManagerVersion = s.managerVersion

	stats := s.cache.GetStats()

	data.TotalWorkers = stats.TotalWorkers
	data.ReadyWorkers = stats.ReadyWorkers
	data.ProcessingWorkers = stats.ProcessingWorkers
	data.UnavailableWorkers = stats.UnavailableWorkers
	data.OfflineWorkers = stats.OfflineWorkers
	data.DeletedWorkers = stats.DeletedWorkers

	for _, rc := range stats.RoleCounts {
		data.RoleCounts = append(data.RoleCounts, roleCount{Role: rc.Role, Online: rc.Online, Total: rc.Total})
	}

	data.TodayTotal = stats.TodayTotal
	data.TodayCompleted = stats.TodayCompleted
	data.TodayQueued = stats.TodayQueued
	data.TodayTimeout = stats.TodayTimeout
	data.TodayFailed = stats.TodayFailed

	for _, rw := range stats.RoleWorkCounts {
		data.RoleWorkCounts = append(data.RoleWorkCounts, roleWorkCount{Role: rw.Role, Total: rw.Total})
	}

	for _, e := range stats.Errors {
		data.Errors = append(data.Errors, errorEntry{
			Fingerprint: e.Fingerprint,
			Version:     e.Version,
			Error:       e.Error,
			ReportedAt:  e.ReportedAt,
		})
	}

	data.VersionTarget = stats.VersionTarget
	data.VersionRolloutMode = stats.VersionRolloutMode
	data.VersionPercentage = stats.VersionPercentage
	data.WorkersSynced = stats.WorkersSynced
	data.WorkersOutdated = stats.WorkersOutdated

	data.BackendName = stats.BackendName
	data.BackendVersion = stats.BackendVersion
	data.BackendDownloadURL = stats.BackendDownloadURL
	data.BackendSHA256Full = stats.BackendSHA256Full
	sha := stats.BackendSHA256Full
	if len(sha) > 16 {
		sha = sha[:16] + "…"
	}
	data.BackendSHA256 = sha
	data.BackendSynced = stats.BackendSynced
	data.BackendOutdated = stats.BackendOutdated
	data.ModelSynced = stats.ModelSynced
	data.ModelOutOfSync = stats.ModelOutOfSync
	data.AvgWorkerGPU = stats.AvgWorkerGPU

	for _, r := range stats.Roles {
		data.Roles = append(data.Roles, roleEntry{
			Role:          r.Role,
			Model:         r.Model,
			Quantisation:  r.Quantisation,
			Filename:      r.Filename,
			ModelFileHash: r.ModelFileHash,
			MinVRAMMB:     r.MinVRAMMB,
			Description:   r.Description,
		})
	}

	data.GeneratedAt = time.Now().Format("2006-01-02 15:04:05 MST")
	return data
}

type dashboardData struct {
	APIKey             string
	ManagerVersion     string
	TotalWorkers       int64
	ReadyWorkers       int64
	ProcessingWorkers  int64
	UnavailableWorkers int64
	OfflineWorkers     int64
	DeletedWorkers     int64
	RoleCounts         []roleCount
	TodayTotal         int64
	TodayCompleted     int64
	TodayQueued        int64
	TodayTimeout       int64
	TodayFailed        int64
	RoleWorkCounts     []roleWorkCount
	Errors             []errorEntry
	VersionTarget      string
	VersionRolloutMode string
	VersionPercentage  int
	VersionSHA256      string
	WorkersSynced      int64
	WorkersOutdated    int64
	BackendName        string
	BackendVersion     string
	BackendDownloadURL string
	BackendSHA256Full  string
	BackendSHA256      string
	BackendSynced      int64
	BackendOutdated    int64
	ModelSynced        int64
	ModelOutOfSync     int64
	AvgWorkerGPU       int
	Roles              []roleEntry
	GeneratedAt        string
}

type roleEntry struct {
	Role          string
	Model         string
	Quantisation  string
	Filename      string
	ModelFileHash string
	MinVRAMMB     int
	Description   string
}

type roleCount struct {
	Role   string
	Online int64
	Total  int64
}

type roleWorkCount struct {
	Role      string
	Completed int64
	Failed    int64
	Total     int64
}

type errorEntry struct {
	Fingerprint string
	Version     string
	Error       string
	ReportedAt  string
}

// logsHandler serves GET /admin/logs — the manager log viewer page.
func (s *Server) logsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := logsTmpl.Execute(w, pageData{APIKey: s.apiKey, ManagerVersion: s.managerVersion}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// handleManagerLogs streams manager log entries via Server-Sent Events.
// On connect it replays the ring buffer, then pushes new lines in real time.
func (s *Server) handleManagerLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	// Clear the server's write deadline so the long-lived SSE connection is not
	// killed by the 30s WriteTimeout set on the http.Server.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if s.logBuf == nil {
		flusher.Flush()
		<-r.Context().Done()
		return
	}

	for _, entry := range s.logBuf.GetAll() {
		fmt.Fprintf(w, "data: %s\n\n", entry)
	}
	flusher.Flush()

	id, ch := s.logBuf.Subscribe()
	defer s.logBuf.Unsubscribe(id)
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}

// handleManagerLogLevel gets or sets the zerolog global log level at runtime.
// GET  → {"level":"info"}
// POST → {"level":"debug"}  — takes effect immediately, no restart needed.
func (s *Server) handleManagerLogLevel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]string{"level": zerolog.GlobalLevel().String()}) //nolint:errcheck
	case http.MethodPost:
		var req struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		lvl, err := zerolog.ParseLevel(strings.ToLower(req.Level))
		if err != nil {
			http.Error(w, "unknown level: "+req.Level, http.StatusBadRequest)
			return
		}
		zerolog.SetGlobalLevel(lvl)
		log.Info().Str("level", lvl.String()).Msg("log level changed")
		json.NewEncoder(w).Encode(map[string]string{"level": lvl.String()}) //nolint:errcheck
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleManagerLogsFile serves the last 1 MB of the manager log file so the
// Logs page can load history beyond the 500-entry ring buffer.
// Returns 404 when no log file path has been configured.
func (s *Server) handleManagerLogsFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logFilePath == "" {
		http.Error(w, "log file not available", http.StatusNotFound)
		return
	}
	f, err := os.Open(s.logFilePath)
	if err != nil {
		http.Error(w, "failed to open log file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	const maxBytes = 1 << 20 // 1 MB
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "failed to stat log file", http.StatusInternalServerError)
		return
	}
	offset := info.Size() - maxBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		http.Error(w, "failed to seek log file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.Copy(w, f) //nolint:errcheck
}
