package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/backend"
	"github.com/ylallemant/synergia/internal/manager/cache"
	"github.com/ylallemant/synergia/internal/manager/gateway"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// BackendAPI handles backend (llama-server) version management and cached proxy download.
type BackendAPI struct {
	apiKey    string
	workerKey string
	store     *store.Store
	gateway   *gateway.Gateway
	cache     *cache.Cache
	cacheDir  string

	mu       sync.RWMutex
	cacheMap map[string]string // sha256 → cached file path
}

func NewBackendAPI(apiKey, workerKey string, s *store.Store, gw *gateway.Gateway, cacheDir string, c *cache.Cache) *BackendAPI {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		log.Warn().Err(err).Str("dir", cacheDir).Msg("failed to create backend cache dir")
	}

	b := &BackendAPI{
		apiKey:    apiKey,
		workerKey: workerKey,
		store:     s,
		gateway:   gw,
		cache:     c,
		cacheDir:  cacheDir,
		cacheMap:  make(map[string]string),
	}

	// Scan existing cached files
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if !e.IsDir() {
			b.cacheMap[e.Name()] = filepath.Join(cacheDir, e.Name())
		}
	}

	return b
}

type backendConfigRequest struct {
	Name        string `json:"name"` // backend name (e.g. "llama.cpp")
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"` // URL with {version}, {os}, {arch} placeholders (optional — resolved from name)
	SHA256      string `json:"sha256"`
}

type backendConfigResponse struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
}

