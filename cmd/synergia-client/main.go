package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/client/autostart"
	"github.com/ylallemant/synergia/internal/client/branding"
	"github.com/ylallemant/synergia/internal/client/config"
	"github.com/ylallemant/synergia/internal/client/connection"
	"github.com/ylallemant/synergia/internal/client/consent"
	"github.com/ylallemant/synergia/internal/client/errorreporter"
	"github.com/ylallemant/synergia/internal/client/gpu"
	"github.com/ylallemant/synergia/internal/client/identity"
	"github.com/ylallemant/synergia/internal/client/llm"
	"github.com/ylallemant/synergia/internal/client/protocol"
	"github.com/ylallemant/synergia/internal/client/server"
	"github.com/ylallemant/synergia/internal/client/status"
	"github.com/ylallemant/synergia/internal/client/tray"
	"github.com/ylallemant/synergia/internal/client/updater"
	"github.com/ylallemant/synergia/internal/client/version"
	"github.com/ylallemant/synergia/internal/client/worker"
	"github.com/ylallemant/synergia/internal/client/workerconfig"
)

const dashboardAddr = "127.0.0.1:9876"

func initLogger() {
	noColor := true
	if fi, err := os.Stdout.Stat(); err == nil {
		noColor = (fi.Mode() & os.ModeCharDevice) == 0
	}

	output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339, NoColor: noColor}
	output.FormatLevel = func(i interface{}) string {
		return strings.ToUpper(fmt.Sprintf("| %-6s|", i))
	}
	zerolog.CallerMarshalFunc = func(pc uintptr, file string, line int) string {
		return filepath.Base(file) + ":" + strconv.Itoa(line)
	}

	level := zerolog.InfoLevel
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		if parsed, err := zerolog.ParseLevel(strings.ToLower(lvl)); err == nil {
			level = parsed
		}
	}
	zerolog.SetGlobalLevel(level)

	log.Logger = zerolog.New(output).With().Timestamp().Caller().Logger()
}

func main() {
	initLogger()

	// Top-level panic recovery — report to manager if possible, then re-panic
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Msg("unrecovered panic in main")
			panic(r) // re-panic to get the default stack trace output
		}
	}()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("configuration error")
	}

	// Configure global HTTP transport with custom CA if provided
	if cfg.TLSCACert != "" {
		caCert, err := os.ReadFile(cfg.TLSCACert)
		if err != nil {
			log.Fatal().Err(err).Str("path", cfg.TLSCACert).Msg("failed to read TLS CA certificate")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			log.Fatal().Str("path", cfg.TLSCACert).Msg("failed to parse TLS CA certificate")
		}
		http.DefaultTransport = &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		}
	}

	// Log when following HTTP redirects
	http.DefaultClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 {
			log.Debug().Str("from", via[len(via)-1].URL.String()).Str("to", req.URL.String()).Msg("following redirect")
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}

	// Load or create worker identity
	id, err := identity.LoadOrCreate(cfg.DataDir)
	if err != nil {
		log.Fatal().Err(err).Msg("identity error")
	}

	// Initialize components
	managerHTTPURL := wsToHTTP(cfg.ManagerURL)
	llmClient := llm.NewClient(cfg.LLMURL)
	conn := connection.New(cfg.ManagerURL, cfg.WorkerKey, id, cfg.Model, cfg.Quantisation, cfg.TLSCACert)

	// Compute initial LLM hash from model file (if provided) or role+model+quant fallback
	var initialHash string
	if cfg.ModelFile != "" {
		fileHash, err := protocol.HashFile(cfg.ModelFile)
		if err != nil {
			log.Fatal().Err(err).Str("path", cfg.ModelFile).Msg("failed to hash model file")
		}
		initialHash = protocol.ComputeLLMHash(cfg.Role, fileHash)
		log.Info().
			Str("model_file", cfg.ModelFile).
			Str("file_hash", fileHash[:16]+"...").
			Str("llm_hash", initialHash[:16]+"...").
			Msg("model file hash computed")
	}
	conn.SetLLMHash(initialHash)

	monitor := gpu.NewMonitor(cfg.GPUMonitorInterval, cfg.GPUContentionThresh, cfg.GPUResumeDelay)
	consentMgr := consent.New(cfg.DataDir, managerHTTPURL, cfg.WorkerKey, id.Fingerprint, cfg.AutoApprove)

	// Propagate GPU driver info detected by the prober to the consent manager
	gpuDriver, gpuDriverVer := monitor.GPUDriverInfo()
	consentMgr.SetGPUDriverInfo(gpuDriver, gpuDriverVer)
	configMgr := workerconfig.New(cfg.DataDir, managerHTTPURL, cfg.WorkerKey, id.Fingerprint)
	brandingMgr := branding.New(cfg.DataDir, managerHTTPURL, cfg.WorkerKey)
	autostartMgr := autostart.New(execPath(), os.Args[1:])
	reporter := errorreporter.New(managerHTTPURL, cfg.WorkerKey, id.Fingerprint, version.Version)
	sp := status.New(conn, monitor, llmClient, id, cfg.Model, cfg.Quantisation)
	w := worker.New(conn, llmClient, id, monitor, sp, sp, sp, reporter, consentMgr)
	w.SetModelDownloadConfig(cfg.Role, filepath.Dir(cfg.ModelFile), managerHTTPURL, cfg.WorkerKey)

	// Configure binary auto-updater
	binaryUpdater := updater.New(cfg.WorkerKey, managerHTTPURL)
	w.SetUpdater(binaryUpdater, func() {
		log.Info().Msg("restarting after binary update")
		os.Exit(0) // systemd/launchd/autostart will restart the process
	})
	srv := server.New(dashboardAddr, sp, consentMgr, configMgr, brandingMgr, autostartMgr)
	adminURL := localAdminURL(cfg.ManagerURL, cfg.WorkerKey)
	t := tray.New(sp, "http://"+dashboardAddr+"/static/index.html", adminURL)

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.Insecure {
		log.Warn().Msg("TLS disabled — running in insecure mode (traffic is unencrypted)")
	}

	log.Info().
		Str("fingerprint", id.Fingerprint).
		Str("manager", cfg.ManagerURL).
		Str("llm", cfg.LLMURL).
		Str("model", cfg.Model).
		Str("quantisation", cfg.Quantisation).
		Str("version", version.Version).
		Str("dashboard", "http://"+dashboardAddr).
		Bool("consent_accepted", consentMgr.IsAccepted()).
		Msg("cluster client starting")

	// Sync consent, config, and branding AFTER WebSocket connection is established
	go syncWithManager(ctx, conn, consentMgr, configMgr, brandingMgr, reporter)
	defer brandingMgr.Stop()

	// Calibrate GPU baseline (sample 5 times, 1s apart, +5% headroom)
	monitor.CalibrateBaseline(5, time.Second, 5)

	// Start GPU monitor
	go monitor.Run(ctx)

	// Start LLM health monitor (check every 10s)
	go llmClient.MonitorHealth(ctx, 10*time.Second)

	// Start WebSocket connection (reconnects automatically)
	go conn.Run(ctx)

	// Start dashboard server
	go srv.Run(ctx)

	// Start work processing loop in background
	go w.Run(ctx)

	// Periodically update the system tray icon with current state
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.UpdateStatus(sp.IsConnected(), sp.GPUState(), sp.IsProcessing(), sp.IsPaused())
			}
		}
	}()

	// Handle tray quit signal
	go func() {
		select {
		case <-t.QuitCh():
			stop()
		case <-ctx.Done():
		}
	}()

	// Handle tray pause/resume signals
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.PauseCh():
				sp.SetPaused(true)
				log.Info().Msg("worker paused by user")
				_ = conn.SendStatus("paused")
			case <-t.ResumeCh():
				sp.SetPaused(false)
				log.Info().Msg("worker resumed by user")
				_ = conn.SendStatus("available")
			}
		}
	}()

	// System tray must run on the main thread (macOS requirement)
	t.Run()

	log.Info().Msg("cluster client stopped")
}

