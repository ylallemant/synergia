package config

import (
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ylallemant/synergia/internal/client/workerstate"
)

// DefaultManagerURL is a 256-byte sentinel in the binary's __DATA segment.
// The manager's download endpoint patches these bytes at distribution time
// with the real WSS URL (null-padded to fill all 256 bytes).
//
// Using a [256]byte var (not a string literal) is intentional: string literals
// are placed in __TEXT,__rodata which is covered by the ad-hoc code signature
// that Go embeds in every darwin/arm64 binary. Patching __TEXT bytes invalidates
// the signature and macOS SIGKILL-s the binary before it can run. __DATA pages
// are writable and are NOT covered by the signature, so patching is safe.
var DefaultManagerURL = [256]byte{
	'$', '$', 'S', 'Y', 'N', 'E', 'R', 'G', 'I', 'A',
	'_', 'M', 'A', 'N', 'A', 'G', 'E', 'R', '_', 'U', 'R', 'L', '$', '$',
	// remaining 232 bytes are zero — the "unpatched" sentinel state
}

const defaultManagerURLSentinelSize = 256
const defaultManagerURLSentinelLen  = 24 // len("$$SYNERGIA_MANAGER_URL$$")

func defaultManagerURLSentinel() string {
	return string(DefaultManagerURL[:defaultManagerURLSentinelLen])
}

func resolveManagerURL() string {
	url := strings.TrimRight(string(DefaultManagerURL[:]), "\x00")
	if url == defaultManagerURLSentinel() || url == "" {
		return ""
	}
	return url
}

// DefaultWorkerKey is a 96-byte sentinel in __DATA, patched at distribution
// time with a Base64-encoded worker key for key-auth deployments.
// Same __DATA placement rationale as DefaultManagerURL above.
var DefaultWorkerKey = [96]byte{
	'$', '$', 'S', 'Y', 'N', 'E', 'R', 'G', 'I', 'A',
	'_', 'W', 'O', 'R', 'K', 'E', 'R', '_', 'K', 'E', 'Y', '$', '$',
	// remaining 73 bytes are zero
}

const defaultWorkerKeySentinelSize = 96
const defaultWorkerKeySentinelLen  = 23 // len("$$SYNERGIA_WORKER_KEY$$")

func defaultWorkerKeySentinel() string {
	return string(DefaultWorkerKey[:defaultWorkerKeySentinelLen])
}

// resolveWorkerKey returns the effective worker key in priority order:
// 1. CLUSTER_WORKER_KEY env var (development / CI override)
// 2. Base64-decoded binary sentinel (patched at distribution time)
// 3. Empty string — TOFU mode, challenge-response auth is used instead
func resolveWorkerKey() string {
	if v := os.Getenv("CLUSTER_WORKER_KEY"); v != "" {
		return v
	}
	raw := strings.TrimRight(string(DefaultWorkerKey[:]), "\x00")
	if raw == defaultWorkerKeySentinel() || raw == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return ""
	}
	return string(decoded)
}

type Config struct {
	ManagerURL          string
	WorkerKey           string // empty = TOFU mode; set = key-auth mode
	LLMURL              string
	Model               string
	Quantisation        string
	Role                string
	ModelFile           string
	DataDir             string
	DashboardAddr       string
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

	defaultURL := envOrDefault("CLUSTER_MANAGER_URL", resolveManagerURL())

	flag.StringVar(&cfg.ManagerURL, "manager-url", defaultURL, "WebSocket URL of the cluster manager")
	flag.StringVar(&cfg.LLMURL, "llm-url", envOrDefault("WORKER_LLM_URL", "http://localhost:9877"), "Local llama-server endpoint")
	flag.StringVar(&cfg.Model, "model", envOrDefault("WORKER_MODEL", "SmolLM2-135M-Instruct"), "Model name to report")
	flag.StringVar(&cfg.Quantisation, "quantisation", envOrDefault("WORKER_QUANTISATION", "Q4_K_M"), "Quantisation level to report")
	flag.StringVar(&cfg.Role, "role", envOrDefault("WORKER_ROLE", "tester"), "Worker role (embedding, inference, ingestion, tester)")
	flag.StringVar(&cfg.ModelFile, "model-file", os.Getenv("WORKER_MODEL_FILE"), "Path to the GGUF model file (for hash verification)")
	flag.StringVar(&cfg.DataDir, "data-dir", envOrDefault("CLUSTER_CLIENT_DATA_DIR", defaultDataDir()), "Directory for identity keystore and local state")
	flag.StringVar(&cfg.DashboardAddr, "dashboard-addr", envOrDefault("CLUSTER_DASHBOARD_ADDR", "127.0.0.1:9876"), "Listen address for the local client dashboard")
	flag.BoolVar(&cfg.AutoApprove, "auto-approve", envOrDefault("CLUSTER_CLIENT_AUTO_APPROVE", "") == "true", "Automatically accept data collection terms (for testing)")
	flag.BoolVar(&cfg.Insecure, "insecure", envOrDefault("CLUSTER_INSECURE", "") == "true", "Connect without TLS (ws:// instead of wss://)")
	flag.StringVar(&cfg.TLSCACert, "tls-ca-cert", os.Getenv("TLS_CA_CERT"), "Path to CA certificate for verifying the manager's TLS cert")

	var gpuInterval string
	flag.StringVar(&gpuInterval, "gpu-monitor-interval", envOrDefault("GPU_MONITOR_INTERVAL", "5s"), "How often to poll GPU utilization")
	flag.IntVar(&cfg.GPUContentionThresh, "gpu-contention-threshold", envOrDefaultInt("GPU_CONTENTION_THRESHOLD", 15), "Percentage above baseline that triggers idle state")
	var gpuResume string
	flag.StringVar(&gpuResume, "gpu-resume-delay", envOrDefault("GPU_RESUME_DELAY", "30s"), "How long contention must be absent before resuming")

	flag.Parse()

	var err error
	cfg.GPUMonitorInterval, err = time.ParseDuration(gpuInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid --gpu-monitor-interval %q: %w", gpuInterval, err)
	}
	cfg.GPUResumeDelay, err = time.ParseDuration(gpuResume)
	if err != nil {
		return nil, fmt.Errorf("invalid --gpu-resume-delay %q: %w", gpuResume, err)
	}

	// Resolve manager URL: saved file takes priority, then worker-state.yaml.
	if cfg.ManagerURL == "" {
		if saved, err := os.ReadFile(cfg.DataDir + "/manager-url"); err == nil {
			if url := strings.TrimSpace(string(saved)); url != "" {
				cfg.ManagerURL = url
			}
		}
	}
	if cfg.ManagerURL == "" {
		if ws, err := workerstate.Load(cfg.DataDir); err == nil {
			if saved := ws.Get().ManagerURL; saved != "" {
				cfg.ManagerURL = saved
			}
		}
	}

	if cfg.ManagerURL == "" {
		cfg.Unconfigured = true
		return cfg, nil
	}

	cfg.WorkerKey = resolveWorkerKey()

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
