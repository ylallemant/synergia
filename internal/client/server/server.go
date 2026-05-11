package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/client/autostart"
	"github.com/ylallemant/synergia/internal/client/branding"
	"github.com/ylallemant/synergia/internal/client/consent"
	"github.com/ylallemant/synergia/internal/client/gpu"
	"github.com/ylallemant/synergia/internal/client/hwinfo"
	"github.com/ylallemant/synergia/internal/client/logbuffer"
	"github.com/ylallemant/synergia/internal/client/workerconfig"
)

//go:embed static/*
var staticFS embed.FS

// StatusProvider supplies live worker state to the server.
type StatusProvider interface {
	IsConnected() bool
	IsProcessing() bool
	IsPaused() bool
	GPUState() gpu.State
	GPUUtilization() int
	GPUSupported() (bool, string)
	GPUDriverInfo() (string, string)
	LLMReachable() (bool, string)
	Fingerprint() string
	Model() string
	Quantisation() string
	UnitsProcessed() int64
	Uptime() time.Duration
}

// Server provides a local HTTP API and dashboard on localhost.
type Server struct {
	addr        string
	status      StatusProvider
	consent     *consent.Manager
	config      *workerconfig.Manager
	branding    *branding.Manager
	autostart   *autostart.Manager
	logBuf      *logbuffer.Buffer
	logFilePath string // path to the per-run log file; empty if unavailable
	dataDir     string // worker data directory (identity, config, models, backend)
	hwInfo      hwinfo.Info
	server      *http.Server

	// onManagerURLSet is called when the user configures a manager URL in setup mode.
	onManagerURLSet func(url string)
	// onGoodbye is called during uninstall to notify the manager before exit.
	onGoodbye func()
	// uninstallFn replaces doUninstall in tests so os.Exit is never called.
	uninstallFn func()
}

// SetGoodbyeCallback registers the function called when the worker uninstalls.
func (s *Server) SetGoodbyeCallback(fn func()) { s.onGoodbye = fn }

// BuildGoodbyeBody constructs the signed goodbye JSON body that the worker
// sends to the manager on uninstall. Extracted as a package-level function
// so it can be unit-tested independently of the HTTP plumbing.
//
//   - fingerprint: hex-encoded SHA256 of the worker's Ed25519 public key
//   - signFn: func that signs a byte slice and returns a hex-encoded signature
//   - now: the timestamp to embed (caller controls for deterministic tests)
func BuildGoodbyeBody(fingerprint string, signFn func([]byte) string, now time.Time) ([]byte, error) {
	payload := "goodbye:" + fingerprint + ":" + now.UTC().Format(time.RFC3339)
	sig := signFn([]byte(payload))
	return json.Marshal(map[string]string{
		"fingerprint": fingerprint,
		"payload":     payload,
		"signature":   sig,
	})
}

func New(addr string, status StatusProvider, consentMgr *consent.Manager, configMgr *workerconfig.Manager, brandingMgr *branding.Manager, autostartMgr *autostart.Manager, buf *logbuffer.Buffer, logFilePath, dataDir string) *Server {
	return &Server{
		addr:        addr,
		status:      status,
		consent:     consentMgr,
		config:      configMgr,
		branding:    brandingMgr,
		autostart:   autostartMgr,
		logBuf:      buf,
		logFilePath: logFilePath,
		dataDir:     dataDir,
		hwInfo:      hwinfo.Collect(),
	}
}

// SetManagerURLCallback sets the function called when the user submits a manager URL.
func (s *Server) SetManagerURLCallback(fn func(url string)) {
	s.onManagerURLSet = fn
}

