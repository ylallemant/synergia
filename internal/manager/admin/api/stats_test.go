package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ylallemant/synergia/internal/manager/cache"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// openTestStore opens an in-memory SQLite store for testing.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	return s
}

// statsResponse is the shape of the /v1/admin/stats response.
type statsResponse struct {
	VersionTarget   string `json:"version_target"`
	WorkersSynced   int64  `json:"workers_synced"`
	WorkersOutdated int64  `json:"workers_outdated"`
}

func getStats(t *testing.T, handler http.Handler) statsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/stats", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats handler returned %d", rec.Code)
	}
	var s statsResponse
	if err := json.NewDecoder(rec.Body).Decode(&s); err != nil {
		t.Fatalf("failed to decode stats response: %v", err)
	}
	return s
}

// TestStatsVersionTargetEmpty reproduces the UI confusion: the Workers page
// populates the Target Version dropdown from GitHub tags on load, making it
// appear that a version is "selected" — but /v1/admin/stats returns an empty
// version_target until the admin actually presses Save & Push. The JS notice
// "No target version configured" is therefore correct even when the dropdown
// shows a tag, but was not explaining *why* clearly enough.
func TestStatsVersionTargetEmpty(t *testing.T) {
	s := openTestStore(t)
	c := cache.New(s)
	api := NewAdminStatsAPI(c)

	// Before any version target is saved, stats must return an empty string.
	// This is the state that triggers the "No target version configured" notice
	// on the Workers page even when the dropdown already shows the latest tag.
	stats := getStats(t, http.HandlerFunc(api.StatsHandler))
	if stats.VersionTarget != "" {
		t.Errorf("expected empty version_target before save, got %q", stats.VersionTarget)
	}
}

// TestStatsVersionTargetAfterSave verifies that once a target is saved via
// SetClientVersionConfig, /v1/admin/stats reflects it after a cache refresh.
// This is the post-"Save & Push" state that replaces the notice with the
// Synced / Out of Sync cards.
func TestStatsVersionTargetAfterSave(t *testing.T) {
	s := openTestStore(t)

	// Save a target version — simulates what POST /v1/admin/version does.
	if err := s.SetClientVersionConfig("0.0.11", "all", 100); err != nil {
		t.Fatalf("failed to set version config: %v", err)
	}

	// cache.New calls refreshStats() synchronously before returning, so the
	// stats already reflect the DB state by the time we query.
	c := cache.New(s)
	api := NewAdminStatsAPI(c)

	stats := getStats(t, http.HandlerFunc(api.StatsHandler))
	if stats.VersionTarget != "0.0.11" {
		t.Errorf("expected version_target %q after save, got %q", "0.0.11", stats.VersionTarget)
	}
}
