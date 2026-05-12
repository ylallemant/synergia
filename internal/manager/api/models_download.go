package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/models"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// ModelsDownloadAPI handles model listing and download for workers.
type ModelsDownloadAPI struct {
	workerKeyFn func() string
	modelStore  models.Store
	dbStore     *store.Store

	// cachingMu guards the caching map so concurrent requests for the same
	// file start only one background download.
	cachingMu sync.Mutex
	caching   map[string]struct{} // filename → download in progress
}

func NewModelsDownloadAPI(workerKeyFn func() string, ms models.Store, db *store.Store) *ModelsDownloadAPI {
	return &ModelsDownloadAPI{
		workerKeyFn: workerKeyFn,
		modelStore:  ms,
		dbStore:     db,
		caching:     make(map[string]struct{}),
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

// fetchAndCacheModel starts a background download for the given filename if a
// DownloadURL is configured for the matching role and no download is already
// in progress. It returns false immediately — the caller should return 404 and
// let the client retry; subsequent requests will be served once caching completes.
func (m *ModelsDownloadAPI) fetchAndCacheModel(_ context.Context, filename string) bool {
	roles, err := m.dbStore.GetRoleModels()
	if err != nil {
		return false
	}
	base := filepath.Base(filename)
	for _, rm := range roles {
		if filepath.Base(rm.ModelFilename) != base || rm.DownloadURL == "" {
			continue
		}
		m.cachingMu.Lock()
		_, alreadyRunning := m.caching[base]
		if !alreadyRunning {
			m.caching[base] = struct{}{}
		}
		m.cachingMu.Unlock()

		if alreadyRunning {
			log.Debug().Str("filename", base).Msg("model download already in progress — client should retry")
			return false
		}
		go m.downloadModelFile(rm.Role, base, rm.DownloadURL)
		return false // client gets 404 now; file will be ready on retry
	}
	return false
}

// downloadModelFile fetches filename from downloadURL, stores it in the model
// store, and updates the role's ModelFileHash. It is safe to call concurrently;
// the caching map prevents duplicate downloads for the same file.
func (m *ModelsDownloadAPI) downloadModelFile(role, filename, downloadURL string) {
	defer func() {
		m.cachingMu.Lock()
		delete(m.caching, filename)
		m.cachingMu.Unlock()
	}()

	log.Info().Str("role", role).Str("filename", filename).Str("url", downloadURL).
		Msg("model cache: starting background download")

	resp, err := http.Get(downloadURL) //nolint:gosec
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		log.Warn().Str("url", downloadURL).Str("filename", filename).Msg("model cache: download request failed")
		return
	}
	defer resp.Body.Close()

	// Use Background context so a client request timeout never aborts the save.
	hash, err := m.modelStore.Save(context.Background(), filename, resp.Body)
	if err != nil {
		log.Warn().Err(err).Str("filename", filename).Msg("model cache: save failed")
		return
	}
	_ = m.dbStore.SetRoleModelFileHash(role, hash)
	log.Info().Str("role", role).Str("filename", filename).Str("hash", hash[:16]+"...").
		Msg("model cache: download complete — file now available to workers")
}

// EnsureModelCache is called at startup. For each configured role it:
//   - computes and stores the file hash if the file exists but has no hash
//   - starts a background download if the file is missing and a DownloadURL is set
func (m *ModelsDownloadAPI) EnsureModelCache() {
	roles, err := m.dbStore.GetRoleModels()
	if err != nil {
		log.Warn().Err(err).Msg("model cache: could not load roles")
		return
	}
	for _, rm := range roles {
		if rm.ModelFilename == "" {
			continue
		}
		if rm.ModelFileHash == "" {
			// File might exist in store already — try to hash it.
			if hash, hashErr := m.modelStore.FileHash(context.Background(), rm.ModelFilename); hashErr == nil {
				_ = m.dbStore.SetRoleModelFileHash(rm.Role, hash)
				log.Info().Str("role", rm.Role).Str("hash", hash[:16]+"...").Msg("model cache: file hash computed and stored")
				continue
			}
			// File missing — trigger download if a URL is configured.
			if rm.DownloadURL != "" {
				m.cachingMu.Lock()
				_, running := m.caching[rm.ModelFilename]
				if !running {
					m.caching[rm.ModelFilename] = struct{}{}
				}
				m.cachingMu.Unlock()
				if !running {
					go m.downloadModelFile(rm.Role, rm.ModelFilename, rm.DownloadURL)
				}
			} else {
				log.Warn().Str("role", rm.Role).Str("filename", rm.ModelFilename).
					Msg("model cache: file missing and no download URL configured — set one in Inference → Role Mappings")
			}
		}
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
