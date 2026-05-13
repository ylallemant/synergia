package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/binary"
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

	data := struct{ ClientVersion string }{
		ClientVersion: d.cache.GetStats().VersionTarget,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := downloadTmpl.Execute(w, data); err != nil {
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

	// Optional ?version= override — used by the updater to request a specific
	// release rather than the currently configured target version.
	versionOverride := r.URL.Query().Get("version")

	data, err := d.loadBinary(filename, targetOS, targetArch, versionOverride)
	if err != nil {
		log.Warn().Err(err).Str("filename", filename).Msg("binary not available")
		http.Error(w, "binary not available for this platform", http.StatusNotFound)
		return
	}

	// Build the manager WSS URL from the request.
	// When the manager runs behind a TLS-terminating reverse proxy, r.TLS is
	// nil even though the client is using HTTPS. Trust X-Forwarded-Proto when
	// present; only fall back to ws:// if neither TLS nor the header is set.
	scheme := "wss"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
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

	// ?format=app (darwin only): wrap the patched binary in a .app bundle zip
	// so the user gets Synergia.app after extraction — no Terminal window on launch.
	if r.URL.Query().Get("format") == "app" && targetOS == "darwin" {
		version := d.cache.GetStats().VersionTarget
		zipData, err := buildAppBundleZip(patched, version)
		if err != nil {
			log.Error().Err(err).Msg("failed to build app bundle zip")
			http.Error(w, "app bundle creation failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="Synergia.zip"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipData)))
		w.Write(zipData)
		return
	}

	// Serve the patched binary
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(patched)))
	w.Write(patched)
}

// buildAppBundleZip wraps a patched darwin binary in a minimal .app bundle
// inside a zip archive. macOS auto-extracts zips on download; the result is
// Synergia.app which launches with LSUIElement=true (tray only, no terminal).
func buildAppBundleZip(binary []byte, version string) ([]byte, error) {
	if version == "" {
		version = "dev"
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key>          <string>synergia-client</string>
  <key>CFBundleIdentifier</key>          <string>net.synergia.client</string>
  <key>CFBundleName</key>                <string>Synergia</string>
  <key>CFBundleShortVersionString</key>  <string>%s</string>
  <key>CFBundlePackageType</key>         <string>APPL</string>
  <key>LSUIElement</key>                 <true/>
</dict>
</plist>
`, version)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// Binary — must be executable (0755)
	bh := &zip.FileHeader{Name: "Synergia.app/Contents/MacOS/synergia-client", Method: zip.Deflate}
	bh.SetMode(0755)
	bw, err := zw.CreateHeader(bh)
	if err != nil {
		return nil, err
	}
	if _, err := bw.Write(binary); err != nil {
		return nil, err
	}

	// Info.plist
	pw, err := zw.Create("Synergia.app/Contents/Info.plist")
	if err != nil {
		return nil, err
	}
	if _, err := pw.Write([]byte(plist)); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
// For darwin/arm64 Mach-O binaries it also recomputes the ad-hoc code signature so
// macOS does not SIGKILL the binary on first execution.
func patchSentinel(data []byte, managerURL string) ([]byte, error) {
	sentinelText := []byte(sentinelValue)

	idx := bytes.Index(data, sentinelText)
	if idx == -1 {
		return nil, fmt.Errorf("sentinel not found in binary")
	}

	if idx+sentinelSize > len(data) {
		return nil, fmt.Errorf("sentinel found too close to end of binary")
	}

	if len(managerURL) >= sentinelSize {
		return nil, fmt.Errorf("manager URL too long (%d bytes, max %d)", len(managerURL), sentinelSize-1)
	}
	replacement := make([]byte, sentinelSize)
	copy(replacement, []byte(managerURL))

	result := make([]byte, len(data))
	copy(result, data)
	copy(result[idx:idx+sentinelSize], replacement)

	if bytes.Contains(result[idx+sentinelSize:], sentinelText) {
		return nil, fmt.Errorf("multiple sentinels found in binary")
	}

	// Recompute the Mach-O ad-hoc code signature so patched darwin/arm64
	// binaries pass macOS signature verification and are not SIGKILL-ed.
	recomputeAdHocSignature(result)

	return result, nil
}

// recomputeAdHocSignature updates the page hashes in a Mach-O ad-hoc code
// signature after the binary has been modified. Go embeds a code signature
// that SHA-256 hashes every 4 KB page from offset 0 to the start of
// __LINKEDIT; patching any byte in that range invalidates the signature.
// This function recomputes only the affected hashes in-place — the structure
// size is unchanged so the result fits in the original __LINKEDIT slot.
func recomputeAdHocSignature(data []byte) {
	if len(data) < 32 {
		return
	}
	// Only 64-bit little-endian Mach-O (darwin arm64 / amd64)
	if binary.LittleEndian.Uint32(data[0:]) != 0xfeedfacf {
		return
	}

	ncmds := binary.LittleEndian.Uint32(data[16:])
	off := uint32(32) // sizeof(mach_header_64)
	var csOff, csSize uint32
	for range ncmds {
		cmd := binary.LittleEndian.Uint32(data[off:])
		cmdsize := binary.LittleEndian.Uint32(data[off+4:])
		if cmd == 0x1d { // LC_CODE_SIGNATURE
			csOff = binary.LittleEndian.Uint32(data[off+8:])
			csSize = binary.LittleEndian.Uint32(data[off+12:])
		}
		off += cmdsize
	}
	if csOff == 0 || int(csOff+csSize) > len(data) {
		return
	}

	cs := data[csOff : csOff+csSize]
	if binary.BigEndian.Uint32(cs[0:]) != 0xfade0cc0 { // CS_MAGIC_EMBEDDED_SIGNATURE
		return
	}
	count := binary.BigEndian.Uint32(cs[8:])

	// Find the CodeDirectory blob (blob type 0)
	var cdOff uint32
	for i := range count {
		btype := binary.BigEndian.Uint32(cs[12+i*8:])
		boff := binary.BigEndian.Uint32(cs[16+i*8:])
		if btype == 0 {
			cdOff = boff
			break
		}
	}
	if cdOff == 0 || int(cdOff+44) > len(cs) {
		return
	}

	// CS_CodeDirectory offsets (all big-endian):
	//   16: hashOffset  (uint32) — byte offset from CD start to hash slot 0
	//   28: nCodeSlots  (uint32)
	//   32: codeLimit   (uint32) — last byte covered by page hashes
	//   36: hashSize    (uint8)
	//   39: pageSize    (uint8)  — log2(page size in bytes)
	cd := cs[cdOff:]
	hashOffset := binary.BigEndian.Uint32(cd[16:])
	nCode := binary.BigEndian.Uint32(cd[28:])
	codeLimit := binary.BigEndian.Uint32(cd[32:])
	hashSize := uint32(cd[36])
	pageSize := uint32(1) << cd[39]

	if hashSize == 0 || pageSize == 0 || nCode == 0 {
		return
	}

	for i := range nCode {
		start := i * pageSize
		end := min(start+pageSize, codeLimit)
		if int(end) > len(data) {
			break
		}
		h := sha256.Sum256(data[start:end])
		slotBase := cdOff + hashOffset + i*hashSize
		if int(slotBase)+int(hashSize) > len(cs) {
			break
		}
		copy(cs[slotBase:slotBase+hashSize], h[:hashSize])
	}
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
// loadBinary fetches the client binary for the given platform and version.
// versionOverride pins a specific release; if empty the configured target version
// (or latest available) is used.
func (d *DownloadAPI) loadBinary(filename, targetOS, targetArch, versionOverride string) ([]byte, error) {
	// 1. Operator-supplied prebuilt binaries (no version check)
	localPath := filepath.Join(d.binaryDir, filename)
	if data, err := os.ReadFile(localPath); err == nil {
		return data, nil
	}

	// 2. Determine target version before checking the cache
	version := versionOverride
	if version == "" {
		version = d.cache.GetStats().VersionTarget
	}
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
	url := buildDownloadURL(version, targetOS, targetArch, "client")
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
