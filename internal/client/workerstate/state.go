// Package workerstate persists all runtime information the client needs to
// survive a restart without re-synchronising with the manager.
//
// State is stored as YAML at {dataDir}/worker-state.yaml for human readability.
// Writes are atomic (write-to-temp then rename) so a crash mid-write cannot
// corrupt the file.
package workerstate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

const fileName = "worker-state.yaml"

// LlamaParams mirrors the llama-server launch parameters so they can be
// restored after a client restart without waiting for a new ModelUpdate.
type LlamaParams struct {
	ContextSize    int    `yaml:"context_size"`
	ParallelSlots  int    `yaml:"parallel_slots"`
	GPULayers      int    `yaml:"gpu_layers"`
	EndpointType   string `yaml:"endpoint_type"`
	FlashAttention bool   `yaml:"flash_attention"`
}

// Persisted is the on-disk representation of the worker's durable state.
type Persisted struct {
	// ManagerURL is the WebSocket URL of the cluster manager.
	// Saved on first successful connection so subsequent starts and binary
	// updates can reconnect without re-patching the binary sentinel.
	ManagerURL string `yaml:"manager_url,omitempty"`

	// Role is the last assigned worker role (e.g. "tester", "embedding").
	Role string `yaml:"role,omitempty"`

	// ModelPath is the absolute path of the last successfully installed
	// model file. Restored at startup so llama-server can be started
	// immediately without waiting for a ModelUpdate from the manager.
	ModelPath   string      `yaml:"model_path,omitempty"`
	LlamaParams LlamaParams `yaml:"llama_params,omitempty"`

	// BackendVersion is the version tag of the installed llama-server binary.
	BackendVersion string `yaml:"backend_version,omitempty"`

	// LogLevel is the zerolog level string last set via the dashboard (e.g. "debug", "info").
	// Restored on startup so the worker keeps the same verbosity across restarts.
	LogLevel string `yaml:"log_level,omitempty"`

	UpdatedAt time.Time `yaml:"updated_at"`
}

// Store manages reads and atomic writes of the persisted state.
type Store struct {
	mu      sync.RWMutex
	path    string
	current Persisted
}

// Load reads state from {dataDir}/worker-state.yaml.
// Returns an empty Store (no error) when the file does not exist yet.
func Load(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, fileName)
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read worker state: %w", err)
	}
	// Corrupt state is treated as empty — client will re-sync from manager.
	_ = yaml.Unmarshal(data, &s.current)
	return s, nil
}

// Get returns a snapshot of the current persisted state.
func (s *Store) Get() Persisted {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// SetManagerURL saves the cluster manager WebSocket URL.
func (s *Store) SetManagerURL(url string) {
	s.mu.Lock()
	s.current.ManagerURL = url
	s.mu.Unlock()
	s.save()
}

// SetModel saves the installed model path, llama-server params, and role.
func (s *Store) SetModel(path string, params LlamaParams, role string) {
	s.mu.Lock()
	s.current.ModelPath = path
	s.current.LlamaParams = params
	s.current.Role = role
	s.mu.Unlock()
	s.save()
}

// SetBackendVersion saves the installed llama-server version tag.
func (s *Store) SetBackendVersion(version string) {
	s.mu.Lock()
	s.current.BackendVersion = version
	s.mu.Unlock()
	s.save()
}

// SetLogLevel saves the zerolog level string so it is restored on next startup.
func (s *Store) SetLogLevel(level string) {
	s.mu.Lock()
	s.current.LogLevel = level
	s.mu.Unlock()
	s.save()
}

func (s *Store) save() {
	s.mu.RLock()
	c := s.current
	s.mu.RUnlock()
	c.UpdatedAt = time.Now()
	data, err := yaml.Marshal(c)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}
