package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/latency"
)

// LatencyAPI handles admin endpoints for latency monitoring.
type LatencyAPI struct {
	monitor *latency.Monitor
}

// NewLatencyAPI creates a new latency admin API handler.
func NewLatencyAPI(monitor *latency.Monitor) *LatencyAPI {
	return &LatencyAPI{monitor: monitor}
}

// LatencyHandler serves GET /v1/latency — returns the latency matrix.
func (a *LatencyAPI) LatencyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	role := r.URL.Query().Get("role")

	matrices, err := a.monitor.ComputeMatrix(role)
	if err != nil {
		log.Error().Err(err).Msg("failed to compute latency matrix")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if role != "" && len(matrices) == 1 {
		json.NewEncoder(w).Encode(matrices[0])
	} else {
		json.NewEncoder(w).Encode(map[string]any{
			"matrices": matrices,
		})
	}
}

// ConfigHandler serves GET/POST /v1/latency/config.
func (a *LatencyAPI) ConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := a.monitor.GetConfig()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)

	case http.MethodPost:
		var req latency.Config
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		a.monitor.SetConfig(req)
		cfg := a.monitor.GetConfig()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
