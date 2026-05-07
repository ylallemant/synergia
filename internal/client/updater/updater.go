package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/client/protocol"
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

	// Download from primary URL (GitHub)
	tmpPath, err := u.download(bu.DownloadURL)
	if err != nil {
		log.Warn().Err(err).Msg("primary download failed, trying fallback")
		// Try fallback (manager proxy)
		fallbackURL := u.buildFallbackURL(bu)
		tmpPath, err = u.download(fallbackURL)
		if err != nil {
			return false, fmt.Errorf("both primary and fallback download failed: %w", err)
		}
	}
	defer os.Remove(tmpPath) // cleanup on failure

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

	// Replace the running binary
	execPath, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("failed to determine executable path: %w", err)
	}

	if err := selfReplace(execPath, tmpPath); err != nil {
		return false, fmt.Errorf("self-replace failed: %w", err)
	}

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

func (u *Updater) buildFallbackURL(bu *protocol.BinaryUpdate) string {
	base := u.managerHTTPURL
	return fmt.Sprintf("%s/v1/binary/download?version=%s&os=%s&arch=%s",
		base, bu.Version, runtime.GOOS, runtime.GOARCH)
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
