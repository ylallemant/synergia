package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/models"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// ModelsDownloadAPI handles model listing and download for workers.
type ModelsDownloadAPI struct {
	workerKeyFn func() string
	modelStore  models.Store
	dbStore     *store.Store
}

func NewModelsDownloadAPI(workerKeyFn func() string, ms models.Store, db *store.Store) *ModelsDownloadAPI {
	return &ModelsDownloadAPI{
		workerKeyFn: workerKeyFn,
		modelStore:  ms,
		dbStore:     db,
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
		// File not in local store — check if a DownloadURL is configured for the role
		// that owns this filename so we can fetch and cache it (firewall fallback).
		if m.dbStore != nil {
			if fetched := m.fetchAndCacheModel(r.Context(), filename); fetched {
				if err2 := m.modelStore.ServeDownload(r.Context(), filename, w, r); err2 == nil {
					return
				}
			}
		}
		log.Warn().Str("filename", filename).Err(err).Msg("model download failed")
		writeError(w, http.StatusNotFound, fmt.Sprintf("model not found: %s", filepath.Base(filename)))
		return
	}
}

// fetchAndCacheModel looks up the DownloadURL for the given filename across all role models,
// downloads the file into the model store, and updates the stored file hash.
// Returns true if the file was successfully fetched and cached.
func (m *ModelsDownloadAPI) fetchAndCacheModel(ctx context.Context, filename string) bool {
	roles, err := m.dbStore.GetRoleModels()
	if err != nil {
		return false
	}
	base := filepath.Base(filename)
	for _, rm := range roles {
		if filepath.Base(rm.ModelFilename) != base || rm.DownloadURL == "" {
			continue
		}
		log.Info().Str("filename", base).Str("url", rm.DownloadURL).Str("role", rm.Role).Msg("fetching model from download URL")
		resp, err := http.Get(rm.DownloadURL) //nolint:gosec
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			log.Warn().Str("url", rm.DownloadURL).Msg("model fetch failed")
			return false
		}
		defer resp.Body.Close()
		hash, err := m.modelStore.Save(ctx, base, resp.Body)
		if err != nil {
			log.Warn().Err(err).Str("filename", base).Msg("model save failed")
			return false
		}
		_ = m.dbStore.SetRoleModelFileHash(rm.Role, hash)
		log.Info().Str("filename", base).Str("hash", hash[:16]+"...").Msg("model cached from download URL")
		return true
	}
	return false
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
