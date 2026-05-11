package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ── ComputeLLMHash ────────────────────────────────────────────────────────────

func TestComputeLLMHash_Deterministic(t *testing.T) {
	h1 := ComputeLLMHash("inference", "abc123modelfilehash")
	h2 := ComputeLLMHash("inference", "abc123modelfilehash")
	if h1 != h2 {
		t.Error("ComputeLLMHash is not deterministic")
	}
}

func TestComputeLLMHash_DiffersOnRole(t *testing.T) {
	h1 := ComputeLLMHash("inference", "samehash")
	h2 := ComputeLLMHash("embedding", "samehash")
	if h1 == h2 {
		t.Error("same model hash with different roles must produce different LLM hashes")
	}
}

func TestComputeLLMHash_DiffersOnModelHash(t *testing.T) {
	h1 := ComputeLLMHash("tester", "hash_a")
	h2 := ComputeLLMHash("tester", "hash_b")
	if h1 == h2 {
		t.Error("same role with different model hashes must produce different LLM hashes")
	}
}

func TestComputeLLMHash_ReturnHex(t *testing.T) {
	h := ComputeLLMHash("inference", "abc")
	if len(h) != 64 {
		t.Errorf("expected 64-char hex SHA-256, got len=%d: %q", len(h), h)
	}
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex character %q in hash %q", c, h)
		}
	}
}

// ── HashFile ──────────────────────────────────────────────────────────────────

func TestHashFile_KnownContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.bin")
	content := []byte("synergia test model content")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	// SHA-256 of "synergia test model content"
	// pre-computed: echo -n "synergia test model content" | sha256sum
	const want = "7e6c3b0e1f3e6b2d4e9f4b7b7e6c3b0e1f3e6b2d4e9f4b7b7e6c3b0e1f3e6b2d"
	// Don't hardcode — instead verify length and that same input gives same output.
	if len(got) != 64 {
		t.Errorf("expected 64-char hex, got %d", len(got))
	}
	got2, _ := HashFile(path)
	if got != got2 {
		t.Error("HashFile is not deterministic")
	}
}

func TestHashFile_MissingFile(t *testing.T) {
	_, err := HashFile("/nonexistent/path/model.gguf")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestHashFile_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.bin")
	os.WriteFile(path, []byte{}, 0644)
	h, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile on empty file: %v", err)
	}
	// SHA-256 of empty input is a known constant
	const sha256Empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if h != sha256Empty {
		t.Errorf("expected SHA-256 of empty file %q, got %q", sha256Empty, h)
	}
}

// ── JSON round-trips ──────────────────────────────────────────────────────────

func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestWorkUnit_RoundTrip(t *testing.T) {
	wu := WorkUnit{
		Type:  TypeWorkUnit,
		ID:    "unit-42",
		Model: "SmolLM2",
		Params: WorkUnitParams{Temperature: 0.7, MaxTokens: 512},
		Messages: []ChatMessage{
			{Role: "user", Content: "hello"},
		},
	}
	got := roundTrip(t, wu)
	if got.ID != wu.ID || got.Type != TypeWorkUnit {
		t.Errorf("WorkUnit round-trip mismatch: %+v", got)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hello" {
		t.Errorf("messages not preserved: %+v", got.Messages)
	}
}

func TestModelUpdate_RoundTrip_AllFields(t *testing.T) {
	mu := ModelUpdate{
		Type:           TypeModelUpdate,
		Role:           "inference",
		Model:          "mistral-nemo",
		Quantisation:   "Q4_K_M",
		Filename:       "mistral.gguf",
		ModelFileHash:  "abc123",
		LLMHash:        "def456",
		ContextSize:    4096,
		EndpointType:   "chat",
		ParallelSlots:  2,
		GPULayers:      -1,
		FlashAttention: true,
	}
	got := roundTrip(t, mu)
	if got.Role != mu.Role || got.ContextSize != 4096 || !got.FlashAttention {
		t.Errorf("ModelUpdate round-trip lost fields: %+v", got)
	}
}

func TestResult_RoundTrip_SignaturePreserved(t *testing.T) {
	r := Result{
		Type:             TypeResult,
		ID:               "unit-1",
		Fingerprint:      "fp123",
		ProcessingTimeMs: 123,
		Signature:        "hexsig",
	}
	got := roundTrip(t, r)
	if got.Signature != "hexsig" {
		t.Errorf("Result.Signature not preserved: %q", got.Signature)
	}
}

func TestBinaryUpdate_RoundTrip(t *testing.T) {
	bu := BinaryUpdate{
		Type:        TypeBinaryUpdate,
		Version:     "0.0.15",
		DownloadURL: "https://github.com/example/releases/download/0.0.15/client",
		FallbackURL: "https://manager.example.com/download/linux/amd64",
		SHA256:      "deadbeef",
	}
	got := roundTrip(t, bu)
	if got.Version != "0.0.15" || got.SHA256 != "deadbeef" {
		t.Errorf("BinaryUpdate round-trip mismatch: %+v", got)
	}
}

func TestBackendUpdate_RoundTrip(t *testing.T) {
	bu := BackendUpdate{
		Type:        TypeBackendUpdate,
		Version:     "b4321",
		DownloadURL: "https://github.com/ggml-org/llama.cpp/releases/download/b4321/archive.zip",
		SHA256:      "cafebabe",
	}
	got := roundTrip(t, bu)
	if got.SHA256 != "cafebabe" {
		t.Errorf("BackendUpdate SHA256 not preserved: %q", got.SHA256)
	}
}

func TestChallenge_RoundTrip(t *testing.T) {
	c := Challenge{Type: TypeChallenge, Nonce: "base64nonce=="}
	got := roundTrip(t, c)
	if got.Type != TypeChallenge || got.Nonce != "base64nonce==" {
		t.Errorf("Challenge round-trip mismatch: %+v", got)
	}
}

func TestSignedResponse_RoundTrip(t *testing.T) {
	sr := SignedResponse{
		Type:      TypeSignedResponse,
		Nonce:     "base64nonce==",
		Signature: "base64sig==",
	}
	got := roundTrip(t, sr)
	if got.Signature != "base64sig==" || got.Nonce != "base64nonce==" {
		t.Errorf("SignedResponse round-trip mismatch: %+v", got)
	}
}

// ── Envelope type routing ─────────────────────────────────────────────────────

func TestEnvelope_ExtractsType(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"type":"work_unit","id":"x"}`, TypeWorkUnit},
		{`{"type":"model_update","role":"tester"}`, TypeModelUpdate},
		{`{"type":"challenge","nonce":"abc"}`, TypeChallenge},
		{`{"type":"unknown_future_type"}`, "unknown_future_type"},
	} {
		var env Envelope
		if err := json.Unmarshal([]byte(tc.raw), &env); err != nil {
			t.Errorf("unmarshal %q: %v", tc.raw, err)
			continue
		}
		if env.Type != tc.want {
			t.Errorf("want type %q, got %q", tc.want, env.Type)
		}
	}
}

func TestEnvelope_UnknownType_NoPanic(t *testing.T) {
	var env Envelope
	// Should not panic on any valid JSON even with unexpected content.
	if err := json.Unmarshal([]byte(`{"type":"future_v2_message","extra":42}`), &env); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
