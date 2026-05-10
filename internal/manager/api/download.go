package api

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/cache"
)

//go:embed public/download.html
var downloadPageFS embed.FS

//go:embed public/install.sh
var installScriptFS embed.FS

var downloadTmpl = template.Must(template.ParseFS(downloadPageFS, "public/download.html"))

// Sentinel constants for binary patching.
// The client binary contains a placeholder string of this form,
// which we replace at runtime with the actual manager URL.
const sentinelValue = "$$SYNERGIA_MANAGER_URL$$"
const sentinelSize = 256

// DownloadAPI serves the public download page, binary patching endpoint, and install script.
type DownloadAPI struct {
	binaryDir string
	cacheDir  string
	cache     *cache.Cache
	mu        sync.Mutex // protects concurrent downloads of the same binary
}

// NewDownloadAPI creates a new download handler.
// binaryDir is the path to the directory containing pre-built generic binaries.
// cacheDir is used to cache binaries fetched from GitHub releases.
// cache provides access to the latest client version tag.
func NewDownloadAPI(binaryDir, cacheDir string, c *cache.Cache) *DownloadAPI {
	return &DownloadAPI{
		binaryDir: binaryDir,
		cacheDir:  cacheDir,
		cache:     c,
	}
}

// DownloadPageHandler serves GET /download — public page with OS/arch detection.
func (d *DownloadAPI) DownloadPageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := downloadTmpl.Execute(w, nil); err != nil {
		log.Error().Err(err).Msg("failed to render download page")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// BinaryHandler serves GET /download/{os}/{arch} — patched binary download.
func (d *DownloadAPI) BinaryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse path: /download/{os}/{arch}
	path := strings.TrimPrefix(r.URL.Path, "/download/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "usage: /download/{os}/{arch}", http.StatusBadRequest)
		return
	}

	targetOS := parts[0]
	targetArch := parts[1]

	// Validate OS and arch
	validOS := map[string]bool{"darwin": true, "linux": true, "windows": true}
	validArch := map[string]bool{"amd64": true, "arm64": true}
	if !validOS[targetOS] {
		http.Error(w, "invalid OS: must be darwin, linux, or windows", http.StatusBadRequest)
		return
	}
	if !validArch[targetArch] {
		http.Error(w, "invalid arch: must be amd64 or arm64", http.StatusBadRequest)
		return
	}

	// Build filename
	filename := fmt.Sprintf("synergia-client-%s-%s", targetOS, targetArch)
	if targetOS == "windows" {
		filename += ".exe"
	}

	data, err := d.loadBinary(filename, targetOS, targetArch)
	if err != nil {
		log.Warn().Err(err).Str("filename", filename).Msg("binary not available")
		http.Error(w, "binary not available for this platform", http.StatusNotFound)
		return
	}

	// Build the manager WSS URL from the request
	scheme := "wss"
	if r.TLS == nil {
		scheme = "ws"
	}
	host := r.Host
	managerURL := fmt.Sprintf("%s://%s/ws/worker", scheme, host)

	// Patch the sentinel
	patched, err := patchSentinel(data, managerURL)
	if err != nil {
		log.Error().Err(err).Msg("failed to patch binary sentinel")
		http.Error(w, "binary patching failed", http.StatusInternalServerError)
		return
	}

	// Serve the patched binary
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(patched)))
	w.Write(patched)
}

// InstallHandler serves GET /install — platform-aware install script.
func (d *DownloadAPI) InstallHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Build base download URL from request
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)

	// Optional worker key from query param
	workerKey := r.URL.Query().Get("key")

	scriptData, _ := installScriptFS.ReadFile("public/install.sh")
	script := string(scriptData)
	script = strings.ReplaceAll(script, "{{BASE_URL}}", baseURL)
	script = strings.ReplaceAll(script, "{{WORKER_KEY}}", workerKey)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(script))
}

// patchSentinel replaces the sentinel placeholder in the binary with the actual manager URL.
func patchSentinel(data []byte, managerURL string) ([]byte, error) {
	sentinelText := []byte(sentinelValue)

	idx := bytes.Index(data, sentinelText)
	if idx == -1 {
		return nil, fmt.Errorf("sentinel not found in binary")
	}

	// Ensure there's enough room for the full sentinel region
	if idx+sentinelSize > len(data) {
		return nil, fmt.Errorf("sentinel found too close to end of binary")
	}

	// Build replacement: URL + null padding to sentinelSize
	if len(managerURL) >= sentinelSize {
		return nil, fmt.Errorf("manager URL too long (%d bytes, max %d)", len(managerURL), sentinelSize-1)
	}
	replacement := make([]byte, sentinelSize)
	copy(replacement, []byte(managerURL))

	result := make([]byte, len(data))
	copy(result, data)
	copy(result[idx:idx+sentinelSize], replacement)

	// Verify no second occurrence
	if bytes.Index(result[idx+sentinelSize:], sentinelText) != -1 {
		return nil, fmt.Errorf("multiple sentinels found in binary")
	}

	return result, nil
}

// loadBinary tries to load the binary from (in order):
// 1. Local binaryDir (pre-built binaries placed by make client-binaries)
// 2. Version-aware cache (previously fetched from GitHub for this exact version)
// 3. GitHub releases (fetched and cached for future use)
//
// The cache path includes the version so stale binaries from older releases are
// naturally bypassed when the target version advances. The startup version-change
// check in main.go removes the entire cache dir when the manager itself upgrades,
// so there is no need to manually prune old version subdirectories.
func (d *DownloadAPI) loadBinary(filename, targetOS, targetArch string) ([]byte, error) {
	// 1. Operator-supplied prebuilt binaries (no version check)
	localPath := filepath.Join(d.binaryDir, filename)
	if data, err := os.ReadFile(localPath); err == nil {
		return data, nil
	}

	// 2. Determine target version before checking the cache
	version := d.cache.GetStats().VersionTarget
	if version == "" {
		if tags := d.cache.GetClientTags(); len(tags) > 0 {
			version = tags[0]
		}
	}
	if version == "" {
		// Tags not yet in cache — fetch directly from GitHub as a last resort
		if tags, err := d.cache.RefreshClientTags(); err == nil && len(tags) > 0 {
			version = tags[0]
		}
	}
	if version == "" {
		return nil, fmt.Errorf("no version configured and GitHub release list unavailable")
	}

	// 3. Version-aware cache: each version has its own subdirectory
	cachedPath := filepath.Join(d.cacheDir, "client-binaries", version, filename)
	if data, err := os.ReadFile(cachedPath); err == nil {
		return data, nil
	}

	// 4. Fetch from GitHub and cache for future requests
	url := buildDownloadURL(version, targetOS, targetArch)
	log.Info().Str("url", url).Str("filename", filename).Msg("fetching client binary from GitHub")

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %d for %s", resp.StatusCode, url)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	versionCacheDir := filepath.Join(d.cacheDir, "client-binaries", version)
	if err := os.MkdirAll(versionCacheDir, 0o755); err == nil {
		if writeErr := os.WriteFile(cachedPath, data, 0o755); writeErr != nil {
			log.Warn().Err(writeErr).Str("path", cachedPath).Msg("failed to cache binary")
		}
	}

	return data, nil
}
