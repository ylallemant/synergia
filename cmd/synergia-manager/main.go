package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/acme"
	"github.com/ylallemant/synergia/internal/manager/api"
	adminapi "github.com/ylallemant/synergia/internal/manager/admin/api"
	"github.com/ylallemant/synergia/internal/manager/admin/auth"
	adminsrv "github.com/ylallemant/synergia/internal/manager/admin/server"
	"github.com/ylallemant/synergia/internal/manager/cache"
	"github.com/ylallemant/synergia/internal/manager/config"
	"github.com/ylallemant/synergia/internal/manager/gateway"
	"github.com/ylallemant/synergia/internal/manager/latency"
	"github.com/ylallemant/synergia/internal/manager/models"
	"github.com/ylallemant/synergia/internal/manager/queue"
	"github.com/ylallemant/synergia/internal/manager/store"
)

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

// version and commit are set at build time via -ldflags:
//   -X main.version=x.y.z  -X main.commit=abc1234
// version is also persisted to the DB; a change triggers a binary-cache purge.
var version = "dev"
var commit  = "unknown"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version":
			fmt.Printf("synergia-manager %s (commit: %s)\n", version, commit)
			os.Exit(0)
		}
	}

	initLogger()

	// Parse CLI flags
	for _, arg := range os.Args[1:] {
		if arg == "--development" || arg == "-development" {
			os.Setenv("CLUSTER_DEVELOPMENT", "true")
		}
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("configuration error")
	}

	// Initialize components
	var db *store.Store
	if cfg.DBDSN != "" {
		db, err = store.OpenPostgres(cfg.DBDSN)
	} else {
		db, err = store.Open(cfg.DBPath)
	}
	if err != nil {
		log.Fatal().Err(err).Msg("database error")
	}

	// Purge binary download cache when the manager version changes so workers
	// always receive a freshly-patched binary that matches the current code.
	checkAndPurgeStaleCache(db, cfg.CacheDir)

	// Seed role-model mappings
	if cfg.TestSetup {
		log.Warn().Msg("test-setup mode — seeding role-model mappings with minimal test models")
		log.Debug().Msg("test-setup: all roles use SmolLM2-135M-Instruct with 512 MB min VRAM — real production roles require 4-20 GB")
		log.Debug().Msg("test-setup: production roles are embedding=bge-m3(4GB), inference=mistral-nemo-12b(10GB), ingestion=mistral-small-3.1-24b(20GB)")
		if err := db.SeedTestRoles(); err != nil {
			log.Fatal().Err(err).Msg("failed to seed test roles")
		}
	} else {
		// Seed production roles if none exist yet
		roles, _ := db.GetRoleModels()
		if len(roles) == 0 {
			log.Info().Msg("no role-model mappings found — seeding production defaults")
			if err := db.SeedProductionRoles(); err != nil {
				log.Fatal().Err(err).Msg("failed to seed production roles")
			}
		}
	}

	// Always ensure the tester role exists (allows any hardware to participate)
	if err := db.SeedTesterRole(); err != nil {
		log.Fatal().Err(err).Msg("failed to seed tester role")
	}

	q := queue.New()
	gw := gateway.New(cfg.WorkerKey, q, db)
	completions := api.NewCompletionsHandler(cfg.APIKey, gw, q, db, cfg.Timeout)
	synergiaAPI := api.NewSynergiaAPI(cfg.APIKey, db)
	batchHandler := api.NewBatchHandler(cfg.APIKey, gw, q, db, cfg.Timeout, cfg.Development)

	// Initialize model store
	var modelStore models.Store
	switch cfg.ModelBackend {
	case "s3":
		modelStore, err = models.NewS3Store(cfg.ModelS3Endpoint, cfg.ModelS3Bucket, cfg.ModelS3Region, cfg.ModelS3Key, cfg.ModelS3Secret, cfg.ModelS3SSL)
	default:
		modelStore, err = models.NewFilesystemStore(cfg.ModelPath)
	}
	if err != nil {
		log.Fatal().Err(err).Msg("model store error")
	}
	modelsAPI := api.NewModelsDownloadAPI(cfg.WorkerKey, modelStore)

	// Compute and store file hashes for any roles that have a model filename but no file hash yet
	roles, _ := db.GetRoleModels()
	for _, r := range roles {
		if r.ModelFilename != "" && r.ModelFileHash == "" {
			ctx := context.Background()
			hash, hashErr := modelStore.FileHash(ctx, r.ModelFilename)
			if hashErr != nil {
				log.Warn().Err(hashErr).Str("role", r.Role).Str("filename", r.ModelFilename).Msg("could not compute model file hash")
				continue
			}
			if err := db.UpsertRoleModel(r.Role, r.LLMModel, r.Quantisation, r.ModelFilename, hash, r.MinVRAMMB, r.Description); err != nil {
				log.Warn().Err(err).Str("role", r.Role).Msg("failed to update model file hash")
			} else {
				log.Info().Str("role", r.Role).Str("hash", hash[:16]+"...").Msg("model file hash computed and stored")
			}
		}
	}

	consentAPI := api.NewConsentAPI(cfg.WorkerKey, db)
	// Worker-facing APIs
	brandingAPI := api.NewBrandingAPI(cfg.WorkerKey, db)
	rolesAPI := api.NewRolesAPI(cfg.APIKey, cfg.WorkerKey, db, cfg.TestSetup)
	errorsAPI := api.NewErrorsAPI(cfg.WorkerKey, db)
	versionAPI := api.NewVersionAPI()
	backendAPI := api.NewBackendAPI(cfg.WorkerKey, db, filepath.Join(cfg.CacheDir, "backend"))

	// Initialize latency monitor
	latencyMonitor := latency.New(db, cfg.LatencyBuckets, cfg.LatencyWindowHours)
	defer latencyMonitor.Stop()
	defer batchHandler.Stop()
	adminCache := cache.New(db)

	// Admin-facing APIs
	latencyAPI := adminapi.NewLatencyAPI(latencyMonitor)
	adminVersionAPI := adminapi.NewAdminVersionAPI(db, gw, adminCache)
	adminBackendAPI := adminapi.NewAdminBackendAPI(db, gw, adminCache)
	adminRolesAPI := adminapi.NewAdminRolesAPI(db)
	adminRolesAPI.SetGateway(gw)
	adminRolesAPI.SetModelStore(modelStore)
	adminBrandingAPI := adminapi.NewAdminBrandingAPI(brandingAPI)

	// Make latency monitor available to completions handler
	completions.SetLatencyMonitor(latencyMonitor)
	batchHandler.SetLatencyMonitor(latencyMonitor)

	// Set up routes
	mux := http.NewServeMux()

	// ACME HTTP-01 challenge passthrough — registered first so it shadows
	// the `/` catch-all (community page) and prevents certificate
	// issuance traffic from being answered with HTML or auth challenges.
	acme.RegisterPassthrough(mux)

	// OpenAI-compatible endpoints (called by Flow Engine)
	mux.Handle("/v1/chat/completions", completions)
	mux.Handle("/v1/batches/", batchHandler)
	mux.Handle("/v1/batches", batchHandler)
	mux.HandleFunc("/v1/models", synergiaAPI.ModelsHandler)

	// Cluster management API
	mux.HandleFunc("/v1/workers", synergiaAPI.WorkersHandler)
	mux.HandleFunc("/v1/work-units", synergiaAPI.WorkUnitsHandler)
	mux.HandleFunc("/v1/stats", synergiaAPI.StatsHandler)

	// Worker consent and configuration API (authenticated with worker key)
	mux.HandleFunc("/v1/consent", consentAPI.ConsentHandler)
	mux.HandleFunc("/v1/worker-config", consentAPI.ConfigHandler)

	// Roles API (worker-facing — eligible roles query)
	mux.HandleFunc("/v1/roles", rolesAPI.RolesHandler)

	// Branding API (worker-facing — CSS served to workers)
	mux.HandleFunc("/v1/branding/style.css", brandingAPI.StyleHandler)

	// Client binary download proxy (worker-facing)
	mux.HandleFunc("/v1/binary/download", versionAPI.BinaryDownloadHandler)

	// Backend (llama-server) binary download (worker-facing)
	mux.HandleFunc("/v1/backend/download", backendAPI.BackendDownloadHandler)

	// Model download API (authenticated with worker key)
	mux.HandleFunc("/v1/models/files", modelsAPI.ListModelsHandler)
	mux.HandleFunc("/v1/models/download/", modelsAPI.DownloadHandler)

	// Client error reporting API (authenticated with worker key)
	mux.HandleFunc("/v1/errors", errorsAPI.ErrorsHandler)

	// Public pages (no auth required)
	downloadAPI := api.NewDownloadAPI(cfg.ClientBinaryDir, cfg.CacheDir, adminCache)
	communityAPI := api.NewCommunityAPI(db)
	mux.HandleFunc("/download/", downloadAPI.BinaryHandler)
	mux.HandleFunc("/download", downloadAPI.DownloadPageHandler)
	mux.HandleFunc("/install", downloadAPI.InstallHandler)
	mux.HandleFunc("/community", communityAPI.PageHandler)
	mux.HandleFunc("/v1/community/stats", communityAPI.StatsHandler)
	mux.HandleFunc("/", communityAPI.PageHandler)

	// Worker WebSocket gateway
	mux.Handle("/ws/worker", gw)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		status := map[string]any{
			"status":       "ok",
			"worker_ready": gw.HasWorker(),
		}
		if info := gw.WorkerStatus(); info != nil {
			status["worker"] = map[string]any{
				"fingerprint":  info.Fingerprint,
				"model":        info.Model,
				"quantisation": info.Quantisation,
				"connected_at": info.ConnectedAt.Format(time.RFC3339),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","worker_ready":%v}`, gw.HasWorker())
	})

	server := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: cfg.Timeout + 10*time.Second, // allow timeout + buffer
		IdleTimeout:  120 * time.Second,
	}

	// Administration server (separate port)

	// Worker auth config stored via admin UI overrides env vars at startup
	if wac, err := db.GetWorkerAuthConfig(); err == nil && wac != nil {
		if wac.TOFUEnabled {
			cfg.WorkerKey = ""
		} else if wac.WorkerKey != "" {
			cfg.WorkerKey = wac.WorkerKey
		}
	}

	// OIDC config stored via admin UI overrides env vars at startup
	if oidcDB, err := db.GetOIDCConfig(); err == nil && oidcDB != nil {
		cfg.OIDCEnabled = oidcDB.Enabled
		if oidcDB.ClientID != "" {
			cfg.OIDCClientID = oidcDB.ClientID
		}
		if oidcDB.ClientSecret != "" {
			cfg.OIDCClientSecret = oidcDB.ClientSecret
		}
		if oidcDB.ProviderURL != "" {
			cfg.OIDCProviderURL = oidcDB.ProviderURL
		}
		if oidcDB.RedirectURL != "" {
			cfg.OIDCRedirectURL = oidcDB.RedirectURL
		}
	}

	authConfig := auth.Config{
		AdminUser:        cfg.AdminUser,
		AdminPassword:    cfg.AdminPassword,
		OIDCEnabled:      cfg.OIDCEnabled,
		OIDCClientID:     cfg.OIDCClientID,
		OIDCClientSecret: cfg.OIDCClientSecret,
		OIDCProviderURL:  cfg.OIDCProviderURL,
		OIDCRedirectURL:  cfg.OIDCRedirectURL,
		Insecure:         cfg.Insecure,
	}
	authInstance, err := auth.New(authConfig)
	if err != nil {
		log.Fatal().Err(err).Msg("auth initialization error")
	}
	adminServer := adminsrv.New(cfg.AdminAddr, cfg.APIKey, db, adminCache, cfg.Insecure, cfg.TLSCertFile, cfg.TLSKeyFile, authInstance)
	adminServer.HandleFuncAdmin("/v1/latency", latencyAPI.LatencyHandler)
	adminServer.HandleFuncAdmin("/v1/latency/config", latencyAPI.ConfigHandler)
	adminServer.HandleFuncAdmin("/v1/admin/version", adminVersionAPI.AdminVersionHandler)
	adminServer.HandleFuncAdmin("/v1/admin/version/tags", adminVersionAPI.AdminVersionTagsHandler)
	adminServer.HandleFuncAdmin("/v1/admin/backend", adminBackendAPI.AdminBackendHandler)
	adminServer.HandleFuncAdmin("/v1/admin/backend/tags", adminBackendAPI.AdminBackendTagsHandler)
	adminServer.HandleFuncAdmin("/v1/admin/backend/names", adminBackendAPI.AdminBackendNamesHandler)
	adminServer.HandleFuncAdmin("/v1/admin/roles", adminRolesAPI.AdminRolesHandler)
	adminServer.HandleFuncAdmin("/v1/admin/branding/css", adminBrandingAPI.AdminUpdateHandler)
	adminOIDCAPI := adminapi.NewAdminOIDCAPI(db)
	adminServer.HandleFuncAdmin("/v1/admin/oidc", adminOIDCAPI.ConfigHandler)
	adminWorkersAPI := adminapi.NewAdminWorkersAPI(db)
	adminServer.HandleFuncAdmin("/v1/admin/workers", adminWorkersAPI.ConfigHandler)
	adminStatsAPI := adminapi.NewAdminStatsAPI(adminCache)
	adminServer.HandleFuncAdmin("/v1/admin/stats", adminStatsAPI.StatsHandler)

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.Insecure {
		log.Warn().Msg("TLS disabled — running in insecure mode (traffic is unencrypted)")
	} else {
		// Start HTTP→HTTPS redirect server if configured
		httpRedirectAddr := os.Getenv("CLUSTER_HTTP_REDIRECT_ADDR")
		if httpRedirectAddr != "" {
			go func() {
				redirectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					target := "https://" + r.Host + r.URL.RequestURI()
					log.Debug().Str("from", r.URL.String()).Str("to", target).Str("remote", r.RemoteAddr).Msg("redirecting HTTP to HTTPS")
					http.Redirect(w, r, target, http.StatusMovedPermanently)
				})
				// Intercept ACME HTTP-01 challenge paths before redirecting.
				// LE permits redirects but the cleaner contract is: this
				// server is for HTTPS upgrades only, ACME validation is
				// handled by the ingress / solver layer.
				redirectServer := &http.Server{
					Addr:    httpRedirectAddr,
					Handler: acme.WrapHandler(redirectHandler),
				}
				log.Info().Str("addr", httpRedirectAddr).Msg("HTTP redirect server starting (redirects to HTTPS)")
				if err := redirectServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Warn().Err(err).Msg("HTTP redirect server error")
				}
			}()
		}
	}

	go func() {
		log.Info().Str("addr", cfg.ListenAddr).Bool("tls", !cfg.Insecure).Msg("cluster manager starting")
		var err error
		if cfg.Insecure {
			err = server.ListenAndServe()
		} else {
			err = server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	go adminServer.Run(ctx)
	adminCache.Start(ctx)

	<-ctx.Done()
	log.Info().Msg("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("shutdown error")
	}

	log.Info().Msg("cluster manager stopped")
}

// checkAndPurgeStaleCache compares the running manager version against the
// last-seen version stored in the DB. On a version change (including first
// start), the client-binary download cache is purged so workers always receive
// a freshly-patched binary that matches the current sentinel format.
func checkAndPurgeStaleCache(s *store.Store, cacheDir string) {
	prev, _ := s.GetSetting("manager_version")
	if prev == version {
		return
	}
	if prev != "" {
		log.Info().Str("prev", prev).Str("current", version).Msg("manager version changed — purging binary download cache")
	} else {
		log.Debug().Str("version", version).Msg("first start — initialising manager version in DB")
	}
	binCacheDir := filepath.Join(cacheDir, "client-binaries")
	if err := os.RemoveAll(binCacheDir); err != nil {
		log.Warn().Err(err).Str("dir", binCacheDir).Msg("failed to purge binary download cache")
	}
	if err := s.SetSetting("manager_version", version); err != nil {
		log.Warn().Err(err).Msg("failed to persist manager version")
	}
}
