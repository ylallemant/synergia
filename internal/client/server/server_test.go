package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/ylallemant/synergia/internal/logbuffer"
)

// newTestServer builds a minimal Server suitable for unit tests.
// The uninstallFn is pre-set to a no-op so os.Exit is never called.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	buf := logbuffer.New(50)
	s := &Server{
		logBuf:      buf,
		logFilePath: "",
		dataDir:     t.TempDir(),
	}
	// Prevent doUninstall (which calls os.Exit) from running in tests.
	s.uninstallFn = func() {}
	return s
}

// ── handleLogLevel ────────────────────────────────────────────────────────────

func TestHandleLogLevel_GET(t *testing.T) {
	s := newTestServer(t)
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	req := httptest.NewRequest(http.MethodGet, "/api/log-level", nil)
	rec := httptest.NewRecorder()
	s.handleLogLevel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var body map[string]string
	json.NewDecoder(rec.Body).Decode(&body)
	if body["level"] != "info" {
		t.Errorf("want level=info, got %q", body["level"])
	}
}

func TestHandleLogLevel_POST_ChangesLevel(t *testing.T) {
	s := newTestServer(t)
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(zerolog.InfoLevel) })

	body := `{"level":"debug"}`
	req := httptest.NewRequest(http.MethodPost, "/api/log-level", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleLogLevel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if zerolog.GlobalLevel() != zerolog.DebugLevel {
		t.Errorf("global level not changed: got %v", zerolog.GlobalLevel())
	}
}

func TestHandleLogLevel_POST_InvalidLevel(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/log-level",
		strings.NewReader(`{"level":"notaLevel"}`))
	rec := httptest.NewRecorder()
	s.handleLogLevel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for unknown level, got %d", rec.Code)
	}
}

// ── handleUninstall ───────────────────────────────────────────────────────────

func TestHandleUninstall_AlwaysReturns200(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/uninstall", nil)
	rec := httptest.NewRecorder()
	s.handleUninstall(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestHandleUninstall_InvokesGoodbyeThenUninstall(t *testing.T) {
	s := newTestServer(t)

	calls := make([]string, 0, 2)
	s.onGoodbye = func() { calls = append(calls, "goodbye") }
	s.uninstallFn = func() { calls = append(calls, "uninstall") }

	req := httptest.NewRequest(http.MethodPost, "/api/uninstall", nil)
	rec := httptest.NewRecorder()
	s.handleUninstall(rec, req)

	// Wait for the async goroutine to complete.
	time.Sleep(500 * time.Millisecond)

	if len(calls) != 2 {
		t.Fatalf("want 2 calls (goodbye + uninstall), got %v", calls)
	}
	if calls[0] != "goodbye" {
		t.Errorf("goodbye should be called first, got %v", calls)
	}
	if calls[1] != "uninstall" {
		t.Errorf("uninstall should be called second, got %v", calls)
	}
}

// ── BuildGoodbyeBody ─────────────────────────────────────────────────────────

func TestBuildGoodbyeBody_SignatureVerifiable(t *testing.T) {
	// Generate a real Ed25519 keypair — no mocking, tests the actual crypto path.
	pubKeyRaw, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	fingerprint := "test-fingerprint-abc123"
	signFn := func(data []byte) string {
		return hex.EncodeToString(ed25519.Sign(privKey, data))
	}
	now := time.Now()

	raw, err := BuildGoodbyeBody(fingerprint, signFn, now)
	if err != nil {
		t.Fatalf("BuildGoodbyeBody: %v", err)
	}

	var req struct {
		Fingerprint string `json:"fingerprint"`
		Payload     string `json:"payload"`
		Signature   string `json:"signature"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Payload must have the expected shape.
	parts := strings.SplitN(req.Payload, ":", 3)
	if len(parts) != 3 || parts[0] != "goodbye" || parts[1] != fingerprint {
		t.Errorf("unexpected payload format: %q", req.Payload)
	}

	// Signature must verify with the public key.
	sigBytes, err := hex.DecodeString(req.Signature)
	if err != nil {
		t.Fatalf("hex decode signature: %v", err)
	}
	if !ed25519.Verify(pubKeyRaw, []byte(req.Payload), sigBytes) {
		t.Error("signature does not verify — BuildGoodbyeBody or signing is broken")
	}
	_ = base64.StdEncoding.EncodeToString(pubKeyRaw) // suppress unused import
	_ = bytes.NewReader(raw)
}

func TestBuildGoodbyeBody_TimestampInPayload(t *testing.T) {
	signFn := func(data []byte) string { return "fakesig" }
	ts := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

	raw, _ := BuildGoodbyeBody("fp", signFn, ts)
	var req map[string]string
	json.Unmarshal(raw, &req)

	if !strings.Contains(req["payload"], "2026-05-11T12:00:00Z") {
		t.Errorf("expected RFC3339 UTC timestamp in payload, got %q", req["payload"])
	}
}
