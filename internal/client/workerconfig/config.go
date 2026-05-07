package workerconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog/log"
)

// Config holds worker configuration preferences that are synced with the manager.
type Config struct {
	PreferredRole string `json:"preferred_role"`
	Nickname      string `json:"nickname"`
}

// Manager handles configuration persistence and sync with the cluster manager.
type Manager struct {
	mu          sync.RWMutex
	config      Config
	filePath    string
	managerURL  string
	workerKey   string
	fingerprint string
}

// New creates a config manager.
func New(dataDir, managerBaseURL, workerKey, fingerprint string) *Manager {
	m := &Manager{
		filePath:    filepath.Join(dataDir, "config.json"),
		managerURL:  managerBaseURL,
		workerKey:   workerKey,
		fingerprint: fingerprint,
	}
	m.load()
	return m
}

// Get returns the current config.
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// Update sets the config and syncs with the manager.
func (m *Manager) Update(cfg Config) error {
	m.mu.Lock()
	m.config = cfg
	m.save()
	m.mu.Unlock()

	return m.SyncWithManager()
}

// SyncWithManager pushes the local config to the cluster manager.
func (m *Manager) SyncWithManager() error {
	m.mu.RLock()
	cfg := m.config
	m.mu.RUnlock()

	payload := map[string]any{
		"fingerprint":    m.fingerprint,
		"preferred_role": cfg.PreferredRole,
		"nickname":       cfg.Nickname,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, m.managerURL+"/v1/worker-config", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.workerKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("failed to sync config with manager")
		return fmt.Errorf("sync config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %d: %s", resp.StatusCode, string(respBody))
	}

	log.Debug().Msg("worker config synced with manager")
	return nil
}

func (m *Manager) load() {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &m.config)
}

func (m *Manager) save() {
	data, _ := json.MarshalIndent(m.config, "", "  ")
	dir := filepath.Dir(m.filePath)
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(m.filePath, data, 0600)
}

// RoleInfo describes a cluster role and whether the worker is eligible for it.
type RoleInfo struct {
	Role         string `json:"role"`
	Model        string `json:"model"`
	Quantisation string `json:"quantisation"`
	MinVRAMMB    int    `json:"min_vram_mb"`
	Description  string `json:"description"`
	Eligible     bool   `json:"eligible"`
}

// FetchEligibleRoles queries the manager for available roles and their eligibility.
func (m *Manager) FetchEligibleRoles() ([]RoleInfo, error) {
	url := m.managerURL + "/v1/roles?fingerprint=" + m.fingerprint
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.workerKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch roles: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("manager returned %d: %s", resp.StatusCode, string(body))
	}

	var roles []RoleInfo
	if err := json.NewDecoder(resp.Body).Decode(&roles); err != nil {
		return nil, fmt.Errorf("decode roles: %w", err)
	}
	return roles, nil
}