// AdminBackendHandler handles GET/POST /v1/admin/backend.
func (b *BackendAPI) AdminBackendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+b.apiKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		b.getBackend(w, r)
	case http.MethodPost:
		b.setBackend(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (b *BackendAPI) getBackend(w http.ResponseWriter, _ *http.Request) {
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

func (b *BackendAPI) setBackend(w http.ResponseWriter, r *http.Request) {
	var req backendConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Version == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}

	// Default to llama.cpp if name not provided
	if req.Name == "" {
		req.Name = backend.LlamaCpp
	}
	if !backend.IsValid(req.Name) {
		http.Error(w, "unknown backend name: "+req.Name, http.StatusBadRequest)
		return
	}

	// Resolve download URL from backend name if not explicitly provided
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

	// Push backend update to connected worker
	if b.gateway != nil && b.gateway.HasWorker() {
		info := b.gateway.WorkerStatus()
		if info != nil {
			downloadURL := expandBackendURL(req.DownloadURL, req.Version, info.OS, info.Arch)
			fallbackURL := fmt.Sprintf("/v1/backend/download?version=%s&os=%s&arch=%s", req.Version, info.OS, info.Arch)
			if err := b.gateway.PushBackendUpdate(req.Version, downloadURL, fallbackURL, req.SHA256); err != nil {
				log.Error().Err(err).Msg("failed to push backend update")
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// BackendDownloadHandler serves the cached backend binary to workers.
// GET /v1/backend/download?version=b5170&os=darwin&arch=arm64
func (b *BackendAPI) BackendDownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate with worker key
	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+b.workerKey && auth != "Bearer "+b.apiKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	version := r.URL.Query().Get("version")
	osParam := r.URL.Query().Get("os")
	archParam := r.URL.Query().Get("arch")

	if version == "" || osParam == "" || archParam == "" {
		http.Error(w, "version, os, and arch query params required", http.StatusBadRequest)
		return
	}

	cfg, err := b.store.GetBackendVersionConfig()
	if err != nil {
		http.Error(w, "no backend config set", http.StatusNotFound)
		return
	}

	if cfg.Version != version {
		http.Error(w, "requested version does not match configured target", http.StatusNotFound)
		return
	}

	// Check cache first
	cacheKey := fmt.Sprintf("%s-%s-%s", version, osParam, archParam)
	b.mu.RLock()
	cachedPath, cached := b.cacheMap[cacheKey]
	b.mu.RUnlock()

	if cached {
		b.serveFile(w, cachedPath)
		return
	}

	// Download from upstream and cache
	downloadURL := expandBackendURL(cfg.DownloadURL, version, osParam, archParam)
	cachedPath, err = b.fetchAndCache(downloadURL, cacheKey, cfg.SHA256)
	if err != nil {
		log.Error().Err(err).Str("url", downloadURL).Msg("failed to fetch backend binary")
		http.Error(w, "failed to fetch backend binary", http.StatusBadGateway)
		return
	}

	b.serveFile(w, cachedPath)
}

func (b *BackendAPI) serveFile(w http.ResponseWriter, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "cache read error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, f)
}

func (b *BackendAPI) fetchAndCache(url, cacheKey, expectedSHA256 string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// Write to temp file and compute hash
	tmpFile, err := os.CreateTemp(b.cacheDir, "backend-download-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	h := sha256.New()
	writer := io.MultiWriter(tmpFile, h)

	if _, err := io.Copy(writer, resp.Body); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write failed: %w", err)
	}
	tmpFile.Close()

	// Verify hash if provided
	actualHash := hex.EncodeToString(h.Sum(nil))
	if expectedSHA256 != "" && actualHash != expectedSHA256 {
		os.Remove(tmpPath)
		return "", fmt.Errorf("SHA256 mismatch: expected %s, got %s", expectedSHA256, actualHash)
	}

	// Move to final cache location
	finalPath := filepath.Join(b.cacheDir, cacheKey)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename to cache: %w", err)
	}

	b.mu.Lock()
	b.cacheMap[cacheKey] = finalPath
	b.mu.Unlock()

	log.Info().Str("key", cacheKey).Str("hash", actualHash[:16]+"...").Msg("backend binary cached")
	return finalPath, nil
}

// expandBackendURL replaces placeholders in the download URL template.
// Supported placeholders: {version}, {os}, {arch}, {platform}, {ext}
// {platform} maps Go runtime values to llama.cpp naming: darwin→macos, linux→ubuntu, windows→win-cpu
// {arch} maps: amd64→x64, arm64→arm64
// {ext} maps: darwin/linux→tar.gz, windows→zip
func expandBackendURL(template, version, goos, goarch string) string {
	platform := mapPlatform(goos)
	arch := mapArch(goarch)
	ext := mapExt(goos)

	url := template
	url = replaceAll(url, "{version}", version)
	url = replaceAll(url, "{os}", goos)
	url = replaceAll(url, "{platform}", platform)
	url = replaceAll(url, "{arch}", arch)
	url = replaceAll(url, "{ext}", ext)
	return url
}

// mapPlatform converts Go GOOS to llama.cpp release platform name.
func mapPlatform(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	case "linux":
		return "ubuntu"
	case "windows":
		return "win-cpu"
	default:
		return goos
	}
}

// mapArch converts Go GOARCH to llama.cpp release arch name.
func mapArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	default:
		return goarch
	}
}

// mapExt returns the archive extension for the platform.
func mapExt(goos string) string {
	if goos == "windows" {
		return "zip"
	}
	return "tar.gz"
}

// DefaultBackendDownloadURL is the standard llama.cpp release URL template.
const DefaultBackendDownloadURL = "https://github.com/ggml-org/llama.cpp/releases/download/{version}/llama-{version}-bin-{platform}-{arch}.{ext}"

// AdminBackendTagsHandler returns recent tags for the given backend.
// GET /v1/admin/backend/tags?name=llama.cpp
func (b *BackendAPI) AdminBackendTagsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+b.apiKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
func (b *BackendAPI) AdminBackendNamesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+b.apiKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"names": backend.Names,
	})
}

func replaceAll(s, old, new string) string {
	for {
		i := indexOf(s, old)
		if i < 0 {
			return s
		}
		s = s[:i] + new + s[i+len(old):]
	}
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