// Run starts the HTTP server. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	mux := http.NewServeMux()

	// API endpoints
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/consent", s.handleConsent)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/roles", s.handleRoles)
	mux.HandleFunc("/api/autostart", s.handleAutostart)
	mux.HandleFunc("/api/hardware-info", s.handleHardwareInfo)
	mux.HandleFunc("/api/manager-url", s.handleManagerURL)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/logs-file", s.handleLogsFile)
	mux.HandleFunc("/api/log-level", s.handleLogLevel)
	mux.HandleFunc("/api/uninstall", s.handleUninstall)

	// Dynamic branding CSS (served from manager cache)
	mux.HandleFunc("/static/branding.css", s.handleBrandingCSS)

	// Static dashboard
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	s.server = &http.Server{
		Addr:         s.addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	log.Info().Str("addr", s.addr).Msg("dashboard server starting")
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error().Err(err).Msg("dashboard server error")
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	consentState := s.consent.GetState()
	cfg := s.config.Get()
	gpuSupported, gpuSupportReason := s.status.GPUSupported()
	gpuDriver, gpuDriverVersion := s.status.GPUDriverInfo()
	llmReachable, llmError := s.status.LLMReachable()

	status := map[string]any{
		"connected":          s.status.IsConnected(),
		"processing":         s.status.IsProcessing(),
		"paused":             s.status.IsPaused(),
		"gpu_state":          s.status.GPUState().String(),
		"gpu_utilization":    s.status.GPUUtilization(),
		"gpu_supported":      gpuSupported,
		"gpu_support_reason": gpuSupportReason,
		"gpu_driver":         gpuDriver,
		"gpu_driver_version": gpuDriverVersion,
		"llm_reachable":      llmReachable,
		"llm_error":          llmError,
		"fingerprint":        s.status.Fingerprint(),
		"model":              s.status.Model(),
		"quantisation":       s.status.Quantisation(),
		"units_processed":    s.status.UnitsProcessed(),
		"uptime_seconds":     int(s.status.Uptime().Seconds()),
		"consent": map[string]any{
			"accepted":           consentState.Accepted,
			"hardware_stats":     consentState.HardwareStats,
			"config_preferences": consentState.ConfigPreferences,
		},
		"config": map[string]any{
			"preferred_role": cfg.PreferredRole,
			"nickname":       cfg.Nickname,
		},
		"hardware": map[string]any{
			"os":                 s.hwInfo.OS,
			"os_version":         s.hwInfo.OSVer,
			"gpu":                s.hwInfo.GPU,
			"gpu_driver":         gpuDriver,
			"gpu_driver_version": gpuDriverVersion,
			"vram_mb":            s.hwInfo.VRAMMB,
			"cpu":                s.hwInfo.CPU,
			"cpu_cores":          s.hwInfo.CPUCores,
			"ram_mb":             s.hwInfo.RAMMB,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok"}`)
}

func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		return
	}

	switch r.Method {
	case http.MethodGet:
		state := s.consent.GetState()
		json.NewEncoder(w).Encode(state)

	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		var req struct {
			Accepted          bool `json:"accepted"`
			HardwareStats     bool `json:"hardware_stats"`
			ConfigPreferences bool `json:"config_preferences"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		if req.Accepted {
			if err := s.consent.Accept(req.HardwareStats, req.ConfigPreferences); err != nil {
				log.Warn().Err(err).Msg("consent sync failed (will retry on next connect)")
			}
		} else {
			if err := s.consent.Revoke(); err != nil {
				log.Warn().Err(err).Msg("consent revoke sync failed")
			}
		}

		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		return
	}

	switch r.Method {
	case http.MethodGet:
		cfg := s.config.Get()
		json.NewEncoder(w).Encode(cfg)

	case http.MethodPost:
		// Consent required before config can be set
		if !s.consent.IsAccepted() {
			http.Error(w, `{"error":"consent required before setting configuration"}`, http.StatusForbidden)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		var cfg workerconfig.Config
		if err := json.Unmarshal(body, &cfg); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		if err := s.config.Update(cfg); err != nil {
			log.Warn().Err(err).Msg("config sync failed (will retry on next connect)")
		}

		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleHardwareInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.config.Get()
	gpuDriver, gpuDriverVersion := s.status.GPUDriverInfo()

	// This is the full payload that gets synced to the cluster manager
	payload := map[string]any{
		"fingerprint": s.status.Fingerprint(),
		"hardware": map[string]any{
			"os":                 s.hwInfo.OS,
			"os_version":         s.hwInfo.OSVer,
			"gpu":                s.hwInfo.GPU,
			"gpu_driver":         gpuDriver,
			"gpu_driver_version": gpuDriverVersion,
			"vram_mb":            s.hwInfo.VRAMMB,
			"cpu":                s.hwInfo.CPU,
			"cpu_cores":          s.hwInfo.CPUCores,
			"ram_mb":             s.hwInfo.RAMMB,
		},
		"config": map[string]any{
			"preferred_role": cfg.PreferredRole,
			"nickname":       cfg.Nickname,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleBrandingCSS(w http.ResponseWriter, r *http.Request) {
	css := s.branding.CSS()
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(css)
}

func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	roles, err := s.config.FetchEligibleRoles()
	if err != nil {
		log.Warn().Err(err).Msg("failed to fetch roles from manager")
		// Return tester fallback so the UI never shows "Hardware Insufficient"
		// when the manager is temporarily unreachable.
		json.NewEncoder(w).Encode([]map[string]any{{
			"role":         "tester",
			"model":        "SmolLM2-135M-Instruct",
			"quantisation": "Q4_K_M",
			"min_vram_mb":  512,
			"description":  "Connectivity testing — minimal model for any hardware",
			"eligible":     true,
		}})
		return
	}
	json.NewEncoder(w).Encode(roles)
}

func (s *Server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		return
	}

	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]any{
			"supported":  s.autostart.IsSupported(),
			"enabled":    s.autostart.IsEnabled(),
			"executable": s.autostart.ExecPath(),
		})

	case http.MethodPost:
		if !s.autostart.IsSupported() {
			http.Error(w, `{"error":"autostart not supported on this platform"}`, http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		if req.Enabled {
			if err := s.autostart.Enable(); err != nil {
				log.Error().Err(err).Msg("failed to enable autostart")
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			log.Info().Msg("autostart enabled")
		} else {
			if err := s.autostart.Disable(); err != nil {
				log.Error().Err(err).Msg("failed to disable autostart")
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			log.Info().Msg("autostart disabled")
		}

		// Re-check actual OS state to confirm the change took effect
		actual := s.autostart.IsEnabled()
		if actual != req.Enabled {
			log.Warn().Bool("requested", req.Enabled).Bool("actual", actual).Msg("autostart state mismatch after toggle")
		}

		json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"enabled": actual,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleManagerURL(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		http.Error(w, `{"error":"url is required"}`, http.StatusBadRequest)
		return
	}

	if s.onManagerURLSet != nil {
		s.onManagerURLSet(req.URL)
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleUninstall removes the client binary, app bundle, autostart service,
// cached llama.cpp binary, downloaded models, and non-identity config files,
// then exits so the process cleans up fully. Identity keypair files are
// preserved so the manager still recognises the worker on reinstall.
// Returns 400 on Windows where deleting a running binary is not supported.
func (s *Server) handleUninstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if runtime.GOOS == "windows" {
		http.Error(w,
			"Uninstall is not supported from the dashboard on Windows. "+
				"Use the Synergia entry in Windows Settings → Apps to uninstall.",
			http.StatusBadRequest)
		return
	}

	log.Info().Msg("uninstall requested — cleaning up")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "uninstalling"})

	// Flush the response before we exit.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Give the browser a moment to receive the response, then notify the
	// manager and run cleanup. Goodbye runs first so the manager marks the
	// worker deleted before we go dark.
	uninstall := s.uninstallFn
	if uninstall == nil {
		uninstall = s.doUninstall
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		if s.onGoodbye != nil {
			s.onGoodbye()
		}
		uninstall()
	}()
}

// identityFiles are the keypair files created by the identity package.
// They are preserved on uninstall so the manager still recognises the worker.
var identityFiles = map[string]bool{
	"identity.enc": true,
	"identity.pub": true,
	"fingerprint":  true,
}

func (s *Server) doUninstall() {
	// 1. Disable and remove the autostart service.
	if s.autostart != nil {
		if err := s.autostart.Disable(); err != nil {
			log.Warn().Err(err).Msg("uninstall: failed to disable autostart")
		}
	}

	// 2. Remove cached llama-server binary.
	backendDir := filepath.Join(s.dataDir, "backend")
	if err := os.RemoveAll(backendDir); err != nil {
		log.Warn().Err(err).Str("dir", backendDir).Msg("uninstall: failed to remove backend dir")
	}

	// 3. Remove downloaded model files.
	modelsDir := filepath.Join(s.dataDir, "models")
	if err := os.RemoveAll(modelsDir); err != nil {
		log.Warn().Err(err).Str("dir", modelsDir).Msg("uninstall: failed to remove models dir")
	}

	// 4. Remove known config / log files — but NOT the identity keypair.
	toDelete := []string{"manager-url", "worker-key", "client.log", "stdout.log", "stderr.log"}
	for _, name := range toDelete {
		path := filepath.Join(s.dataDir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Warn().Err(err).Str("file", path).Msg("uninstall: failed to remove file")
		}
	}

	// 5. Remove the client binary (safe on macOS/Linux — kernel holds inode open).
	exe, err := os.Executable()
	if err == nil {
		exe, _ = filepath.EvalSymlinks(exe)

		// If running inside a .app bundle, remove the whole bundle.
		if idx := strings.Index(exe, ".app/Contents/MacOS/"); idx != -1 {
			appBundle := exe[:idx+4] // up to and including ".app"
			log.Info().Str("bundle", appBundle).Msg("uninstall: removing .app bundle")
			os.RemoveAll(appBundle)
		} else {
			log.Info().Str("binary", exe).Msg("uninstall: removing binary")
			os.Remove(exe)
		}

		// Also remove the binary from ~/.local/bin if it exists there separately.
		if home, err := os.UserHomeDir(); err == nil {
			localBin := filepath.Join(home, ".local", "bin", "synergia-client")
			if localBin != exe {
				os.Remove(localBin)
			}
		}
	}

	log.Info().Msg("uninstall complete")
	os.Exit(0)
}

// handleLogsFile serves the full per-run log file as newline-delimited JSON so
// the dashboard can load history beyond the 500-entry ring buffer.
func (s *Server) handleLogsFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logFilePath == "" {
		http.Error(w, "log file not available", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(s.logFilePath)
	if err != nil {
		http.Error(w, "failed to read log file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// handleLogLevel gets or sets the zerolog global log level at runtime.
// GET  → {"level":"info"}
// POST → {"level":"debug"}  sets the level immediately, no restart needed.
func (s *Server) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]string{"level": zerolog.GlobalLevel().String()})
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
		json.NewEncoder(w).Encode(map[string]string{"level": lvl.String()})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLogs streams log entries to the client via Server-Sent Events.
// On connect it replays the ring buffer, then pushes new lines in real time.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Replay history
	for _, entry := range s.logBuf.GetAll() {
		fmt.Fprintf(w, "data: %s\n\n", entry)
	}
	flusher.Flush()

	// Stream new entries
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
