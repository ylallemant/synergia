package server

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/acme"
	"github.com/ylallemant/synergia/internal/manager/cache"
	"github.com/ylallemant/synergia/internal/manager/store"
)

//go:embed static/*
var staticFS embed.FS

var dashboardTmpl = template.Must(template.ParseFS(staticFS, "static/index.html"))

// Server serves the admin dashboard and additional admin API routes.
type Server struct {
	addr        string
	apiKey      string
	workerKey   string
	store       *store.Store
	cache       *cache.Cache
	insecure    bool
	tlsCertFile string
	tlsKeyFile  string
	mux         *http.ServeMux
	server      *http.Server
}

// New creates a new admin dashboard server.
func New(addr, apiKey, workerKey string, s *store.Store, c *cache.Cache, insecure bool, tlsCertFile, tlsKeyFile string) *Server {
	srv := &Server{
		addr:        addr,
		apiKey:      apiKey,
		workerKey:   workerKey,
		store:       s,
		cache:       c,
		insecure:    insecure,
		tlsCertFile: tlsCertFile,
		tlsKeyFile:  tlsKeyFile,
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

	// Dashboard handler
	srv.mux.HandleFunc("/", srv.dashboardHandler)

	return srv
}

// HandleFunc registers an additional handler on the admin mux.
func (s *Server) HandleFunc(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
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

// dashboardHandler serves GET / on the admin port.
func (s *Server) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	data := s.collectData()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) authenticate(r *http.Request) bool {
	if key := r.Header.Get("X-API-Key"); key == s.apiKey || key == s.workerKey {
		return true
	}
	if auth := r.Header.Get("Authorization"); auth == "Bearer "+s.apiKey || auth == "Bearer "+s.workerKey {
		return true
	}
	// Allow query param for browser access
	if key := r.URL.Query().Get("key"); key == s.apiKey || key == s.workerKey {
		return true
	}
	return false
}

func (s *Server) collectData() dashboardData {
	var data dashboardData
	data.APIKey = s.apiKey

	stats := s.cache.GetStats()

	data.TotalWorkers = stats.TotalWorkers
	data.ReadyWorkers = stats.ReadyWorkers
	data.ProcessingWorkers = stats.ProcessingWorkers
	data.UnavailableWorkers = stats.UnavailableWorkers
	data.OfflineWorkers = stats.OfflineWorkers

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
	TotalWorkers       int64
	ReadyWorkers       int64
	ProcessingWorkers  int64
	UnavailableWorkers int64
	OfflineWorkers     int64
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
