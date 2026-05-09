package api

import (
	"net/http"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// BrandingAPI serves customizable CSS to worker dashboards.
// CSS is cached in memory and only read from DB on startup or admin update.
type BrandingAPI struct {
	workerKey string
	store     *store.Store

	mu  sync.RWMutex
	css string
}

func NewBrandingAPI(workerKey string, s *store.Store) *BrandingAPI {
	b := &BrandingAPI{
		workerKey: workerKey,
		store:     s,
	}
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

// SetCSS persists new CSS to the DB and updates the in-memory cache.
func (b *BrandingAPI) SetCSS(css string) error {
	if err := b.store.SetBrandingCSS(css); err != nil {
		return err
	}
	b.mu.Lock()
	b.css = css
	b.mu.Unlock()
	return nil
}

// StyleHandler serves the branding CSS to workers.
// GET /v1/branding/style.css
func (b *BrandingAPI) StyleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if auth := r.Header.Get("Authorization"); auth != "Bearer "+b.workerKey {
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
