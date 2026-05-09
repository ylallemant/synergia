package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rs/zerolog/log"
	workerapi "github.com/ylallemant/synergia/internal/manager/api"
	"github.com/ylallemant/synergia/internal/manager/backend"
	"github.com/ylallemant/synergia/internal/manager/cache"
	"github.com/ylallemant/synergia/internal/manager/gateway"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// AdminBackendAPI manages backend (llama-server) version configuration.
type AdminBackendAPI struct {
	store   *store.Store
	gateway *gateway.Gateway
	cache   *cache.Cache
}

func NewAdminBackendAPI(s *store.Store, gw *gateway.Gateway, c *cache.Cache) *AdminBackendAPI {
	return &AdminBackendAPI{
		store:   s,
		gateway: gw,
		cache:   c,
	}
}

type backendConfigRequest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
}

type backendConfigResponse struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
}

// AdminBackendHandler handles GET/POST /v1/admin/backend.
func (b *AdminBackendAPI) AdminBackendHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b.getBackend(w, r)
	case http.MethodPost:
		b.setBackend(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *AdminBackendAPI) getBackend(w http.ResponseWriter, _ *http.Request) {
	cfg, err := b.store.GetBackendVersionConfig()
	if err != nil {
		http.Error(w, "no backend config set", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(backendConfigResponse{
		Name:        cfg.Name,
		Version:     cfg.Version,
		DownloadURL: cfg.DownloadURL,
		SHA256:      cfg.SHA256,
	})
}

func (b *AdminBackendAPI) setBackend(w http.ResponseWriter, r *http.Request) {
	var req backendConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Version == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		req.Name = backend.LlamaCpp
	}
	if !backend.IsValid(req.Name) {
		http.Error(w, "unknown backend name: "+req.Name, http.StatusBadRequest)
		return
	}

	if req.DownloadURL == "" {
		tpl, ok := backend.DownloadURLTemplates[req.Name]
		if !ok {
			http.Error(w, "no download URL template for backend: "+req.Name, http.StatusBadRequest)
			return
		}
		req.DownloadURL = tpl
	}

	if err := b.store.SetBackendVersionConfig(req.Name, req.Version, req.DownloadURL, req.SHA256); err != nil {
		log.Error().Err(err).Msg("failed to save backend version config")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if b.gateway != nil && b.gateway.HasWorker() {
		info := b.gateway.WorkerStatus()
		if info != nil {
			downloadURL := workerapi.ExpandBackendURL(req.DownloadURL, req.Version, info.OS, info.Arch)
			fallbackURL := fmt.Sprintf("/v1/backend/download?version=%s&os=%s&arch=%s", req.Version, info.OS, info.Arch)
			if err := b.gateway.PushBackendUpdate(req.Version, downloadURL, fallbackURL, req.SHA256); err != nil {
				log.Error().Err(err).Msg("failed to push backend update")
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// AdminBackendTagsHandler returns recent tags for the given backend.
// GET /v1/admin/backend/tags?name=llama.cpp
func (b *AdminBackendAPI) AdminBackendTagsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		name = backend.LlamaCpp
	}
	if !backend.IsValid(name) {
		http.Error(w, "unknown backend: "+name, http.StatusBadRequest)
		return
	}

	tags := b.cache.GetBackendTags(name)
	if len(tags) == 0 {
		var err error
		tags, err = b.cache.RefreshBackendTags(name)
		if err != nil {
			log.Error().Err(err).Str("backend", name).Msg("failed to fetch tags")
			http.Error(w, "failed to fetch tags: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"name": name,
		"tags": tags,
	})
}

// AdminBackendNamesHandler returns the list of supported backend names.
// GET /v1/admin/backend/names
func (b *AdminBackendAPI) AdminBackendNamesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"names": backend.Names,
	})
}
