package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ylallemant/synergia/internal/manager/store"
)

// openStore opens a fully-migrated in-memory SQLite store for API tests.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	return s
}

// noKey returns a func() string that always returns "" (TOFU mode — no auth).
func noKey() func() string { return func() string { return "" } }

// withKey returns a func() string that always returns key (key-auth mode).
func withKey(key string) func() string { return func() string { return key } }

// ── RolesAPI ──────────────────────────────────────────────────────────────────

func TestRolesHandler_GET_ReturnsRoles(t *testing.T) {
	s := openStore(t)
	s.UpsertRoleModel("tester", "SmolLM2", "Q4_K_M", "smol.gguf", "h", 512, "test role") //nolint:errcheck

	api := NewRolesAPI("apikey", noKey(), s, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/roles", nil)
	rec := httptest.NewRecorder()
	api.RolesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var roles []RoleInfo
	if err := json.NewDecoder(rec.Body).Decode(&roles); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(roles) == 0 {
		t.Error("expected at least one role")
	}
}

func TestRolesHandler_POST_Returns405(t *testing.T) {
	s := openStore(t)
	api := NewRolesAPI("apikey", noKey(), s, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", nil)
	rec := httptest.NewRecorder()
	api.RolesHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

func TestRolesHandler_WrongKey_Returns401(t *testing.T) {
	s := openStore(t)
	api := NewRolesAPI("apikey", withKey("secret"), s, false)
	req := httptest.NewRequest(http.MethodGet, "/v1/roles", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	api.RolesHandler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for wrong key, got %d", rec.Code)
	}
}

// ── SynergiaAPI ───────────────────────────────────────────────────────────────

func TestModelsHandler_GET_ReturnsJSON(t *testing.T) {
	s := openStore(t)
	api := NewSynergiaAPI("apikey", s)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer apikey")
	rec := httptest.NewRecorder()
	api.ModelsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["object"] != "list" {
		t.Errorf("want object=list, got %q", m["object"])
	}
}

func TestModelsHandler_POST_Returns405(t *testing.T) {
	s := openStore(t)
	api := NewSynergiaAPI("apikey", s)
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	rec := httptest.NewRecorder()
	api.ModelsHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

func TestWorkersHandler_GET_RequiresAPIKey(t *testing.T) {
	s := openStore(t)
	api := NewSynergiaAPI("apikey", s)

	// No auth → 401
	req := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	rec := httptest.NewRecorder()
	api.WorkersHandler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 without key, got %d", rec.Code)
	}

	// Correct key → 200
	req = httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer apikey")
	rec = httptest.NewRecorder()
	api.WorkersHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200 with correct key, got %d", rec.Code)
	}
}

func TestWorkersHandler_GET_ReturnsValidJSON(t *testing.T) {
	s := openStore(t)
	s.UpsertWorker("fp-1", "pk", "M", "Q4", "0.0.1", "linux", "amd64") //nolint:errcheck
	api := NewSynergiaAPI("apikey", s)

	req := httptest.NewRequest(http.MethodGet, "/v1/workers", nil)
	req.Header.Set("Authorization", "Bearer apikey")
	rec := httptest.NewRecorder()
	api.WorkersHandler(rec, req)

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := resp["workers"]; !ok {
		t.Error("response missing 'workers' field")
	}
}

func TestStatsHandler_GET_ReturnsJSON(t *testing.T) {
	s := openStore(t)
	api := NewSynergiaAPI("apikey", s)

	req := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer apikey")
	rec := httptest.NewRecorder()
	api.StatsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

// ── ConsentAPI ────────────────────────────────────────────────────────────────

func TestConsentHandler_POST_StoresConsent(t *testing.T) {
	s := openStore(t)
	s.UpsertWorker("fp-consent", "pk", "M", "Q4", "0.0.1", "linux", "amd64") //nolint:errcheck
	api := NewConsentAPI(noKey(), s)

	body := `{"fingerprint":"fp-consent","accepted":true,"hardware_stats":true,"config_preferences":true,"hardware":{"os":"linux","os_version":"5.15","gpu":"GTX 4090","vram_mb":24576,"cpu":"Ryzen 9","cpu_cores":16,"ram_mb":65536}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/consent", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	api.ConsentHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !s.HasConsent("fp-consent") {
		t.Error("consent should be stored after POST")
	}
}

func TestConsentHandler_GET_ReturnsConsentState(t *testing.T) {
	s := openStore(t)
	s.UpsertWorker("fp-get", "pk", "M", "Q4", "0.0.1", "linux", "amd64") //nolint:errcheck
	api := NewConsentAPI(noKey(), s)

	req := httptest.NewRequest(http.MethodGet, "/v1/consent?fingerprint=fp-get", nil)
	rec := httptest.NewRecorder()
	api.ConsentHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := m["accepted"]; !ok {
		t.Error("response missing 'accepted' field")
	}
}

func TestConsentHandler_POST_InvalidJSON_Returns400(t *testing.T) {
	s := openStore(t)
	api := NewConsentAPI(noKey(), s)
	req := httptest.NewRequest(http.MethodPost, "/v1/consent", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	api.ConsentHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}
