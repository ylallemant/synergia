package api

import (
	"fmt"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
)

// VersionAPI proxies client binary downloads for workers that cannot reach GitHub directly.
type VersionAPI struct{}

func NewVersionAPI() *VersionAPI { return &VersionAPI{} }

// BinaryDownloadHandler proxies the GitHub release binary.
// GET /v1/binary/download?version=v1.2.3&os=linux&arch=amd64
func (v *VersionAPI) BinaryDownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.Header.Get("Authorization") == "" {
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

	url := buildDownloadURL(version, osParam, archParam)

	resp, err := http.Get(url)
	if err != nil {
		log.Error().Err(err).Str("url", url).Msg("failed to fetch binary from GitHub")
		http.Error(w, "failed to fetch binary", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("GitHub returned %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	if resp.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
	}
	io.Copy(w, resp.Body)
}

func buildDownloadURL(version, os, arch string) string {
	name := fmt.Sprintf("synergia-client-%s-%s", os, arch)
	if os == "windows" {
		name += ".exe"
	}
	return fmt.Sprintf("https://github.com/ylallemant/synergia/releases/download/%s/%s", version, name)
}
