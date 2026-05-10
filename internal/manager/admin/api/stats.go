package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/ylallemant/synergia/internal/manager/cache"
)

// AdminStatsAPI exposes cache-backed stats to the JS-driven admin pages.
type AdminStatsAPI struct {
	cache *cache.Cache
}

func NewAdminStatsAPI(c *cache.Cache) *AdminStatsAPI {
	return &AdminStatsAPI{cache: c}
}

func (a *AdminStatsAPI) StatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := a.cache.GetStats()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"workers_synced":    stats.WorkersSynced,
		"workers_outdated":  stats.WorkersOutdated,
		"version_target":    stats.VersionTarget,
		"backend_synced":    stats.BackendSynced,
		"backend_outdated":  stats.BackendOutdated,
		"backend_version":   stats.BackendVersion,
		"model_synced":      stats.ModelSynced,
		"model_out_of_sync": stats.ModelOutOfSync,
	})
}
