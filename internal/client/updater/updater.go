package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/protocol"
	"github.com/ylallemant/synergia/internal/client/version"
)

// Updater downloads and installs client binary updates.
type Updater struct {
	workerKey      string
	managerHTTPURL string
}

// New creates a new Updater.
func New(workerKey, managerHTTPURL string) *Updater {
	return &Updater{
		workerKey:      workerKey,
		managerHTTPURL: managerHTTPURL,
	}
}

// Apply downloads the binary, verifies it, and replaces the current executable.
// Returns true if the binary was replaced (caller should restart).
func (u *Updater) Apply(bu *protocol.BinaryUpdate) (bool, error) {
	if bu.Version == version.Version {
		log.Info().Str("version", bu.Version).Msg("binary already at target version, skipping update")
		return false, nil
	}

	log.Info().
		Str("current", version.Version).
		Str("target", bu.Version).
		Msg("binary update received — starting download")

	// On Windows, ensure the synergia-updater.exe sidecar is present before
	// downloading the new binary — it is the only mechanism that can swap a
	// locked executable. Fetched ahead of time so we fail fast (and don't waste
	// the binary download) if the sidecar is unreachable. No-op on macOS/Linux.
	execPath, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("failed to determine executable path: %w", err)
	}
	if err := u.ensureUpdater(filepath.Dir(execPath), bu.Version, runtime.GOARCH); err != nil {
		return false, fmt.Errorf("ensure updater sidecar: %w", err)
	}

	// Download from the upstream URL (GitHub) first — it is faster and doesn't
	// require the manager to be reachable. The manager URL is restored from the
	// persisted state file on restart, so sentinel patching is no longer needed
	// for binary updates.
	// Fall back to the manager's /download endpoint if the upstream URL fails
	// (air-gapped workers, GitHub unavailable, etc.).
	tmpPath, err := u.download(bu.DownloadURL)
	if err != nil {
		log.Warn().Err(err).Msg("upstream download failed, trying manager fallback")
		tmpPath, err = u.download(u.buildManagerDownloadURL(bu))
		if err != nil {
			return false, fmt.Errorf("both upstream and manager download failed: %w", err)
		}
	}
	// Cleanup tmpPath on every error path below. On the success path the
	// download is consumed by selfReplace — either renamed into place on
	// Unix (so removing it is a no-op) or handed off to synergia-updater
	// on Windows (which will rename it from this same path AFTER our
	// process exits). The previous unconditional `defer os.Remove` raced
	// with the Windows sidecar and deleted the file before it could be
	// installed, leaving the on-disk binary unchanged.
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			os.Remove(tmpPath)
		}
	}()

	// Verify SHA256 if provided
	if bu.SHA256 != "" {
		hash, err := hashFile(tmpPath)
		if err != nil {
			return false, fmt.Errorf("failed to hash downloaded binary: %w", err)
		}
		if hash != bu.SHA256 {
			return false, fmt.Errorf("SHA256 mismatch: expected %s, got %s", bu.SHA256, hash)
		}
		log.Info().Msg("SHA256 verified")
	}

	if err := selfReplace(execPath, tmpPath); err != nil {
		return false, fmt.Errorf("self-replace failed: %w", err)
	}
	// selfReplace owns tmpPath from here on — don't clean it up.
	cleanupTmp = false

	log.Info().Str("version", bu.Version).Msg("binary updated successfully — restart required")
	return true, nil
}

func (u *Updater) download(url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Minute}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+u.workerKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	// Write to temp file next to the executable
	tmp, err := os.CreateTemp("", "synergia-update-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write temp file: %w", err)
	}
	tmp.Close()

	// Make executable on Unix
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmp.Name(), 0755); err != nil {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("chmod temp file: %w", err)
		}
	}

	return tmp.Name(), nil
}

// buildManagerDownloadURL returns the manager's patched binary endpoint.
// The manager patches the sentinel (WSS URL) before serving, ensuring the
// updated client can reconnect without manual reconfiguration.
func (u *Updater) buildManagerDownloadURL(bu *protocol.BinaryUpdate) string {
	return fmt.Sprintf("%s/download/%s/%s?version=%s",
		u.managerHTTPURL, runtime.GOOS, runtime.GOARCH, bu.Version)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
