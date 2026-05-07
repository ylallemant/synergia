package config

import (
	"fmt"
	"os"
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

	// Test mode
	TestSetup   bool // If true, seed role-model mappings with minimal test models
	Development bool // If true, batch processes sequentially with random 1-5s delays

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
}

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:         envOrDefault("CLUSTER_LISTEN_ADDR", ":7500"),
		APIKey:             os.Getenv("CLUSTER_API_KEY"),
		WorkerKey:          os.Getenv("CLUSTER_WORKER_KEY"),
		DBPath:             envOrDefault("CLUSTER_DB_PATH", "cluster-manager.db"),
		DBDSN:              os.Getenv("CLUSTER_DB_DSN"),
		Insecure:           os.Getenv("CLUSTER_INSECURE") == "true",
		TestSetup:          os.Getenv("CLUSTER_TEST_SETUP") == "true",
		Development:        os.Getenv("CLUSTER_DEVELOPMENT") == "true",
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
	}

	if cfg.APIKey == "" {
		return nil, fmt.Errorf("CLUSTER_API_KEY is required")
	}
	if cfg.WorkerKey == "" {
		return nil, fmt.Errorf("CLUSTER_WORKER_KEY is required")
	}
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
