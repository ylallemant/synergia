package latency

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// BucketStats holds computed percentile statistics for one payload-size bucket.
type BucketStats struct {
	RangeMin int    `json:"range_min"`
	RangeMax int    `json:"range_max"` // 0 means unbounded (last bucket)
	Range    string `json:"range"`
	Count    int    `json:"count"`
	P50Ms    int64  `json:"p50_ms"`
	P95Ms    int64  `json:"p95_ms"`
	P99Ms    int64  `json:"p99_ms"`
}

// RoleMatrix holds the latency matrix for one role.
type RoleMatrix struct {
	Role        string        `json:"role"`
	WindowHours int           `json:"window_hours"`
	BucketCount int           `json:"bucket_count"`
	Bounds      []int         `json:"bounds"`
	Matrix      []BucketStats `json:"matrix"`
}

// Config holds the configurable parameters for latency monitoring.
type Config struct {
	BucketCount int `json:"bucket_count"`
	WindowHours int `json:"window_hours"`
}

// Monitor manages latency observation recording and periodic aggregation.
type Monitor struct {
	store  *store.Store
	config Config

	mu     sync.RWMutex
	stopCh chan struct{}
}

// New creates a new latency monitor and starts the background aggregation goroutine.
func New(s *store.Store, bucketCount, windowHours int) *Monitor {
	m := &Monitor{
		store: s,
		config: Config{
			BucketCount: bucketCount,
			WindowHours: windowHours,
		},
		stopCh: make(chan struct{}),
	}
	go m.runAggregationLoop()
	return m
}

// GetConfig returns the current latency monitoring configuration.
func (m *Monitor) GetConfig() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// SetConfig updates the latency monitoring configuration.
func (m *Monitor) SetConfig(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.BucketCount > 0 {
		m.config.BucketCount = cfg.BucketCount
	}
	if cfg.WindowHours > 0 {
		m.config.WindowHours = cfg.WindowHours
	}
}

// Record stores a latency sample. Called on each completed work unit.
func (m *Monitor) Record(fingerprint, role string, payloadBytes int, latencyMs int64) {
	if err := m.store.RecordLatencySample(fingerprint, role, payloadBytes, latencyMs); err != nil {
		log.Error().Err(err).Str("fingerprint", fingerprint).Msg("failed to record latency sample")
	}
}

// ComputeMatrix computes the latency matrix for a given role (or all roles if role is empty).
func (m *Monitor) ComputeMatrix(role string) ([]RoleMatrix, error) {
	m.mu.RLock()
	cfg := m.config
	m.mu.RUnlock()

	since := time.Now().Add(-time.Duration(cfg.WindowHours) * time.Hour)

	var roles []string
	if role != "" {
		roles = []string{role}
	} else {
		var err error
		roles, err = m.store.GetDistinctRoles(since)
		if err != nil {
			return nil, err
		}
		// Fallback: if no hourly stats yet, get roles from raw samples
		if len(roles) == 0 {
			roles, err = m.store.GetDistinctSampleRoles(since)
			if err != nil {
				return nil, err
			}
		}
	}

	var matrices []RoleMatrix
	for _, r := range roles {
		matrix, err := m.computeRoleMatrix(r, cfg, since)
		if err != nil {
			return nil, err
		}
		matrices = append(matrices, *matrix)
	}
	return matrices, nil
}

func (m *Monitor) computeRoleMatrix(role string, cfg Config, since time.Time) (*RoleMatrix, error) {
	// Get hourly stats to compute adaptive bucket boundaries
	stats, err := m.store.GetHourlyStats(since, role)
	if err != nil {
		return nil, err
	}

	var bounds []int
	if len(stats) > 0 {
		bounds = computeBounds(stats, cfg.BucketCount)
	} else {
		// Fallback: compute bounds directly from raw samples
		minPayload, maxPayload, err := m.store.GetSamplePayloadRange(since, role)
		if err != nil {
			return nil, err
		}
		bounds = computeBoundsFromRange(minPayload, maxPayload, cfg.BucketCount)
	}

	// When bounds are nil (not enough spread), use a single bucket
	effectiveBuckets := cfg.BucketCount
	if bounds == nil {
		effectiveBuckets = 1
	}

	// Build matrix by querying samples in each bucket range
	matrix := make([]BucketStats, effectiveBuckets)
	for i := 0; i < effectiveBuckets; i++ {
		minBytes := 0
		maxBytes := 0 // 0 = unbounded

		if i == 0 {
			if len(bounds) > 0 {
				maxBytes = bounds[0]
			}
		} else if i < len(bounds) {
			minBytes = bounds[i-1]
			maxBytes = bounds[i]
		} else {
			if len(bounds) > 0 {
				minBytes = bounds[len(bounds)-1]
			}
		}

		samples, err := m.store.GetLatencySamplesInRange(since, role, minBytes, maxBytes)
		if err != nil {
			return nil, err
		}

		bs := BucketStats{
			RangeMin: minBytes,
			RangeMax: maxBytes,
			Count:    len(samples),
		}
		if maxBytes == 0 {
			bs.Range = formatRange(minBytes, 0)
		} else {
			bs.Range = formatRange(minBytes, maxBytes)
		}

		if len(samples) > 0 {
			latencies := make([]int64, len(samples))
			for j, s := range samples {
				latencies[j] = s.LatencyMs
			}
			sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
			bs.P50Ms = percentile(latencies, 50)
			bs.P95Ms = percentile(latencies, 95)
			bs.P99Ms = percentile(latencies, 99)
		}
		matrix[i] = bs
	}

	return &RoleMatrix{
		Role:        role,
		WindowHours: cfg.WindowHours,
		BucketCount: cfg.BucketCount,
		Bounds:      bounds,
		Matrix:      matrix,
	}, nil
}

