package cache

import (
	"context"
	"sync"
	"time"

	"github.com/ylallemant/synergia/internal/manager/backend"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// Cache provides an in-memory cache for the admin dashboard and tag endpoints.
// Background goroutines periodically refresh DB-derived data.
// GitHub tags are only refreshed on explicit request (refresh button).
type Cache struct {
	mu    sync.RWMutex
	store *store.Store

	// Tags (refreshed only on explicit request)
	backendTags map[string][]string // backend name → tags
	clientTags  []string

	// Dashboard stats (refreshed periodically)
	stats DashboardStats
}

// DashboardStats holds all data needed to render the admin dashboard.
type DashboardStats struct {
	TotalWorkers       int64
	ReadyWorkers       int64
	ProcessingWorkers  int64
	UnavailableWorkers int64
	OfflineWorkers     int64
	RoleCounts         []RoleCount
	TodayTotal         int64
	TodayCompleted     int64
	TodayQueued        int64
	TodayTimeout       int64
	TodayFailed        int64
	RoleWorkCounts     []RoleWorkCount
	Errors             []ErrorEntry
	VersionTarget      string
	VersionRolloutMode string
	VersionPercentage  int
	VersionSHA256      string
	WorkersSynced      int64
	WorkersOutdated    int64
	BackendName        string
	BackendVersion     string
	BackendDownloadURL string
	BackendSHA256Full  string
	BackendSynced      int64
	BackendOutdated    int64
	// ModelSynced / ModelOutOfSync count non-offline workers by LLM model sync status.
	// A worker is model-synced when its reported llm_hash matches the expected hash
	// for its role. This is what directly gates work dispatch.
	ModelSynced    int64
	ModelOutOfSync int64
	Roles              []RoleEntry
}

// RoleCount holds role distribution info.
type RoleCount struct {
	Role   string
	Online int64
	Total  int64
}

// RoleWorkCount holds work units by role.
type RoleWorkCount struct {
	Role  string
	Total int64
}

// ErrorEntry holds a recent client error.
type ErrorEntry struct {
	Fingerprint string
	Version     string
	Error       string
	ReportedAt  string
}

// RoleEntry holds a role-model mapping.
type RoleEntry struct {
	Role          string
	Model         string
	Quantisation  string
	Filename      string
	ModelFileHash string
	MinVRAMMB     int
	Description   string
}

// New creates a new cache and starts background refresh goroutines.
func New(s *store.Store) *Cache {
	c := &Cache{
		store:       s,
		backendTags: make(map[string][]string),
	}
	// Initial stats load
	c.refreshStats()
	// Prefetch the latest client release tags in the background so they are
	// available for /download/* requests immediately without a manual refresh.
	go func() {
		if tags, err := fetchSynergiaTags(5); err == nil && len(tags) > 0 {
			c.mu.Lock()
			c.clientTags = tags
			c.mu.Unlock()
		}
	}()
	return c
}

// Start launches background goroutines. Call this once after construction.
func (c *Cache) Start(ctx context.Context) {
	go c.statsLoop(ctx, 1*time.Second)
}

// GetStats returns a snapshot of the current dashboard stats.
func (c *Cache) GetStats() DashboardStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

// GetBackendTags returns cached tags for the given backend.
func (c *Cache) GetBackendTags(name string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.backendTags[name]
}

// GetClientTags returns cached client release tags.
func (c *Cache) GetClientTags() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientTags
}

// RefreshBackendTags fetches tags from GitHub for the given backend and updates the cache.
func (c *Cache) RefreshBackendTags(name string) ([]string, error) {
	tags, err := backend.FetchTags(name, 20)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.backendTags[name] = tags
	c.mu.Unlock()
	return tags, nil
}

// RefreshClientTags fetches tags from GitHub for the client binary and updates the cache.
func (c *Cache) RefreshClientTags() ([]string, error) {
	tags, err := fetchSynergiaTags(20)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.clientTags = tags
	c.mu.Unlock()
	return tags, nil
}

// statsLoop periodically refreshes dashboard stats from the database.
func (c *Cache) statsLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshStats()
		}
	}
}

