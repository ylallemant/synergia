package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// ConsentAPI handles worker consent and configuration endpoints.
type ConsentAPI struct {
	workerKey string
	store     *store.Store
}

func NewConsentAPI(workerKey string, s *store.Store) *ConsentAPI {
	return &ConsentAPI{
		workerKey: workerKey,
		store:     s,
	}
}

// ConsentRequest is the JSON body for consent submission.
type ConsentRequest struct {
	Fingerprint       string        `json:"fingerprint"`
	Accepted          bool          `json:"accepted"`
	HardwareStats     bool          `json:"hardware_stats"`
	ConfigPreferences bool          `json:"config_preferences"`
	Hardware          *HardwareInfo `json:"hardware,omitempty"`
}

// HardwareInfo contains the hardware statistics reported by the worker.
type HardwareInfo struct {
	OS               string `json:"os"`
	OSVer            string `json:"os_version"`
	GPU              string `json:"gpu"`
	GPUDriver        string `json:"gpu_driver"`
	GPUDriverVersion string `json:"gpu_driver_version"`
	VRAMMB           int    `json:"vram_mb"`
	CPU              string `json:"cpu"`
	CPUCores         int    `json:"cpu_cores"`
	RAMMB            int    `json:"ram_mb"`
}

// ConsentResponse is returned on consent queries.
type ConsentResponse struct {
	Fingerprint       string `json:"fingerprint"`
	Accepted          bool   `json:"accepted"`
	AcceptedAt        string `json:"accepted_at,omitempty"`
	HardwareStats     bool   `json:"hardware_stats"`
	ConfigPreferences bool   `json:"config_preferences"`
}

// ConfigRequest is the JSON body for worker config updates.
type ConfigRequest struct {
	Fingerprint   string `json:"fingerprint"`
	PreferredRole string `json:"preferred_role"`
}

// ConfigResponse is returned on config queries.
type ConfigResponse struct {
	Fingerprint   string `json:"fingerprint"`
	PreferredRole string `json:"preferred_role"`
}

// ConsentHandler handles GET and POST on /v1/consent.
func (c *ConsentAPI) ConsentHandler(w http.ResponseWriter, r *http.Request) {
	if !c.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		c.getConsent(w, r)
	case http.MethodPost:
		c.setConsent(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (c *ConsentAPI) getConsent(w http.ResponseWriter, r *http.Request) {
	fingerprint := r.URL.Query().Get("fingerprint")
	if fingerprint == "" {
		writeError(w, http.StatusBadRequest, "fingerprint query parameter required")
		return
	}

	consent, err := c.store.GetConsent(fingerprint)
	if err != nil {
		log.Error().Err(err).Msg("failed to query consent")
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	resp := ConsentResponse{Fingerprint: fingerprint}
	if consent != nil {
		resp.Accepted = consent.Accepted
		resp.HardwareStats = consent.HardwareStats
		resp.ConfigPreferences = consent.ConfigPreferences
		if consent.AcceptedAt != nil {
			resp.AcceptedAt = consent.AcceptedAt.Format("2006-01-02T15:04:05Z")
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (c *ConsentAPI) setConsent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var req ConsentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.Fingerprint == "" {
		writeError(w, http.StatusBadRequest, "fingerprint is required")
		return
	}

	var hw *store.HardwareInfo
	if req.Hardware != nil {
		hw = &store.HardwareInfo{
			OS:               req.Hardware.OS,
			OSVer:            req.Hardware.OSVer,
			GPU:              req.Hardware.GPU,
			GPUDriver:        req.Hardware.GPUDriver,
			GPUDriverVersion: req.Hardware.GPUDriverVersion,
			VRAMMB:           req.Hardware.VRAMMB,
			CPU:              req.Hardware.CPU,
			CPUCores:         req.Hardware.CPUCores,
			RAMMB:            req.Hardware.RAMMB,
		}
	}

	if err := c.store.SetConsent(req.Fingerprint, req.Accepted, req.HardwareStats, req.ConfigPreferences, hw); err != nil {
		log.Error().Err(err).Str("fingerprint", req.Fingerprint).Msg("failed to set consent")
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	// When consent is withdrawn, mark worker so it becomes unavailable for dispatch
	if !req.Accepted {
		if err := c.store.SetWorkerStatus(req.Fingerprint, "withdrawn"); err != nil {
			log.Error().Err(err).Str("fingerprint", req.Fingerprint).Msg("failed to set worker status on consent withdrawal")
		}
	} else {
		// When consent is re-accepted, restore worker to available (if currently withdrawn)
		c.store.SetWorkerAvailableIfWithdrawn(req.Fingerprint)
	}

	log.Info().
		Bool("accepted", req.Accepted).
		Str("fingerprint", req.Fingerprint).
		Msg("consent updated")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ConfigHandler handles GET and POST on /v1/worker-config.
func (c *ConsentAPI) ConfigHandler(w http.ResponseWriter, r *http.Request) {
	if !c.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		c.getConfig(w, r)
	case http.MethodPost:
		c.setConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (c *ConsentAPI) getConfig(w http.ResponseWriter, r *http.Request) {
	fingerprint := r.URL.Query().Get("fingerprint")
	if fingerprint == "" {
		writeError(w, http.StatusBadRequest, "fingerprint query parameter required")
		return
	}

	// Verify consent before returning config
	if !c.store.HasConsent(fingerprint) {
		writeError(w, http.StatusForbidden, "worker has not accepted data collection terms")
		return
	}

	config, err := c.store.GetWorkerConfig(fingerprint)
	if err != nil {
		log.Error().Err(err).Msg("failed to query worker config")
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	resp := ConfigResponse{Fingerprint: fingerprint}
	if config != nil {
		resp.PreferredRole = config.PreferredRole
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (c *ConsentAPI) setConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var req ConfigRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.Fingerprint == "" {
		writeError(w, http.StatusBadRequest, "fingerprint is required")
		return
	}

	// Verify consent before storing config
	if !c.store.HasConsent(req.Fingerprint) {
		writeError(w, http.StatusForbidden, "worker has not accepted data collection terms")
		return
	}

	if err := c.store.SetWorkerConfig(req.Fingerprint, req.PreferredRole); err != nil {
		log.Error().Err(err).Str("fingerprint", req.Fingerprint).Msg("failed to set worker config")
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	log.Debug().Str("fingerprint", req.Fingerprint).Msg("worker config updated")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (c *ConsentAPI) authenticate(r *http.Request) bool {
	if apiKey := r.Header.Get("X-API-Key"); apiKey == c.workerKey {
		return true
	}
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:] == c.workerKey
	}
	return false
}
