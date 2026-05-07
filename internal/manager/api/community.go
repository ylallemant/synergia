package api

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

//go:embed public/community.html
var communityPageFS embed.FS

var communityTmpl = template.Must(template.ParseFS(communityPageFS, "public/community.html"))

// CommunityAPI serves the public community stats page and API.
type CommunityAPI struct {
	store *store.Store
}

// NewCommunityAPI creates a new community stats handler.
func NewCommunityAPI(s *store.Store) *CommunityAPI {
	return &CommunityAPI{store: s}
}

// PageHandler serves GET /community — public live dashboard.
func (c *CommunityAPI) PageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := communityTmpl.Execute(w, nil); err != nil {
		log.Error().Err(err).Msg("failed to render community page")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// CommunityStats holds the aggregate cluster metrics for the public API.
type CommunityStats struct {
	WorkersOnline  int64              `json:"workers_online"`
	WorkersTotal   int64              `json:"workers_total"`
	WorkUnitsTotal int64              `json:"work_units_total"`
	WorkUnitsToday int64              `json:"work_units_today"`
	AvgLatencyMs   int64              `json:"avg_latency_ms"`
	GPUBreakdown   map[string]int64   `json:"gpu_breakdown"`
	Leaderboard    []LeaderboardEntry `json:"leaderboard"`
}

// LeaderboardEntry represents a contributor on the leaderboard.
type LeaderboardEntry struct {
	Nickname    string `json:"nickname"`
	WorkUnits   int64  `json:"work_units"`
	TotalTimeMs int64  `json:"total_time_ms"`
}

// StatsHandler serves GET /v1/community/stats — public JSON API.
func (c *CommunityAPI) StatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := c.computeStats()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (c *CommunityAPI) computeStats() CommunityStats {
	var stats CommunityStats

	// Worker counts
	c.store.DB.Model(&store.Worker{}).Count(&stats.WorkersTotal)
	c.store.DB.Model(&store.Worker{}).
		Where("status NOT IN ?", []string{"offline", "withdrawn"}).
		Count(&stats.WorkersOnline)

	// Work unit counts
	c.store.DB.Model(&store.WorkUnit{}).
		Where("status = ?", "completed").
		Count(&stats.WorkUnitsTotal)

	today := time.Now().Truncate(24 * time.Hour)
	c.store.DB.Model(&store.WorkUnit{}).
		Where("status = ? AND created_at >= ?", "completed", today).
		Count(&stats.WorkUnitsToday)

	// Average latency (completed units today)
	var avgLatency struct{ Avg *float64 }
	c.store.DB.Model(&store.WorkUnit{}).
		Select("AVG(processing_time_ms) as avg").
		Where("status = ? AND completed_at >= ?", "completed", today).
		Scan(&avgLatency)
	if avgLatency.Avg != nil {
		stats.AvgLatencyMs = int64(*avgLatency.Avg)
	}

	// GPU breakdown from consent hardware info
	stats.GPUBreakdown = make(map[string]int64)
	type gpuRow struct {
		GPU   string
		Count int64
	}
	var gpuRows []gpuRow
	c.store.DB.Model(&store.WorkerConsent{}).
		Select("hw_gpu as gpu, COUNT(*) as count").
		Where("hw_gpu != ''").
		Group("hw_gpu").
		Scan(&gpuRows)
	for _, row := range gpuRows {
		stats.GPUBreakdown[row.GPU] = row.Count
	}

	// Leaderboard: top 20 contributors by completed work units
	type leaderRow struct {
		Fingerprint string
		WorkUnits   int64
		TotalTimeMs int64
	}
	var rows []leaderRow
	c.store.DB.Model(&store.WorkUnit{}).
		Select("worker_fingerprint as fingerprint, COUNT(*) as work_units, SUM(processing_time_ms) as total_time_ms").
		Where("status = ?", "completed").
		Group("worker_fingerprint").
		Order("work_units DESC").
		Limit(20).
		Scan(&rows)

	// Resolve nicknames
	for _, row := range rows {
		entry := LeaderboardEntry{
			WorkUnits:   row.WorkUnits,
			TotalTimeMs: row.TotalTimeMs,
		}

		// Try to get nickname from worker config
		cfg, err := c.store.GetWorkerConfig(row.Fingerprint)
		if err == nil && cfg != nil && cfg.Nickname != "" {
			entry.Nickname = cfg.Nickname
		} else {
			// Use truncated fingerprint as fallback
			if len(row.Fingerprint) > 8 {
				entry.Nickname = row.Fingerprint[:8] + "..."
			} else {
				entry.Nickname = row.Fingerprint
			}
		}

		stats.Leaderboard = append(stats.Leaderboard, entry)
	}

	return stats
}
