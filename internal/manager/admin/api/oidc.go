package adminapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

const secretMask = "••••••••"

// AdminOIDCAPI manages OIDC / SSO configuration stored in the database.
type AdminOIDCAPI struct {
	store *store.Store
}

func NewAdminOIDCAPI(s *store.Store) *AdminOIDCAPI {
	return &AdminOIDCAPI{store: s}
}

type oidcConfigPayload struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	ProviderURL  string `json:"provider_url"`
	RedirectURL  string `json:"redirect_url"`
}

type oidcConfigResponse struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	ProviderURL  string `json:"provider_url"`
	RedirectURL  string `json:"redirect_url"`
	IsConfigured bool   `json:"is_configured"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

func (a *AdminOIDCAPI) ConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getConfig(w, r)
	case http.MethodPut:
		a.setConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *AdminOIDCAPI) getConfig(w http.ResponseWriter, _ *http.Request) {
	cfg, err := a.store.GetOIDCConfig()
	if err != nil {
		log.Error().Err(err).Msg("failed to load OIDC config")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := oidcConfigResponse{}
	if cfg != nil {
		resp.IsConfigured = true
		resp.Enabled = cfg.Enabled
		resp.ClientID = cfg.ClientID
		if cfg.ClientSecret != "" {
			resp.ClientSecret = secretMask
		}
		resp.ProviderURL = cfg.ProviderURL
		resp.RedirectURL = cfg.RedirectURL
		resp.UpdatedAt = cfg.UpdatedAt.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (a *AdminOIDCAPI) setConfig(w http.ResponseWriter, r *http.Request) {
	var payload oidcConfigPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Masked placeholder means "keep existing secret"
	secret := payload.ClientSecret
	if secret == secretMask {
		secret = ""
	}

	if err := a.store.SetOIDCConfig(payload.Enabled, payload.ClientID, secret, payload.ProviderURL, payload.RedirectURL); err != nil {
		log.Error().Err(err).Msg("failed to save OIDC config")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Info().Bool("enabled", payload.Enabled).Str("provider", payload.ProviderURL).Msg("OIDC config updated — restart required to apply")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
