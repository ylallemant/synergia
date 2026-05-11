package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ylallemant/synergia/internal/manager/api"
)

// ── stub ──────────────────────────────────────────────────────────────────────

type stubKeyUpdater struct{ key string }

func (s *stubKeyUpdater) SetWorkerKey(key string) { s.key = key }

// ── AdminRolesAPI ─────────────────────────────────────────────────────────────

func TestAdminRolesHandler_GET_EmptyList(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminRolesAPI(s)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	rec := httptest.NewRecorder()
	a.AdminRolesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var roles []any
	if err := json.NewDecoder(rec.Body).Decode(&roles); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestAdminRolesHandler_POST_CreatesRole(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminRolesAPI(s)

	body := `{"role":"tester","model":"SmolLM2","quantisation":"Q4_K_M","filename":"smol.gguf","min_vram_mb":512}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.AdminRolesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET should now return the new role
	req2 := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	rec2 := httptest.NewRecorder()
	a.AdminRolesHandler(rec2, req2)
	var roles []map[string]any
	json.NewDecoder(rec2.Body).Decode(&roles) //nolint:errcheck
	if len(roles) == 0 {
		t.Error("expected at least one role after POST")
	}
}

func TestAdminRolesHandler_POST_MissingRequired_Returns400(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminRolesAPI(s)

	// missing min_vram_mb
	body := `{"role":"tester","model":"SmolLM2"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.AdminRolesHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestAdminRolesHandler_POST_InvalidJSON_Returns400(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminRolesAPI(s)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	a.AdminRolesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestAdminRolesHandler_DELETE_RemovesRole(t *testing.T) {
	s := openTestStore(t)
	s.UpsertRoleModel("to-delete", "SmolLM2", "Q4", "smol.gguf", "h", 512, "") //nolint:errcheck
	a := NewAdminRolesAPI(s)

	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/roles?role=to-delete", nil)
	rec := httptest.NewRecorder()
	a.AdminRolesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	roles, _ := s.GetRoleModels()
	for _, r := range roles {
		if r.Role == "to-delete" {
			t.Error("role should have been deleted")
		}
	}
}

func TestAdminRolesHandler_DELETE_MissingRoleParam_Returns400(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminRolesAPI(s)
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/roles", nil)
	rec := httptest.NewRecorder()
	a.AdminRolesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestAdminRolesHandler_PATCH_Returns405(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminRolesAPI(s)
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/roles", nil)
	rec := httptest.NewRecorder()
	a.AdminRolesHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// ── AdminWorkersAPI ───────────────────────────────────────────────────────────

func TestWorkersConfigHandler_GET_NoConfig_ReturnsNotConfigured(t *testing.T) {
	s := openTestStore(t)
	gw := &stubKeyUpdater{}
	a := NewAdminWorkersAPI(s, gw)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/workers/config", nil)
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp workerAuthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.IsConfigured {
		t.Error("want is_configured=false when no config exists")
	}
}

func TestWorkersConfigHandler_PUT_SavesConfig(t *testing.T) {
	s := openTestStore(t)
	gw := &stubKeyUpdater{}
	a := NewAdminWorkersAPI(s, gw)

	body := `{"tofu_enabled":false,"worker_key":"mykey"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/workers/config", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gw.key != "mykey" {
		t.Errorf("want gateway key 'mykey', got %q", gw.key)
	}
}

func TestWorkersConfigHandler_PUT_TOFUMode_ClearsGatewayKey(t *testing.T) {
	s := openTestStore(t)
	gw := &stubKeyUpdater{key: "existing"}
	a := NewAdminWorkersAPI(s, gw)

	body := `{"tofu_enabled":true,"worker_key":""}`
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/workers/config", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if gw.key != "" {
		t.Errorf("want empty gateway key in TOFU mode, got %q", gw.key)
	}
}

func TestWorkersConfigHandler_GET_MasksStoredKey(t *testing.T) {
	s := openTestStore(t)
	s.SetWorkerAuthConfig(false, "secretkey") //nolint:errcheck
	gw := &stubKeyUpdater{}
	a := NewAdminWorkersAPI(s, gw)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/workers/config", nil)
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)

	var resp workerAuthResponse
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.WorkerKey == "secretkey" {
		t.Error("worker_key should be masked in GET response")
	}
	if resp.WorkerKey != workerKeyMask {
		t.Errorf("want mask %q, got %q", workerKeyMask, resp.WorkerKey)
	}
}

func TestWorkersConfigHandler_DELETE_Returns405(t *testing.T) {
	s := openTestStore(t)
	gw := &stubKeyUpdater{}
	a := NewAdminWorkersAPI(s, gw)
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/workers/config", nil)
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// ── AdminVersionAPI ───────────────────────────────────────────────────────────

func TestAdminVersionHandler_GET_NoConfig_Returns404(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminVersionAPI(s, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/version", nil)
	rec := httptest.NewRecorder()
	a.AdminVersionHandler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404 when no version config, got %d", rec.Code)
	}
}

func TestAdminVersionHandler_POST_SavesConfig(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminVersionAPI(s, nil, nil)

	body := `{"target_version":"0.2.0","rollout_mode":"all"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/version", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.AdminVersionHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET should now return the saved version
	req2 := httptest.NewRequest(http.MethodGet, "/v1/admin/version", nil)
	rec2 := httptest.NewRecorder()
	a.AdminVersionHandler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("want 200 after save, got %d", rec2.Code)
	}
	var resp versionConfigResponse
	json.NewDecoder(rec2.Body).Decode(&resp) //nolint:errcheck
	if resp.TargetVersion != "0.2.0" {
		t.Errorf("want target_version=0.2.0, got %q", resp.TargetVersion)
	}
}

func TestAdminVersionHandler_POST_MissingVersion_Returns400(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminVersionAPI(s, nil, nil)

	body := `{"rollout_mode":"all"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/version", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.AdminVersionHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 when target_version missing, got %d", rec.Code)
	}
}

func TestAdminVersionHandler_POST_InvalidRolloutMode_Returns400(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminVersionAPI(s, nil, nil)

	body := `{"target_version":"0.2.0","rollout_mode":"unknown"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/version", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.AdminVersionHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for invalid rollout_mode, got %d", rec.Code)
	}
}

func TestAdminVersionHandler_PATCH_Returns405(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminVersionAPI(s, nil, nil)
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/version", nil)
	rec := httptest.NewRecorder()
	a.AdminVersionHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// ── AdminOIDCAPI ──────────────────────────────────────────────────────────────

func TestOIDCConfigHandler_GET_NoConfig_ReturnsNotConfigured(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminOIDCAPI(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/oidc", nil)
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp oidcConfigResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.IsConfigured {
		t.Error("want is_configured=false when no OIDC config exists")
	}
}

func TestOIDCConfigHandler_PUT_SavesConfig(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminOIDCAPI(s)

	body := `{"enabled":true,"client_id":"myapp","client_secret":"secret","provider_url":"https://id.example.com","redirect_url":"https://synergia.example.com/callback"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/oidc", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET should now show configured
	req2 := httptest.NewRequest(http.MethodGet, "/v1/admin/oidc", nil)
	rec2 := httptest.NewRecorder()
	a.ConfigHandler(rec2, req2)
	var resp oidcConfigResponse
	json.NewDecoder(rec2.Body).Decode(&resp) //nolint:errcheck
	if !resp.IsConfigured {
		t.Error("want is_configured=true after PUT")
	}
	if resp.ClientID != "myapp" {
		t.Errorf("want client_id=myapp, got %q", resp.ClientID)
	}
}

func TestOIDCConfigHandler_GET_MasksClientSecret(t *testing.T) {
	s := openTestStore(t)
	s.SetOIDCConfig(true, "id", "topsecret", "https://p.example.com", "https://r.example.com") //nolint:errcheck
	a := NewAdminOIDCAPI(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/oidc", nil)
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)

	var resp oidcConfigResponse
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.ClientSecret == "topsecret" {
		t.Error("client_secret should be masked in GET response")
	}
	if resp.ClientSecret != secretMask {
		t.Errorf("want mask %q, got %q", secretMask, resp.ClientSecret)
	}
}

func TestOIDCConfigHandler_PUT_InvalidJSON_Returns400(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminOIDCAPI(s)
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/oidc", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestOIDCConfigHandler_POST_Returns405(t *testing.T) {
	s := openTestStore(t)
	a := NewAdminOIDCAPI(s)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/oidc", nil)
	rec := httptest.NewRecorder()
	a.ConfigHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// ── AdminBrandingAPI ──────────────────────────────────────────────────────────

func TestAdminBrandingHandler_PUT_UpdatesCSS(t *testing.T) {
	s := openTestStore(t)
	branding := api.NewBrandingAPI(func() string { return "" }, s)
	a := NewAdminBrandingAPI(branding)

	css := "body { color: blue; }"
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/branding/css", bytes.NewBufferString(css))
	rec := httptest.NewRecorder()
	a.AdminUpdateHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := branding.GetCSS(); got != css {
		t.Errorf("want CSS %q, got %q", css, got)
	}
}

func TestAdminBrandingHandler_GET_Returns405(t *testing.T) {
	s := openTestStore(t)
	branding := api.NewBrandingAPI(func() string { return "" }, s)
	a := NewAdminBrandingAPI(branding)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/branding/css", nil)
	rec := httptest.NewRecorder()
	a.AdminUpdateHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}
