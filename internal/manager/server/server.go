package server

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
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
	insecure    bool
	tlsCertFile string
	tlsKeyFile  string
	mux         *http.ServeMux
	server      *http.Server
}

// New creates a new admin dashboard server.
func New(addr, apiKey, workerKey string, s *store.Store, insecure bool, tlsCertFile, tlsKeyFile string) *Server {
	srv := &Server{
		addr:        addr,
		apiKey:      apiKey,
		workerKey:   workerKey,
		store:       s,
		insecure:    insecure,
		tlsCertFile: tlsCertFile,
		tlsKeyFile:  tlsKeyFile,
		mux:         http.NewServeMux(),
	}

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

	// Worker counts by status (aggregated: available = status available/online AND sync_status synced)
	s.store.DB.Model(&store.Worker{}).Count(&data.TotalWorkers)
	s.store.DB.Model(&store.Worker{}).Where("status IN ? AND sync_status = ?", []string{"available", "online"}, "synced").Count(&data.ReadyWorkers)
	s.store.DB.Model(&store.Worker{}).Where("status = ? AND sync_status = ?", "processing", "synced").Count(&data.ProcessingWorkers)
	s.store.DB.Model(&store.Worker{}).Where("status != ? AND NOT (status IN ? AND sync_status = ?) AND NOT (status = ? AND sync_status = ?)", "offline", []string{"available", "online"}, "synced", "processing", "synced").Count(&data.UnavailableWorkers)
	s.store.DB.Model(&store.Worker{}).Where("status = ?", "offline").Count(&data.OfflineWorkers)

	// Workers by role
	type roleRow struct {
		Role   string
		Online int64
		Total  int64
	}
	var roleRows []roleRow
	s.store.DB.Raw(`
		SELECT
		  CASE WHEN wc.preferred_role IS NOT NULL AND wc.preferred_role != '' THEN wc.preferred_role ELSE 'inference' END AS role,
		  COUNT(*) AS total,
		  SUM(CASE WHEN w.status IN ('available','processing') THEN 1 ELSE 0 END) AS online
		FROM workers w
		LEFT JOIN worker_configs wc ON wc.fingerprint = w.fingerprint
		GROUP BY role
		ORDER BY role
	`).Scan(&roleRows)
	for _, rr := range roleRows {
		data.RoleCounts = append(data.RoleCounts, roleCount{
			Role:   rr.Role,
			Online: rr.Online,
			Total:  rr.Total,
		})
	}

	// Today's work units
	startOfDay := time.Now().Truncate(24 * time.Hour)
	s.store.DB.Model(&store.WorkUnit{}).Where("created_at >= ?", startOfDay).Count(&data.TodayTotal)
	s.store.DB.Model(&store.WorkUnit{}).Where("created_at >= ? AND status = ?", startOfDay, "completed").Count(&data.TodayCompleted)
	s.store.DB.Model(&store.WorkUnit{}).Where("created_at >= ? AND status = ?", startOfDay, "timeout").Count(&data.TodayTimeout)
	s.store.DB.Model(&store.WorkUnit{}).Where("created_at >= ? AND status = ?", startOfDay, "failed").Count(&data.TodayFailed)
	s.store.DB.Model(&store.BatchRequest{}).Where("status IN ?", []string{"pending", "processing"}).Count(&data.TodayQueued)

	// Work units by role today
	type roleWorkRow struct {
		Role  string
		Total int64
	}
	var rwRows []roleWorkRow
	s.store.DB.Raw(`
		SELECT role, COUNT(*) AS total
		FROM latency_samples
		WHERE created_at >= ?
		GROUP BY role
		ORDER BY role
	`, startOfDay).Scan(&rwRows)
	for _, rw := range rwRows {
		data.RoleWorkCounts = append(data.RoleWorkCounts, roleWorkCount{
			Role:  rw.Role,
			Total: rw.Total,
		})
	}

	// Last 10 errors
	var errors []store.ClientError
	s.store.DB.Order("reported_at desc").Limit(10).Find(&errors)
	for _, e := range errors {
		errMsg := e.ErrorMessage
		if len(errMsg) > 120 {
			errMsg = errMsg[:120] + "…"
		}
		data.Errors = append(data.Errors, errorEntry{
			Fingerprint: truncFP(e.Fingerprint),
			Version:     e.Version,
			Error:       errMsg,
			ReportedAt:  e.ReportedAt.Format("2006-01-02 15:04:05"),
		})
	}

	// Binary version config
	if cfg, err := s.store.GetClientVersionConfig(); err == nil {
		data.VersionTarget = cfg.TargetVersion
		data.VersionRolloutMode = cfg.RolloutMode
		data.VersionPercentage = cfg.RolloutPercentage
	}
	if data.VersionTarget != "" {
		s.store.DB.Model(&store.Worker{}).Where("status != ? AND client_version = ?", "offline", data.VersionTarget).Count(&data.WorkersSynced)
		s.store.DB.Model(&store.Worker{}).Where("status != ? AND client_version != ?", "offline", data.VersionTarget).Count(&data.WorkersOutdated)
	}

	data.GeneratedAt = time.Now().Format("2006-01-02 15:04:05 MST")
	return data
}

func truncFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12] + "…"
	}
	return fp
}

type dashboardData struct {
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
	WorkersSynced      int64
	WorkersOutdated    int64
	GeneratedAt        string
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