func (c *Cache) refreshStats() {
	var stats DashboardStats

	db := c.store.DB

	// Worker counts
	db.Model(&store.Worker{}).Count(&stats.TotalWorkers)
	db.Model(&store.Worker{}).Where("status IN ? AND sync_status = ?", []string{"available", "online"}, "synced").Count(&stats.ReadyWorkers)
	db.Model(&store.Worker{}).Where("status = ? AND sync_status = ?", "processing", "synced").Count(&stats.ProcessingWorkers)
	db.Model(&store.Worker{}).Where("status != ? AND NOT (status IN ? AND sync_status = ?) AND NOT (status = ? AND sync_status = ?)", "offline", []string{"available", "online"}, "synced", "processing", "synced").Count(&stats.UnavailableWorkers)
	db.Model(&store.Worker{}).Where("status = ?", "offline").Count(&stats.OfflineWorkers)

	// LLM model sync — independent of binary/backend version targets
	db.Model(&store.Worker{}).Where("status != ? AND sync_status = ?", "offline", "synced").Count(&stats.ModelSynced)
	db.Model(&store.Worker{}).Where("status != ? AND sync_status = ?", "offline", "out-of-sync").Count(&stats.ModelOutOfSync)

	// Workers by role
	type roleRow struct {
		Role   string
		Online int64
		Total  int64
	}
	var roleRows []roleRow
	db.Raw(`
		SELECT
		  CASE WHEN wc.preferred_role IS NOT NULL AND wc.preferred_role != '' THEN wc.preferred_role ELSE 'inference' END AS role,
		  COUNT(*) AS total,
		  SUM(CASE WHEN w.status IN ('available','processing') THEN 1 ELSE 0 END) AS online
		FROM workers w
		LEFT JOIN worker_configs wc ON wc.fingerprint = w.fingerprint
		GROUP BY role
		ORDER BY role
	`).Scan(&roleRows)
	for _, rr := range roleRows {
		stats.RoleCounts = append(stats.RoleCounts, RoleCount{
			Role:   rr.Role,
			Online: rr.Online,
			Total:  rr.Total,
		})
	}

	// Today's work units
	startOfDay := time.Now().Truncate(24 * time.Hour)
	db.Model(&store.WorkUnit{}).Where("created_at >= ?", startOfDay).Count(&stats.TodayTotal)
	db.Model(&store.WorkUnit{}).Where("created_at >= ? AND status = ?", startOfDay, "completed").Count(&stats.TodayCompleted)
	db.Model(&store.WorkUnit{}).Where("created_at >= ? AND status = ?", startOfDay, "timeout").Count(&stats.TodayTimeout)
	db.Model(&store.WorkUnit{}).Where("created_at >= ? AND status = ?", startOfDay, "failed").Count(&stats.TodayFailed)
	db.Model(&store.BatchRequest{}).Where("status IN ?", []string{"pending", "processing"}).Count(&stats.TodayQueued)

	// Work units by role today
	type roleWorkRow struct {
		Role  string
		Total int64
	}
	var rwRows []roleWorkRow
	db.Raw(`
		SELECT role, COUNT(*) AS total
		FROM latency_samples
		WHERE created_at >= ?
		GROUP BY role
		ORDER BY role
	`, startOfDay).Scan(&rwRows)
	for _, rw := range rwRows {
		stats.RoleWorkCounts = append(stats.RoleWorkCounts, RoleWorkCount{
			Role:  rw.Role,
			Total: rw.Total,
		})
	}

	// Last 10 errors
	var errors []store.ClientError
	db.Order("reported_at desc").Limit(10).Find(&errors)
	for _, e := range errors {
		errMsg := e.ErrorMessage
		if len(errMsg) > 120 {
			errMsg = errMsg[:120] + "…"
		}
		fp := e.Fingerprint
		if len(fp) > 12 {
			fp = fp[:12] + "…"
		}
		stats.Errors = append(stats.Errors, ErrorEntry{
			Fingerprint: fp,
			Version:     e.Version,
			Error:       errMsg,
			ReportedAt:  e.ReportedAt.Format("2006-01-02 15:04:05"),
		})
	}

	// Binary version config
	if cfg, err := c.store.GetClientVersionConfig(); err == nil {
		stats.VersionTarget = cfg.TargetVersion
		stats.VersionRolloutMode = cfg.RolloutMode
		stats.VersionPercentage = cfg.RolloutPercentage
	}
	if stats.VersionTarget != "" {
		db.Model(&store.Worker{}).Where("status != ? AND client_version = ?", "offline", stats.VersionTarget).Count(&stats.WorkersSynced)
		db.Model(&store.Worker{}).Where("status != ? AND client_version != ?", "offline", stats.VersionTarget).Count(&stats.WorkersOutdated)
	}

	// Backend version config
	if cfg, err := c.store.GetBackendVersionConfig(); err == nil {
		stats.BackendName = cfg.Name
		stats.BackendVersion = cfg.Version
		stats.BackendDownloadURL = cfg.DownloadURL
		stats.BackendSHA256Full = cfg.SHA256
	}
	if stats.BackendVersion != "" {
		db.Model(&store.Worker{}).Where("status != ? AND backend_sync_status = ?", "offline", "synced").Count(&stats.BackendSynced)
		db.Model(&store.Worker{}).Where("status != ? AND backend_sync_status = ?", "offline", "out-of-sync").Count(&stats.BackendOutdated)
	}

	// Roles
	if roles, err := c.store.GetRoleModels(); err == nil {
		for _, r := range roles {
			stats.Roles = append(stats.Roles, RoleEntry{
				Role:          r.Role,
				Model:         r.LLMModel,
				Quantisation:  r.Quantisation,
				Filename:      r.ModelFilename,
				ModelFileHash: r.ModelFileHash,
				MinVRAMMB:     r.MinVRAMMB,
				Description:   r.Description,
			})
		}
	}

	c.mu.Lock()
	c.stats = stats
	c.mu.Unlock()
}