// wsToHTTP converts a WebSocket URL to its HTTP equivalent for REST API calls.
// e.g., "ws://localhost:7500/ws/worker" → "http://localhost:7500"
func wsToHTTP(wsURL string) string {
	u := wsURL
	u = strings.Replace(u, "wss://", "https://", 1)
	u = strings.Replace(u, "ws://", "http://", 1)
	// Strip the path (keep only scheme + host)
	if idx := strings.Index(u, "//"); idx != -1 {
		rest := u[idx+2:]
		if pathIdx := strings.Index(rest, "/"); pathIdx != -1 {
			u = u[:idx+2+pathIdx]
		}
	}
	return u
}

// localAdminURL returns the admin dashboard URL if the manager is on localhost.
// The admin port is the manager port + 1. Returns "" if not localhost.
func localAdminURL(managerWSURL, apiKey string) string {
	wsURL := strings.Replace(managerWSURL, "wss://", "https://", 1)
	wsURL = strings.Replace(wsURL, "ws://", "http://", 1)

	u, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}

	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return ""
	}

	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	portNum, err := strconv.Atoi(port)
	if err != nil {
		return ""
	}

	adminPort := portNum + 1
	return fmt.Sprintf("%s://%s:%d/?key=%s", u.Scheme, host, adminPort, apiKey)
}

// execPath returns the absolute path to the current executable.
func execPath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	// Resolve symlinks to get the real path
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe
	}
	return real
}

// syncWithManager waits for the WebSocket connection, then syncs consent, config, and branding
// with the manager. Uses a retry strategy: 3 attempts every 5s, then randomized 30-90s intervals.
func syncWithManager(ctx context.Context, conn *connection.Connection, consentMgr *consent.Manager, configMgr *workerconfig.Manager, brandingMgr *branding.Manager, reporter *errorreporter.Reporter) {
	// Wait for the WebSocket connection to be established
	select {
	case <-conn.Connected():
	case <-ctx.Done():
		return
	}

	log.Info().Msg("WebSocket connected — starting manager sync")

	syncFn := func() bool {
		var failed bool
		if consentMgr.IsAccepted() {
			if err := consentMgr.SyncWithManager(); err != nil {
				log.Warn().Err(err).Msg("consent sync failed")
				reporter.Report(err)
				failed = true
			}
			if err := configMgr.SyncWithManager(); err != nil {
				log.Warn().Err(err).Msg("config sync failed")
				reporter.Report(err)
				failed = true
			}
		}
		brandingMgr.FetchFromManager()
		return !failed
	}

	// First 3 attempts: every 5 seconds
	for attempt := 0; attempt < 3; attempt++ {
		if syncFn() {
			brandingMgr.StartPeriodicRefresh()
			return
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}

	// Continuous retry: randomized 30-90 seconds
	for {
		if syncFn() {
			brandingMgr.StartPeriodicRefresh()
			return
		}
		wait := 30*time.Second + time.Duration(rand.Int63n(int64(60*time.Second)))
		log.Debug().Dur("retry_in", wait).Msg("manager sync retry scheduled")
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}
