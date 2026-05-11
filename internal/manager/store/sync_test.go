package store

import (
	"testing"

	"github.com/ylallemant/synergia/internal/protocol"
)

// openTestStore opens a fully-migrated in-memory SQLite store for testing.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("openTestStore: %v", err)
	}
	return s
}

// seedWorker inserts a minimal worker record and optionally sets its LLM hash.
func seedWorker(t *testing.T, s *Store, fingerprint, llmHash string) {
	t.Helper()
	if err := s.UpsertWorker(fingerprint, "pubkey", "TestModel", "Q4", "0.0.1", "linux", "amd64"); err != nil {
		t.Fatalf("UpsertWorker: %v", err)
	}
	if llmHash != "" {
		if err := s.SetWorkerLLMHash(fingerprint, llmHash); err != nil {
			t.Fatalf("SetWorkerLLMHash: %v", err)
		}
	}
}

// seedRole inserts a role-model mapping with the given model file hash (may be empty).
func seedRole(t *testing.T, s *Store, role, modelFileHash string) {
	t.Helper()
	if err := s.UpsertRoleModel(role, "TestModel", "Q4", "model.gguf", modelFileHash, 512, ""); err != nil {
		t.Fatalf("UpsertRoleModel: %v", err)
	}
}

// TestUpdateWorkerSyncStatus_NoRoleHash is the primary regression test for the
// fix: when a role has no model_file_hash configured (e.g. the tester role or a
// role whose model files are not on the server), workers in that role must be
// treated as synced regardless of their own llm_hash. Before the fix they were
// permanently stuck as out-of-sync → unavailable.
func TestUpdateWorkerSyncStatus_NoRoleHash(t *testing.T) {
	fp := "fingerprint-tofu"

	t.Run("empty llm_hash with unconfigured role hash", func(t *testing.T) {
		s := openTestStore(t)
		seedRole(t, s, "tester", "") // no model hash — nothing to enforce
		seedWorker(t, s, fp, "")     // no running LLM

		// worker preferred role must be set so the lookup finds the right role
		if err := s.SetWorkerConfig(fp, "tester", ""); err != nil {
			t.Fatalf("SetWorkerConfig: %v", err)
		}

		got := s.UpdateWorkerSyncStatus(fp)
		if got != "synced" {
			t.Errorf("want synced (no role hash = nothing to enforce), got %q", got)
		}
	})

	t.Run("non-empty llm_hash with unconfigured role hash", func(t *testing.T) {
		s := openTestStore(t)
		seedRole(t, s, "tester", "")
		seedWorker(t, s, fp, "some-hash-from-worker")
		if err := s.SetWorkerConfig(fp, "tester", ""); err != nil {
			t.Fatalf("SetWorkerConfig: %v", err)
		}

		got := s.UpdateWorkerSyncStatus(fp)
		if got != "synced" {
			t.Errorf("want synced (no role hash = nothing to enforce), got %q", got)
		}
	})
}

// TestUpdateWorkerSyncStatus_WithRoleHash covers the existing hash-comparison
// logic: when the role does have a model_file_hash the worker must report a
// matching llm_hash to be considered synced.
func TestUpdateWorkerSyncStatus_WithRoleHash(t *testing.T) {
	fp := "fingerprint-hash"
	const roleModelHash = "abc123modelfilehash"
	expectedLLMHash := protocol.ComputeLLMHash("inference", roleModelHash)

	t.Run("matching llm_hash → synced", func(t *testing.T) {
		s := openTestStore(t)
		seedRole(t, s, "inference", roleModelHash)
		seedWorker(t, s, fp, expectedLLMHash)
		if err := s.SetWorkerConfig(fp, "inference", ""); err != nil {
			t.Fatalf("SetWorkerConfig: %v", err)
		}

		got := s.UpdateWorkerSyncStatus(fp)
		if got != "synced" {
			t.Errorf("want synced (matching hash), got %q", got)
		}
	})

	t.Run("empty llm_hash → out-of-sync", func(t *testing.T) {
		s := openTestStore(t)
		seedRole(t, s, "inference", roleModelHash)
		seedWorker(t, s, fp, "") // no running LLM
		if err := s.SetWorkerConfig(fp, "inference", ""); err != nil {
			t.Fatalf("SetWorkerConfig: %v", err)
		}

		got := s.UpdateWorkerSyncStatus(fp)
		if got != "out-of-sync" {
			t.Errorf("want out-of-sync (empty llm_hash, role has hash), got %q", got)
		}
	})

	t.Run("wrong llm_hash → out-of-sync", func(t *testing.T) {
		s := openTestStore(t)
		seedRole(t, s, "inference", roleModelHash)
		seedWorker(t, s, fp, "completely-wrong-hash")
		if err := s.SetWorkerConfig(fp, "inference", ""); err != nil {
			t.Fatalf("SetWorkerConfig: %v", err)
		}

		got := s.UpdateWorkerSyncStatus(fp)
		if got != "out-of-sync" {
			t.Errorf("want out-of-sync (wrong hash), got %q", got)
		}
	})
}
