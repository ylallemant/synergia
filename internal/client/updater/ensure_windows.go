//go:build windows

package updater

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
)

// ensureUpdater makes sure synergia-updater.exe is present next to the
// running synergia-client.exe. If missing, it is downloaded from the same
// GitHub release as the target client version, with a manager-proxy fallback
// for clients behind firewalls that block github.com directly.
func (u *Updater) ensureUpdater(execDir, targetVersion, arch string) error {
	updaterPath := filepath.Join(execDir, "synergia-updater.exe")
	if _, err := os.Stat(updaterPath); err == nil {
		return nil
	}

	if targetVersion == "" {
		return fmt.Errorf("cannot download synergia-updater: target version unknown")
	}

	primary := fmt.Sprintf(
		"https://github.com/ylallemant/synergia/releases/download/%s/synergia-updater-windows-%s.exe",
		targetVersion, arch)
	fallback := fmt.Sprintf(
		"%s/v1/binary/download?version=%s&os=windows&arch=%s&kind=updater",
		u.managerHTTPURL, targetVersion, arch)

	log.Info().
		Str("path", updaterPath).
		Str("version", targetVersion).
		Msg("synergia-updater.exe missing — downloading sidecar")

	tmpPath := updaterPath + ".tmp"
	if err := u.downloadFile(primary, tmpPath); err != nil {
		log.Warn().Err(err).Msg("upstream synergia-updater download failed, trying manager fallback")
		if err := u.downloadFile(fallback, tmpPath); err != nil {
			return fmt.Errorf("download synergia-updater (upstream and manager failed): %w", err)
		}
	}

	if err := os.Rename(tmpPath, updaterPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("install synergia-updater: %w", err)
	}

	log.Info().
		Str("path", updaterPath).
		Str("version", targetVersion).
		Msg("synergia-updater.exe installed")
	return nil
}

func (u *Updater) downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if u.workerKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.workerKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	out.Close()
	return nil
}
