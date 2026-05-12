package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/ylallemant/synergia/internal/manager/backend"
	"github.com/ylallemant/synergia/internal/manager/cache"
)

// AdminStatsAPI exposes cache-backed stats to the JS-driven admin pages.
type AdminStatsAPI struct {
	cache          *cache.Cache
	managerVersion string
}

func NewAdminStatsAPI(c *cache.Cache) *AdminStatsAPI {
	return &AdminStatsAPI{cache: c}
}

func (a *AdminStatsAPI) SetManagerVersion(v string) { a.managerVersion = v }

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
		"deleted_workers":   stats.DeletedWorkers,
	})
}

// VersionsStatusHandler returns a consolidated view of all tracked versions and
// their latest available counterparts from the GitHub release cache.
// GET /v1/admin/versions/status
func (a *AdminStatsAPI) VersionsStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := a.cache.GetStats()

	latestClient := ""
	if tags := a.cache.GetClientTags(); len(tags) > 0 {
		latestClient = tags[0]
	}

	latestBackend := ""
	if tags := a.cache.GetBackendTags(backend.LlamaCpp); len(tags) > 0 {
		latestBackend = tags[0]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"manager_version":   a.managerVersion,
		"manager_latest":    latestClient, // manager and client share the same release
		"client_target":     stats.VersionTarget,
		"client_latest":     latestClient,
		"client_synced":     stats.WorkersSynced,
		"client_outdated":   stats.WorkersOutdated,
		"backend_version":   stats.BackendVersion,
		"backend_latest":    latestBackend,
		"backend_synced":    stats.BackendSynced,
		"backend_outdated":  stats.BackendOutdated,
	})
}
