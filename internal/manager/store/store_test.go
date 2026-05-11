package store

import (
	"testing"
	"time"
)

// ── Worker lifecycle ──────────────────────────────────────────────────────────

func TestUpsertWorker_CreatesAndUpdates(t *testing.T) {
	s := openTestStore(t)
	fp := "fp-upsert-1"

	if err := s.UpsertWorker(fp, "pubkey", "M", "Q4", "0.0.1", "linux", "amd64"); err != nil {
		t.Fatalf("UpsertWorker (create): %v", err)
	}
	w, err := s.GetWorker(fp)
	if err != nil || w == nil {
		t.Fatalf("GetWorker after create: %v", err)
	}
	if w.LLMModel != "M" || w.Arch != "amd64" {
		t.Errorf("unexpected worker fields: %+v", w)
	}

	// Update
	if err := s.UpsertWorker(fp, "pubkey", "M2", "Q8", "0.0.2", "darwin", "arm64"); err != nil {
		t.Fatalf("UpsertWorker (update): %v", err)
	}
	w2, _ := s.GetWorker(fp)
	if w2.LLMModel != "M2" || w2.ClientVersion != "0.0.2" {
		t.Errorf("update not reflected: %+v", w2)
	}
}

func TestGetWorker_UnknownFingerprint_ReturnsError(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetWorker("nonexistent-fingerprint")
	if err == nil {
		t.Error("expected error for unknown fingerprint, got nil")
	}
}

func TestSetWorkerOnlineIfAllowed_SetsOnline(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-online", "")
	s.SetWorkerOnlineIfAllowed("fp-online")
	w, _ := s.GetWorker("fp-online")
	if w.Status != "online" {
		t.Errorf("want status=online, got %q", w.Status)
	}
}

func TestSetWorkerOffline_SetsOffline(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-offline", "")
	s.SetWorkerOnlineIfAllowed("fp-offline")
	if err := s.SetWorkerOffline("fp-offline"); err != nil {
		t.Fatalf("SetWorkerOffline: %v", err)
	}
	w, _ := s.GetWorker("fp-offline")
	if w.Status != "offline" {
		t.Errorf("want status=offline, got %q", w.Status)
	}
}

func TestSetWorkerDeleted_SetsDeleted(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-del", "")
	if err := s.SetWorkerDeleted("fp-del"); err != nil {
		t.Fatalf("SetWorkerDeleted: %v", err)
	}
	w, _ := s.GetWorker("fp-del")
	if w.Status != "deleted" {
		t.Errorf("want status=deleted, got %q", w.Status)
	}
}

func TestSetWorkerOffline_DoesNotOverwriteDeleted(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-del2", "")
	s.SetWorkerDeleted("fp-del2") //nolint:errcheck
	s.SetWorkerOffline("fp-del2") //nolint:errcheck
	w, _ := s.GetWorker("fp-del2")
	if w.Status != "deleted" {
		t.Errorf("deleted status must not be overwritten by offline; got %q", w.Status)
	}
}

func TestSetWorkerAvailableIfProcessing_OnlyWhenProcessing(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-proc", "")
	s.SetWorkerStatus("fp-proc", "processing") //nolint:errcheck
	s.SetWorkerAvailableIfProcessing("fp-proc")
	w, _ := s.GetWorker("fp-proc")
	if w.Status != "available" {
		t.Errorf("want available after SetWorkerAvailableIfProcessing, got %q", w.Status)
	}
}

func TestSetWorkerAvailableIfProcessing_DoesNotChangeOtherStatuses(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-paused", "")
	s.SetWorkerStatus("fp-paused", "paused") //nolint:errcheck
	s.SetWorkerAvailableIfProcessing("fp-paused")
	w, _ := s.GetWorker("fp-paused")
	if w.Status != "paused" {
		t.Errorf("paused status should not change, got %q", w.Status)
	}
}

func TestIsWorkerAvailable_TrueWhenAvailableAndSynced(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-avail", "")
	s.SetWorkerStatus("fp-avail", "available")       //nolint:errcheck
	s.DB.Model(&Worker{}).Where("fingerprint = ?", "fp-avail").Update("sync_status", "synced")
	if !s.IsWorkerAvailable("fp-avail") {
		t.Error("want IsWorkerAvailable=true for available+synced worker")
	}
}

func TestIsWorkerAvailable_FalseWhenOutOfSync(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-oos", "")
	s.SetWorkerStatus("fp-oos", "available") //nolint:errcheck
	// sync_status defaults to out-of-sync
	if s.IsWorkerAvailable("fp-oos") {
		t.Error("want IsWorkerAvailable=false when out-of-sync")
	}
}

// ── Consent ───────────────────────────────────────────────────────────────────

func TestSetConsent_Accept_PersistsAndHasConsent(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-consent", "")
	if err := s.SetConsent("fp-consent", true, true, true, nil); err != nil {
		t.Fatalf("SetConsent: %v", err)
	}
	if !s.HasConsent("fp-consent") {
		t.Error("HasConsent should return true after accepting")
	}
}

func TestSetConsent_Revoke_ClearsConsent(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-revoke", "")
	s.SetConsent("fp-revoke", true, true, true, nil)   //nolint:errcheck
	s.SetConsent("fp-revoke", false, false, false, nil) //nolint:errcheck
	if s.HasConsent("fp-revoke") {
		t.Error("HasConsent should return false after revoking")
	}
}

func TestHasConsent_NoRecord_ReturnsFalse(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-noconsent", "")
	if s.HasConsent("fp-noconsent") {
		t.Error("HasConsent should return false when no consent record exists")
	}
}

// ── Worker config ─────────────────────────────────────────────────────────────

