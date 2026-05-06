package consent

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
	"github.com/ylallemant/synergia/internal/client/hwinfo"
)

// State represents the local consent record.
type State struct {
	Accepted          bool `json:"accepted"`
	HardwareStats     bool `json:"hardware_stats"`
	ConfigPreferences bool `json:"config_preferences"`
}

// Manager handles consent state persistence and sync with the cluster manager.
type Manager struct {
	mu          sync.RWMutex
	state       State
	filePath    string
	managerURL  string // HTTP base URL (e.g., "http://localhost:7500")
	workerKey   string
	fingerprint string
	hwInfo      hwinfo.Info
}

// New creates a consent manager. If autoApprove is true, consent is granted immediately.
func New(dataDir, managerBaseURL, workerKey, fingerprint string, autoApprove bool) *Manager {
	m := &Manager{
		filePath:    filepath.Join(dataDir, "consent.json"),
		managerURL:  managerBaseURL,
		workerKey:   workerKey,
		fingerprint: fingerprint,
		hwInfo:      hwinfo.Collect(),
	}

	// Load existing state from disk
	m.load()

	if autoApprove && !m.state.Accepted {
		log.Info().Msg("auto-approve enabled — accepting data collection terms")
		m.state = State{
			Accepted:          true,
			HardwareStats:     true,
			ConfigPreferences: true,
		}
		m.save()
	}

	return m
}

// IsAccepted returns whether the user has accepted the data collection terms.
func (m *Manager) IsAccepted() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.Accepted
}

// SetGPUDriverInfo updates the GPU driver name and version in the hardware info
// that gets synced to the cluster manager. Call this after the GPU prober is initialized.
func (m *Manager) SetGPUDriverInfo(name, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hwInfo.GPUDriver = name
	m.hwInfo.GPUDriverVersion = version
}

// GetState returns the current consent state.
func (m *Manager) GetState() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Accept sets consent to accepted and syncs with the manager.
func (m *Manager) Accept(hardwareStats, configPreferences bool) error {
	m.mu.Lock()
	m.state = State{
		Accepted:          true,
		HardwareStats:     hardwareStats,
		ConfigPreferences: configPreferences,
	}
	m.save()
	m.mu.Unlock()

	return m.SyncWithManager()
}

// Revoke removes consent and syncs with the manager.
func (m *Manager) Revoke() error {
	m.mu.Lock()
	m.state = State{
		Accepted:          false,
		HardwareStats:     false,
		ConfigPreferences: false,
	}
	m.save()
	m.mu.Unlock()

	return m.SyncWithManager()
}

// SyncWithManager pushes the local consent state to the cluster manager.
func (m *Manager) SyncWithManager() error {
	m.mu.RLock()
	state := m.state
	m.mu.RUnlock()

	payload := map[string]any{
		"fingerprint":        m.fingerprint,
		"accepted":           state.Accepted,
		"hardware_stats":     state.HardwareStats,
		"config_preferences": state.ConfigPreferences,
		"hardware": map[string]any{
			"os":                 m.hwInfo.OS,
			"os_version":         m.hwInfo.OSVer,
			"gpu":                m.hwInfo.GPU,
			"gpu_driver":         m.hwInfo.GPUDriver,
			"gpu_driver_version": m.hwInfo.GPUDriverVersion,
			"vram_mb":            m.hwInfo.VRAMMB,
			"cpu":                m.hwInfo.CPU,
			"cpu_cores":          m.hwInfo.CPUCores,
			"ram_mb":             m.hwInfo.RAMMB,
		},
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, m.managerURL+"/v1/consent", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.workerKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("failed to sync consent with manager")
		return fmt.Errorf("sync consent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("manager returned %d: %s", resp.StatusCode, string(respBody))
	}

	log.Debug().Bool("accepted", state.Accepted).Msg("consent synced with manager")
	return nil
}

func (m *Manager) load() {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		return // no file yet = not accepted
	}
	_ = json.Unmarshal(data, &m.state)
}

func (m *Manager) save() {
	data, _ := json.MarshalIndent(m.state, "", "  ")
	dir := filepath.Dir(m.filePath)
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(m.filePath, data, 0600)
}
