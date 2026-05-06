package api

import (
	"io"
	"net/http"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// BrandingAPI serves customizable CSS to worker dashboards.
// CSS is cached in memory and only read from DB on startup or admin update.
type BrandingAPI struct {
	apiKey    string
	workerKey string
	store     *store.Store

	mu  sync.RWMutex
	css string
}

func NewBrandingAPI(apiKey, workerKey string, s *store.Store) *BrandingAPI {
	b := &BrandingAPI{
		apiKey:    apiKey,
		workerKey: workerKey,
		store:     s,
	}
	// Load from DB on startup
	css, err := s.GetBrandingCSS()
	if err != nil {
		log.Warn().Err(err).Msg("failed to load branding CSS from DB")
	}
	b.css = css
	return b
}

// GetCSS returns the current cached CSS.
func (b *BrandingAPI) GetCSS() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.css
}

// StyleHandler serves the branding CSS to workers (worker-key auth).
// GET /v1/branding/style.css
func (b *BrandingAPI) StyleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate with worker key
	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+b.workerKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	b.mu.RLock()
	css := b.css
	b.mu.RUnlock()

	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(css))
}

// AdminUpdateHandler allows admins to update the branding CSS (api-key auth).
// PUT /v1/admin/branding/css
func (b *BrandingAPI) AdminUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate with admin API key
	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+b.apiKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024)) // 256KB max
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	css := string(body)
	if err := b.store.SetBrandingCSS(css); err != nil {
		log.Error().Err(err).Msg("failed to persist branding CSS")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Update in-memory cache
	b.mu.Lock()
	b.css = css
	b.mu.Unlock()

	log.Info().Int("size_bytes", len(css)).Msg("branding CSS updated")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}
