package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	ListenAddr string
	APIKey     string
	WorkerKey  string
	Timeout    time.Duration
	DBPath     string
	DBDSN      string // PostgreSQL DSN; if set, takes precedence over DBPath (SQLite)

	// TLS
	Insecure    bool   // If true, serve plain HTTP (no TLS)
	TLSCertFile string // Path to TLS certificate (PEM)
	TLSKeyFile  string // Path to TLS private key (PEM)

	// Test / development mode
	TestSetup        bool   // If true, seed role-model mappings with minimal test models
	Development      bool   // If true, seed all test roles and auto-configure backend/client versions
	DevBackendURL    string // Development: backend download URL (empty = fetch latest from GitHub)
	DevClientVersion string // Development: client version target (e.g. "0.1.0-dev")

	// Model storage
	ModelBackend    string // "filesystem" or "s3"
	ModelPath       string // filesystem path (used when ModelBackend == "filesystem")
	ModelS3Endpoint string // S3 endpoint URL (used when ModelBackend == "s3")
	ModelS3Bucket   string // S3 bucket name
	ModelS3Region   string // S3 region
	ModelS3Key      string // S3 access key
	ModelS3Secret   string // S3 secret key
	ModelS3SSL      bool   // Use HTTPS for S3

	// Administration
	AdminAddr string // Separate listener for admin-only endpoints

	// Cache
	CacheDir string // Directory for cached downloads (backend binaries, etc.)

	// Latency monitoring
	LatencyBuckets     int // Number of payload-size buckets for the latency matrix
	LatencyWindowHours int // Rolling window in hours for sample retention

	// Client distribution
	ClientBinaryDir string // Directory containing pre-built generic client binaries

	// Authentication
	AdminUser        string // Default admin username
	AdminPassword    string // Default admin password
	OIDCEnabled      bool   // Enable OIDC authentication
	OIDCClientID     string // OIDC client ID
	OIDCClientSecret string // OIDC client secret
	OIDCProviderURL  string // OIDC provider issuer URL
	OIDCRedirectURL  string // OIDC redirect URL
}

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:         envOrDefault("CLUSTER_LISTEN_ADDR", ":7500"),
		APIKey:             os.Getenv("CLUSTER_API_KEY"),
		WorkerKey:          os.Getenv("CLUSTER_WORKER_KEY"),
		DBPath:             envOrDefault("CLUSTER_DB_PATH", "cluster-manager.db"),
		DBDSN:              os.Getenv("CLUSTER_DB_DSN"),
		Insecure:           os.Getenv("CLUSTER_INSECURE") == "true",
		TestSetup:        os.Getenv("CLUSTER_TEST_SETUP") == "true",
		Development:      os.Getenv("CLUSTER_DEVELOPMENT") == "true",
		DevBackendURL:    os.Getenv("CLUSTER_DEV_BACKEND_URL"),
		DevClientVersion: os.Getenv("CLUSTER_DEV_CLIENT_VERSION"),
		TLSCertFile:        os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:         os.Getenv("TLS_KEY_FILE"),
		ModelBackend:       envOrDefault("CLUSTER_MODEL_BACKEND", "filesystem"),
		ModelPath:          envOrDefault("CLUSTER_MODEL_PATH", "./models"),
		ModelS3Endpoint:    os.Getenv("CLUSTER_MODEL_S3_ENDPOINT"),
		ModelS3Bucket:      envOrDefault("CLUSTER_MODEL_S3_BUCKET", "synergia-models"),
		ModelS3Region:      envOrDefault("CLUSTER_MODEL_S3_REGION", "us-east-1"),
		ModelS3Key:         os.Getenv("CLUSTER_MODEL_S3_KEY"),
		ModelS3Secret:      os.Getenv("CLUSTER_MODEL_S3_SECRET"),
		ModelS3SSL:         envOrDefault("CLUSTER_MODEL_S3_SSL", "true") == "true",
		AdminAddr:          envOrDefault("CLUSTER_ADMIN_ADDR", ":7501"),
		CacheDir:           envOrDefault("CLUSTER_CACHE_DIR", "./cache"),
		LatencyBuckets:     envOrDefaultInt("CLUSTER_LATENCY_BUCKETS", 4),
		LatencyWindowHours: envOrDefaultInt("CLUSTER_LATENCY_WINDOW_HOURS", 48),
		ClientBinaryDir:    envOrDefault("CLUSTER_CLIENT_BINARY_DIR", "./binaries"),
		AdminUser:          envOrDefault("CLUSTER_ADMIN_USER", "admin"),
		AdminPassword:      envOrDefault("CLUSTER_ADMIN_PASSWORD", "synergia"),
		OIDCEnabled:        os.Getenv("CLUSTER_OIDC_ENABLED") == "true",
		OIDCClientID:       os.Getenv("CLUSTER_OIDC_CLIENT_ID"),
		OIDCClientSecret:   os.Getenv("CLUSTER_OIDC_CLIENT_SECRET"),
		OIDCProviderURL:    os.Getenv("CLUSTER_OIDC_PROVIDER_URL"),
		OIDCRedirectURL:    envOrDefault("CLUSTER_OIDC_REDIRECT_URL", "http://localhost:7501/auth/oidc/callback"),
	}

	if cfg.APIKey == "" {
		return nil, fmt.Errorf("CLUSTER_API_KEY is required")
	}
	// WorkerKey is optional: empty = TOFU mode (Ed25519 challenge-response), non-empty = key-auth mode.
	if !cfg.Insecure {
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			return nil, fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE are required (set CLUSTER_INSECURE=true to disable TLS)")
		}
	}

	timeoutStr := envOrDefault("CLUSTER_TIMEOUT", "120s")
	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return nil, fmt.Errorf("invalid CLUSTER_TIMEOUT %q: %w", timeoutStr, err)
	}
	cfg.Timeout = timeout

	// Validate model backend
	switch cfg.ModelBackend {
	case "filesystem":
		// ModelPath is used
	case "s3":
		if cfg.ModelS3Endpoint == "" {
			return nil, fmt.Errorf("CLUSTER_MODEL_S3_ENDPOINT is required when model backend is s3")
		}
		if cfg.ModelS3Key == "" || cfg.ModelS3Secret == "" {
			return nil, fmt.Errorf("CLUSTER_MODEL_S3_KEY and CLUSTER_MODEL_S3_SECRET are required when model backend is s3")
		}
	default:
		return nil, fmt.Errorf("invalid CLUSTER_MODEL_BACKEND %q: must be \"filesystem\" or \"s3\"", cfg.ModelBackend)
	}

	// Ensure filesystem directories exist
	for _, dir := range []string{cfg.CacheDir, cfg.ClientBinaryDir, filepath.Dir(cfg.DBPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
	}
	if cfg.ModelBackend == "filesystem" {
		if err := os.MkdirAll(cfg.ModelPath, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create directory %q: %w", cfg.ModelPath, err)
		}
	}

	return cfg, nil
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envOrDefaultInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}