// computeBounds derives adaptive bucket boundaries from hourly stats.
func computeBounds(stats []store.LatencyHourlyStat, bucketCount int) []int {
	if len(stats) == 0 || bucketCount <= 1 {
		return nil
	}

	var sumMin, sumMax int64
	for _, s := range stats {
		sumMin += int64(s.MinPayloadBytes)
		sumMax += int64(s.MaxPayloadBytes)
	}
	avgMin := int(sumMin / int64(len(stats)))
	avgMax := int(sumMax / int64(len(stats)))

	if avgMax <= avgMin {
		return nil
	}

	bucketSize := float64(avgMax-avgMin) / float64(bucketCount)
	bounds := make([]int, bucketCount-1)
	for i := 0; i < bucketCount-1; i++ {
		bounds[i] = avgMin + int(math.Round(bucketSize*float64(i+1)))
	}
	return bounds
}

// computeBoundsFromRange derives bucket boundaries directly from a min/max payload range.
// Used as fallback when no hourly stats have been aggregated yet.
func computeBoundsFromRange(minPayload, maxPayload, bucketCount int) []int {
	if bucketCount <= 1 || maxPayload <= minPayload {
		return nil
	}

	bucketSize := float64(maxPayload-minPayload) / float64(bucketCount)
	bounds := make([]int, bucketCount-1)
	for i := 0; i < bucketCount-1; i++ {
		bounds[i] = minPayload + int(math.Round(bucketSize*float64(i+1)))
	}
	return bounds
}

func percentile(sorted []int64, pct int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := len(sorted) * pct / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func formatRange(min, max int) string {
	if max == 0 {
		return fmt.Sprintf("%d+", min)
	}
	return fmt.Sprintf("%d-%d", min, max)
}

// Stop halts the background aggregation goroutine.
func (m *Monitor) Stop() {
	close(m.stopCh)
}

func (m *Monitor) runAggregationLoop() {
	// Align to the next hour boundary
	now := time.Now()
	nextHour := now.Truncate(time.Hour).Add(time.Hour)
	timer := time.NewTimer(time.Until(nextHour))

	for {
		select {
		case <-m.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			m.aggregate()
			// Reset for next hour
			timer.Reset(time.Hour)
		}
	}
}

func (m *Monitor) aggregate() {
	m.mu.RLock()
	cfg := m.config
	m.mu.RUnlock()

	// Compute stats for the hour that just completed
	completedHour := time.Now().Truncate(time.Hour).Add(-time.Hour)

	// Get all roles that have samples
	since := time.Now().Add(-time.Duration(cfg.WindowHours) * time.Hour)
	roles, err := m.store.GetDistinctRoles(since)
	if err != nil {
		log.Error().Err(err).Msg("latency: failed to get distinct roles")
		return
	}

	for _, role := range roles {
		if err := m.store.ComputeHourlyStat(role, completedHour); err != nil {
			log.Error().Err(err).Str("role", role).Msg("latency: failed to compute hourly stat")
		}
	}

	// Purge old data
	cutoff := time.Now().Add(-time.Duration(cfg.WindowHours) * time.Hour)
	if err := m.store.PurgeOldLatencyData(cutoff); err != nil {
		log.Error().Err(err).Msg("latency: failed to purge old data")
	}

	log.Debug().Int("roles", len(roles)).Time("hour", completedHour).Msg("latency: hourly aggregation complete")
}