func TestSetAndGetWorkerConfig(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-cfg", "")

	if err := s.SetWorkerConfig("fp-cfg", "inference", "TestNode"); err != nil {
		t.Fatalf("SetWorkerConfig: %v", err)
	}
	cfg, err := s.GetWorkerConfig("fp-cfg")
	if err != nil || cfg == nil {
		t.Fatalf("GetWorkerConfig: %v", err)
	}
	if cfg.PreferredRole != "inference" || cfg.Nickname != "TestNode" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestGetWorkerConfig_NoRecord_ReturnsNil(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-nocfg", "")
	cfg, err := s.GetWorkerConfig("fp-nocfg")
	if err != nil {
		t.Fatalf("GetWorkerConfig: %v", err)
	}
	if cfg != nil {
		t.Error("expected nil config when no record exists")
	}
}

// ── Role models ───────────────────────────────────────────────────────────────

func TestUpsertRoleModel_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertRoleModel("tester", "SmolLM2", "Q4_K_M", "smol.gguf", "hash1", 512, "test role"); err != nil {
		t.Fatalf("UpsertRoleModel: %v", err)
	}
	roles, err := s.GetRoleModels()
	if err != nil {
		t.Fatalf("GetRoleModels: %v", err)
	}
	var found bool
	for _, r := range roles {
		if r.Role == "tester" && r.LLMModel == "SmolLM2" {
			found = true
		}
	}
	if !found {
		t.Errorf("upserted role not found in GetRoleModels; got %+v", roles)
	}
}

func TestUpsertRoleModel_Update(t *testing.T) {
	s := openTestStore(t)
	s.UpsertRoleModel("tester", "SmolLM2", "Q4_K_M", "smol.gguf", "hash1", 512, "v1") //nolint:errcheck
	s.UpsertRoleModel("tester", "SmolLM2", "Q4_K_M", "smol.gguf", "hash2", 1024, "v2") //nolint:errcheck
	rm, err := s.GetRoleModel("tester")
	if err != nil || rm == nil {
		t.Fatalf("GetRoleModel: %v", err)
	}
	if rm.ModelFileHash != "hash2" || rm.MinVRAMMB != 1024 {
		t.Errorf("update not reflected: %+v", rm)
	}
}

func TestDeleteRoleModel(t *testing.T) {
	s := openTestStore(t)
	s.UpsertRoleModel("delete-me", "SmolLM2", "Q4", "smol.gguf", "h", 512, "") //nolint:errcheck
	if err := s.DeleteRoleModel("delete-me"); err != nil {
		t.Fatalf("DeleteRoleModel: %v", err)
	}
	roles, _ := s.GetRoleModels()
	for _, r := range roles {
		if r.Role == "delete-me" {
			t.Error("role should have been deleted")
		}
	}
}

// ── Client version config ─────────────────────────────────────────────────────

func TestSetAndGetClientVersionConfig(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetClientVersionConfig("0.1.0", "all", 100); err != nil {
		t.Fatalf("SetClientVersionConfig: %v", err)
	}
	got, err := s.GetClientVersionConfig()
	if err != nil || got == nil {
		t.Fatalf("GetClientVersionConfig: %v", err)
	}
	if got.TargetVersion != "0.1.0" || got.RolloutMode != "all" {
		t.Errorf("unexpected version config: %+v", got)
	}
}

func TestGetClientVersionConfig_NoRecord_ReturnsError(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetClientVersionConfig()
	if err == nil {
		t.Error("expected error when no version config record exists, got nil")
	}
}

// ── Branding ──────────────────────────────────────────────────────────────────

func TestSetAndGetBrandingCSS(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetBrandingCSS("body { color: red; }"); err != nil {
		t.Fatalf("SetBrandingCSS: %v", err)
	}
	css, err := s.GetBrandingCSS()
	if err != nil {
		t.Fatalf("GetBrandingCSS: %v", err)
	}
	if css != "body { color: red; }" {
		t.Errorf("want branding CSS, got %q", css)
	}
}

// ── Work units ────────────────────────────────────────────────────────────────

func TestRecordAndCompleteWorkUnit(t *testing.T) {
	s := openTestStore(t)
	fp := "fp-wu"
	seedWorker(t, s, fp, "")

	if err := s.RecordWorkUnit("wu-1", fp, "SmolLM2"); err != nil {
		t.Fatalf("RecordWorkUnit: %v", err)
	}
	if err := s.CompleteWorkUnit("wu-1", 250); err != nil {
		t.Fatalf("CompleteWorkUnit: %v", err)
	}
}

func TestFailWorkUnit(t *testing.T) {
	s := openTestStore(t)
	seedWorker(t, s, "fp-fail-wu", "")
	s.RecordWorkUnit("wu-fail", "fp-fail-wu", "SmolLM2") //nolint:errcheck
	if err := s.FailWorkUnit("wu-fail", "inference error"); err != nil {
		t.Fatalf("FailWorkUnit: %v", err)
	}
}

// ── Client errors ─────────────────────────────────────────────────────────────

func TestCreateAndGetClientErrors(t *testing.T) {
	s := openTestStore(t)
	fp := "fp-err"
	seedWorker(t, s, fp, "")

	if err := s.CreateClientError(fp, "0.1.0", "something broke", "", time.Now()); err != nil {
		t.Fatalf("CreateClientError: %v", err)
	}
	errs, err := s.GetClientErrors()
	if err != nil {
		t.Fatalf("GetClientErrors: %v", err)
	}
	if len(errs) == 0 {
		t.Error("expected at least one error entry")
	}
	if errs[0].ErrorMessage != "something broke" {
		t.Errorf("unexpected error message: %q", errs[0].ErrorMessage)
	}
}
