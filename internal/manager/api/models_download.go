package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/models"
)

// ModelsDownloadAPI handles model listing and download for workers.
type ModelsDownloadAPI struct {
	workerKeyFn func() string
	modelStore  models.Store
}

func NewModelsDownloadAPI(workerKeyFn func() string, store models.Store) *ModelsDownloadAPI {
	return &ModelsDownloadAPI{
		workerKeyFn: workerKeyFn,
		modelStore:  store,
	}
}

// ListModelsHandler returns available model files.
// GET /v1/models/files — authenticated with worker key.
func (m *ModelsDownloadAPI) ListModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !m.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	modelList, err := m.modelStore.List(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("failed to list models")
		writeError(w, http.StatusInternalServerError, "failed to list models")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": modelList,
		"total":  len(modelList),
	})
}

// DownloadHandler streams a model file to the requesting worker.
// GET /v1/models/download/{filename} — authenticated with worker key.
// Supports Range header for resumable downloads.
func (m *ModelsDownloadAPI) DownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !m.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Extract filename from path: /v1/models/download/{filename}
	filename := strings.TrimPrefix(r.URL.Path, "/v1/models/download/")
	if filename == "" || filename == r.URL.Path {
		writeError(w, http.StatusBadRequest, "filename required in path")
		return
	}

	log.Info().Str("filename", filename).Str("remote", r.RemoteAddr).Msg("model download requested")

	if err := m.modelStore.ServeDownload(r.Context(), filename, w, r); err != nil {
		log.Warn().Str("filename", filename).Err(err).Msg("model download failed")
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
}

func (m *ModelsDownloadAPI) authenticate(r *http.Request) bool {
	key := m.workerKeyFn()
	if key == "" {
		return true // TOFU mode — no key required
	}
	if apiKey := r.Header.Get("X-API-Key"); apiKey == key {
		return true
	}
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:] == key
	}
	return false
}
