package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// BackendAPI serves the cached backend binary to workers.
type BackendAPI struct {
	workerKeyFn func() string
	store       *store.Store
	cacheDir    string

	mu       sync.RWMutex
	cacheMap map[string]string // version-os-arch → cached file path
}

func NewBackendAPI(workerKeyFn func() string, s *store.Store, cacheDir string) *BackendAPI {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		log.Warn().Err(err).Str("dir", cacheDir).Msg("failed to create backend cache dir")
	}

	b := &BackendAPI{
		workerKeyFn: workerKeyFn,
		store:       s,
		cacheDir:    cacheDir,
		cacheMap:    make(map[string]string),
	}

	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if !e.IsDir() {
			b.cacheMap[e.Name()] = filepath.Join(cacheDir, e.Name())
		}
	}

	return b
}

// BackendDownloadHandler serves the cached backend binary to workers.
// GET /v1/backend/download?version=b5170&os=darwin&arch=arm64
func (b *BackendAPI) BackendDownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if key := b.workerKeyFn(); key != "" && r.Header.Get("Authorization") != "Bearer "+key {
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

	cacheKey := fmt.Sprintf("%s-%s-%s", version, osParam, archParam)
	b.mu.RLock()
	cachedPath, cached := b.cacheMap[cacheKey]
	b.mu.RUnlock()

	if cached {
		b.serveFile(w, cachedPath)
		return
	}

	downloadURL := ExpandBackendURL(cfg.DownloadURL, version, osParam, archParam)
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

	actualHash := hex.EncodeToString(h.Sum(nil))
	if expectedSHA256 != "" && actualHash != expectedSHA256 {
		os.Remove(tmpPath)
		return "", fmt.Errorf("SHA256 mismatch: expected %s, got %s", expectedSHA256, actualHash)
	}

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

// ExpandBackendURL replaces placeholders in the download URL template.
// Supported placeholders: {version}, {os}, {arch}, {platform}, {ext}
func ExpandBackendURL(template, version, goos, goarch string) string {
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

// DefaultBackendDownloadURL is the standard llama.cpp release URL template.
const DefaultBackendDownloadURL = "https://github.com/ggml-org/llama.cpp/releases/download/{version}/llama-{version}-bin-{platform}-{arch}.{ext}"

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

func mapArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	default:
		return goarch
	}
}

func mapExt(goos string) string {
	if goos == "windows" {
		return "zip"
	}
	return "tar.gz"
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
