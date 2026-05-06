package branding

import (
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Manager handles fetching and caching of branding CSS from the cluster manager.
type Manager struct {
	dataDir    string
	managerURL string
	workerKey  string

	mu     sync.RWMutex
	css    []byte
	stopCh chan struct{}
}

func New(dataDir, managerURL, workerKey string) *Manager {
	m := &Manager{
		dataDir:    dataDir,
		managerURL: managerURL,
		workerKey:  workerKey,
	}
	// Load cached CSS from disk (best effort)
	if data, err := os.ReadFile(m.cssPath()); err == nil {
		m.css = data
	}
	return m
}

func (m *Manager) cssPath() string {
	return filepath.Join(m.dataDir, "branding.css")
}

// FetchFromManager downloads the latest CSS from the cluster manager.
// Falls back to the local cache if the fetch fails.
func (m *Manager) FetchFromManager() {
	req, err := http.NewRequest(http.MethodGet, m.managerURL+"/v1/branding/style.css", nil)
	if err != nil {
		log.Warn().Err(err).Msg("branding: failed to create request")
		return
	}
	req.Header.Set("Authorization", "Bearer "+m.workerKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn().Err(err).Msg("branding: failed to fetch CSS from manager")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Msg("branding: manager returned non-200")
		return
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		log.Warn().Err(err).Msg("branding: failed to read response")
		return
	}

	m.mu.Lock()
	m.css = data
	m.mu.Unlock()

	// Persist to disk
	if err := os.WriteFile(m.cssPath(), data, 0644); err != nil {
		log.Warn().Err(err).Msg("branding: failed to cache CSS to disk")
	}

	log.Debug().Int("size_bytes", len(data)).Msg("branding CSS fetched from manager")
}

// StartPeriodicRefresh starts a background goroutine that re-fetches CSS
// at randomized intervals between 30 and 90 minutes.
func (m *Manager) StartPeriodicRefresh() {
	m.stopCh = make(chan struct{})
	go func() {
		for {
			wait := 30*time.Minute + time.Duration(rand.Int63n(int64(60*time.Minute)))
			log.Debug().Dur("next_refresh", wait).Msg("branding: scheduled next CSS refresh")
			select {
			case <-time.After(wait):
				m.FetchFromManager()
			case <-m.stopCh:
				return
			}
		}
	}()
}

// Stop shuts down the periodic refresh goroutine.
func (m *Manager) Stop() {
	if m.stopCh != nil {
		close(m.stopCh)
	}
}

// CSS returns the current cached CSS content.
func (m *Manager) CSS() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.css
}
