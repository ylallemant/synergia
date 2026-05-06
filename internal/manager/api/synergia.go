package api

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// SynergiaAPI provides admin and status endpoints for the cluster manager.
// These endpoints are called by the Flow Engine or other internal services.
type SynergiaAPI struct {
	apiKey string
	store  *store.Store
}

func NewSynergiaAPI(apiKey string, s *store.Store) *SynergiaAPI {
	return &SynergiaAPI{
		apiKey: apiKey,
		store:  s,
	}
}

// ModelsHandler returns the list of available models (workers that are online).
// GET /v1/models — OpenAI-compatible model listing.
func (d *SynergiaAPI) ModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !d.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var workers []store.Worker
	if err := d.store.DB.Where("status = ?", "online").Find(&workers).Error; err != nil {
		log.Error().Err(err).Msg("failed to query workers")
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	// Deduplicate models from online workers
	modelSet := make(map[string]bool)
	var models []map[string]any
	for _, worker := range workers {
		key := worker.LLMModel + "/" + worker.Quantisation
		if modelSet[key] {
			continue
		}
		modelSet[key] = true
		models = append(models, map[string]any{
			"id":       worker.LLMModel,
			"object":   "model",
			"owned_by": "synergia",
			"metadata": map[string]any{
				"quantisation": worker.Quantisation,
				"worker_count": 1,
				"fingerprint":  worker.Fingerprint,
			},
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	})
}

// WorkersHandler returns the list of all registered workers.
// GET /v1/workers — cluster-specific endpoint.
func (d *SynergiaAPI) WorkersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !d.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var workers []store.Worker
	if err := d.store.DB.Order("last_seen_at desc").Find(&workers).Error; err != nil {
		log.Error().Err(err).Msg("failed to query workers")
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	var result []map[string]any
	for _, worker := range workers {
		result = append(result, map[string]any{
			"fingerprint":  worker.Fingerprint,
			"model":        worker.LLMModel,
			"quantisation": worker.Quantisation,
			"trust_score":  worker.TrustScore,
			"status":       worker.Status,
			"last_seen_at": worker.LastSeenAt,
			"created_at":   worker.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"workers": result,
		"total":   len(result),
	})
}

// WorkUnitsHandler returns recent work unit history.
// GET /v1/work-units — cluster-specific endpoint.
func (d *SynergiaAPI) WorkUnitsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !d.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var units []store.WorkUnit
	if err := d.store.DB.Order("created_at desc").Limit(100).Find(&units).Error; err != nil {
		log.Error().Err(err).Msg("failed to query work units")
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	var result []map[string]any
	for _, unit := range units {
		entry := map[string]any{
			"unit_id":            unit.UnitID,
			"worker_fingerprint": unit.WorkerFingerprint,
			"model":              unit.LLMModel,
			"status":             unit.Status,
			"processing_time_ms": unit.ProcessingTimeMs,
			"created_at":         unit.CreatedAt,
		}
		if unit.CompletedAt != nil {
			entry["completed_at"] = unit.CompletedAt
		}
		if unit.ErrorMessage != "" {
			entry["error"] = unit.ErrorMessage
		}
		result = append(result, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"work_units": result,
		"total":      len(result),
	})
}

// StatsHandler returns aggregate cluster statistics.
// GET /v1/stats — cluster-specific endpoint.
func (d *SynergiaAPI) StatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !d.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var totalWorkers, onlineWorkers int64
	d.store.DB.Model(&store.Worker{}).Count(&totalWorkers)
	d.store.DB.Model(&store.Worker{}).Where("status = ?", "online").Count(&onlineWorkers)

	var totalUnits, completedUnits, failedUnits, timeoutUnits int64
	d.store.DB.Model(&store.WorkUnit{}).Count(&totalUnits)
	d.store.DB.Model(&store.WorkUnit{}).Where("status = ?", "completed").Count(&completedUnits)
	d.store.DB.Model(&store.WorkUnit{}).Where("status = ?", "failed").Count(&failedUnits)
	d.store.DB.Model(&store.WorkUnit{}).Where("status = ?", "timeout").Count(&timeoutUnits)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"workers": map[string]any{
			"total":  totalWorkers,
			"online": onlineWorkers,
		},
		"work_units": map[string]any{
			"total":     totalUnits,
			"completed": completedUnits,
			"failed":    failedUnits,
			"timeout":   timeoutUnits,
		},
	})
}

func (d *SynergiaAPI) authenticate(r *http.Request) bool {
	if apiKey := r.Header.Get("X-API-Key"); apiKey == d.apiKey {
		return true
	}
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:] == d.apiKey
	}
	return false
}
