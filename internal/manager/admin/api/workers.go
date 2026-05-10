package adminapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

const workerKeyMask = "••••••••"

// WorkerKeyUpdater lets the admin API push auth-mode changes to the live gateway
// without requiring a manager restart.
type WorkerKeyUpdater interface {
	SetWorkerKey(key string)
}

// AdminWorkersAPI manages worker authentication configuration.
type AdminWorkersAPI struct {
	store   *store.Store
	gateway WorkerKeyUpdater
}

func NewAdminWorkersAPI(s *store.Store, gw WorkerKeyUpdater) *AdminWorkersAPI {
	return &AdminWorkersAPI{store: s, gateway: gw}
}

type workerAuthPayload struct {
	TOFUEnabled bool   `json:"tofu_enabled"`
	WorkerKey   string `json:"worker_key"`
}

type workerAuthResponse struct {
	TOFUEnabled  bool   `json:"tofu_enabled"`
	WorkerKey    string `json:"worker_key"`
	IsConfigured bool   `json:"is_configured"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

func (a *AdminWorkersAPI) ConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getConfig(w, r)
	case http.MethodPut:
		a.setConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *AdminWorkersAPI) getConfig(w http.ResponseWriter, _ *http.Request) {
	cfg, err := a.store.GetWorkerAuthConfig()
	if err != nil {
		log.Error().Err(err).Msg("failed to load worker auth config")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := workerAuthResponse{}
	if cfg != nil {
		resp.IsConfigured = true
		resp.TOFUEnabled = cfg.TOFUEnabled
		if cfg.WorkerKey != "" {
			resp.WorkerKey = workerKeyMask
		}
		resp.UpdatedAt = cfg.UpdatedAt.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (a *AdminWorkersAPI) setConfig(w http.ResponseWriter, r *http.Request) {
	var payload workerAuthPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Masked placeholder means "keep existing key"
	key := payload.WorkerKey
	if key == workerKeyMask {
		key = ""
	}

	if err := a.store.SetWorkerAuthConfig(payload.TOFUEnabled, key); err != nil {
		log.Error().Err(err).Msg("failed to save worker auth config")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Apply to the live gateway immediately — no restart required.
	if payload.TOFUEnabled {
		a.gateway.SetWorkerKey("")
		log.Info().Msg("worker auth config updated — TOFU mode active")
	} else {
		a.gateway.SetWorkerKey(key)
		log.Info().Msg("worker auth config updated — key-auth mode active")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
