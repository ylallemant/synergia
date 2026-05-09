package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// DefaultManagerURL is a fixed-size sentinel that can be replaced at binary distribution time.
// The manager's download endpoint patches this with the real WSS URL (null-padded to 256 bytes).
// If still set to the sentinel (or empty), the client starts in unconfigured mode.
var DefaultManagerURL = buildDefaultManagerURL()

const defaultManagerURLSentinelSize = 256

var defaultManagerURLSentinelBytes = []byte{
	'$', '$', 'S', 'Y', 'N', 'E', 'R', 'G', 'I', 'A', '_', 'M', 'A', 'N', 'A', 'G', 'E', 'R', '_', 'U', 'R', 'L', '$', '$',
}

func defaultManagerURLSentinel() string {
	return string(defaultManagerURLSentinelBytes)
}

func buildDefaultManagerURL() string {
	data := make([]byte, defaultManagerURLSentinelSize)
	copy(data, defaultManagerURLSentinelBytes)
	return string(data)
}

// resolveManagerURL returns the effective manager URL from (in priority order):
// 1. CLI flag / env var (if non-empty and not the sentinel)
// 2. Patched sentinel value (if patched)
// 3. Empty string (unconfigured)
func resolveManagerURL() string {
	// Strip null padding from sentinel
	url := strings.TrimRight(DefaultManagerURL, "\x00")
	if url == defaultManagerURLSentinel() || url == "" {
		return ""
	}
	return url
}

type Config struct {
	ManagerURL          string
	WorkerKey           string
	LLMURL              string
	Model               string
	Quantisation        string
	Role                string
	ModelFile           string
	DataDir             string
	GPUMonitorInterval  time.Duration
	GPUContentionThresh int
	GPUResumeDelay      time.Duration
	AutoApprove         bool
	Insecure            bool
	TLSCACert           string
	Unconfigured        bool // true when no manager URL is available
}

func Load() (*Config, error) {
	cfg := &Config{}

	// Resolve the effective default: env var > patched sentinel > empty
	defaultURL := envOrDefault("CLUSTER_MANAGER_URL", resolveManagerURL())

	flag.StringVar(&cfg.ManagerURL, "manager-url", defaultURL, "WebSocket URL of the cluster manager")
	flag.StringVar(&cfg.WorkerKey, "worker-key", os.Getenv("CLUSTER_WORKER_KEY"), "Shared secret for WebSocket auth")
	flag.StringVar(&cfg.LLMURL, "llm-url", envOrDefault("WORKER_LLM_URL", "http://localhost:8080"), "Local llama-server endpoint")
	flag.StringVar(&cfg.Model, "model", envOrDefault("WORKER_MODEL", "SmolLM2-135M-Instruct"), "Model name to report")
	flag.StringVar(&cfg.Quantisation, "quantisation", envOrDefault("WORKER_QUANTISATION", "Q4_K_M"), "Quantisation level to report")
	flag.StringVar(&cfg.Role, "role", envOrDefault("WORKER_ROLE", "tester"), "Worker role (embedding, inference, ingestion, tester)")
	flag.StringVar(&cfg.ModelFile, "model-file", os.Getenv("WORKER_MODEL_FILE"), "Path to the GGUF model file (for hash verification)")
	flag.StringVar(&cfg.DataDir, "data-dir", envOrDefault("CLUSTER_CLIENT_DATA_DIR", defaultDataDir()), "Directory for identity keystore and local state")
	flag.BoolVar(&cfg.AutoApprove, "auto-approve", envOrDefault("CLUSTER_CLIENT_AUTO_APPROVE", "") == "true", "Automatically accept data collection terms (for testing)")
	flag.BoolVar(&cfg.Insecure, "insecure", envOrDefault("CLUSTER_INSECURE", "") == "true", "Connect without TLS (ws:// instead of wss://)")
	flag.StringVar(&cfg.TLSCACert, "tls-ca-cert", os.Getenv("TLS_CA_CERT"), "Path to CA certificate for verifying the manager's TLS cert")

	var gpuInterval string
	flag.StringVar(&gpuInterval, "gpu-monitor-interval", envOrDefault("GPU_MONITOR_INTERVAL", "5s"), "How often to poll GPU utilization")
	flag.IntVar(&cfg.GPUContentionThresh, "gpu-contention-threshold", envOrDefaultInt("GPU_CONTENTION_THRESHOLD", 15), "Percentage above baseline that triggers idle state")
	var gpuResume string
	flag.StringVar(&gpuResume, "gpu-resume-delay", envOrDefault("GPU_RESUME_DELAY", "30s"), "How long contention must be absent before resuming")

	flag.Parse()

	// Parse durations after flag.Parse
	var err error
	cfg.GPUMonitorInterval, err = time.ParseDuration(gpuInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid --gpu-monitor-interval %q: %w", gpuInterval, err)
	}
	cfg.GPUResumeDelay, err = time.ParseDuration(gpuResume)
	if err != nil {
		return nil, fmt.Errorf("invalid --gpu-resume-delay %q: %w", gpuResume, err)
	}

	// Validate required fields
	// Check for a saved manager-url file (from setup mode)
	if cfg.ManagerURL == "" {
		if saved, err := os.ReadFile(cfg.DataDir + "/manager-url"); err == nil {
			url := strings.TrimSpace(string(saved))
			if url != "" {
				cfg.ManagerURL = url
			}
		}
	}

	if cfg.ManagerURL == "" {
		cfg.Unconfigured = true
		// In unconfigured mode, relax other requirements
		return cfg, nil
	}
	if cfg.WorkerKey == "" {
		return nil, fmt.Errorf("--worker-key or CLUSTER_WORKER_KEY is required")
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
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return defaultVal
	}
	return n
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".synergia/worker"
	}
	return home + "/.synergia/worker"
}
