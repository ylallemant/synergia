package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/gateway"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// VersionAPI handles client binary version configuration and push.
type VersionAPI struct {
	apiKey  string
	store   *store.Store
	gateway *gateway.Gateway
}

func NewVersionAPI(apiKey string, s *store.Store, gw *gateway.Gateway) *VersionAPI {
	return &VersionAPI{
		apiKey:  apiKey,
		store:   s,
		gateway: gw,
	}
}

type versionConfigRequest struct {
	TargetVersion     string `json:"target_version"`
	RolloutMode       string `json:"rollout_mode"`
	RolloutPercentage int    `json:"rollout_percentage"`
	SHA256            string `json:"sha256"`
}

type versionConfigResponse struct {
	TargetVersion     string `json:"target_version"`
	RolloutMode       string `json:"rollout_mode"`
	RolloutPercentage int    `json:"rollout_percentage"`
}

// AdminVersionHandler handles GET/POST /v1/admin/version.
func (v *VersionAPI) AdminVersionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+v.apiKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		v.getVersion(w, r)
	case http.MethodPost:
		v.setVersion(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (v *VersionAPI) getVersion(w http.ResponseWriter, _ *http.Request) {
	cfg, err := v.store.GetClientVersionConfig()
	if err != nil {
		http.Error(w, "no version config set", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(versionConfigResponse{
		TargetVersion:     cfg.TargetVersion,
		RolloutMode:       cfg.RolloutMode,
		RolloutPercentage: cfg.RolloutPercentage,
	})
}

func (v *VersionAPI) setVersion(w http.ResponseWriter, r *http.Request) {
	var req versionConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.TargetVersion == "" {
		http.Error(w, "target_version is required", http.StatusBadRequest)
		return
	}

	if req.RolloutMode == "" {
		req.RolloutMode = "all"
	}
	if req.RolloutMode != "all" && req.RolloutMode != "percentage" {
		http.Error(w, "rollout_mode must be 'all' or 'percentage'", http.StatusBadRequest)
		return
	}
	if req.RolloutMode == "percentage" && (req.RolloutPercentage < 1 || req.RolloutPercentage > 100) {
		http.Error(w, "rollout_percentage must be 1-100", http.StatusBadRequest)
		return
	}
	if req.RolloutMode == "all" {
		req.RolloutPercentage = 100
	}

	if err := v.store.SetClientVersionConfig(req.TargetVersion, req.RolloutMode, req.RolloutPercentage); err != nil {
		log.Error().Err(err).Msg("failed to save client version config")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Push binary update to connected worker
	if v.gateway != nil && v.gateway.HasWorker() {
		info := v.gateway.WorkerStatus()
		if info != nil && info.Version != req.TargetVersion {
			downloadURL := buildDownloadURL(req.TargetVersion, info.OS, info.Arch)
			fallbackURL := "/v1/binary/download"
			if err := v.gateway.PushBinaryUpdate(req.TargetVersion, downloadURL, fallbackURL, req.SHA256); err != nil {
				log.Error().Err(err).Msg("failed to push binary update")
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// buildDownloadURL constructs the GitHub release download URL for a given version/os/arch.
func buildDownloadURL(version, os, arch string) string {
	name := fmt.Sprintf("synergia-client-%s-%s", os, arch)
	if os == "windows" {
		name += ".exe"
	}
	return fmt.Sprintf("https://github.com/ylallemant/synergia/releases/download/%s/%s", version, name)
}

// BinaryDownloadHandler proxies the GitHub release binary for workers that cannot reach GitHub directly.
// GET /v1/binary/download?version=v1.2.3&os=linux&arch=amd64
func (v *VersionAPI) BinaryDownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate with worker key (workers call this)
	workerKey := r.Header.Get("Authorization")
	if workerKey == "" {
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
