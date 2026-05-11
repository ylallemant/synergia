package gateway

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ylallemant/synergia/internal/manager/queue"
	"github.com/ylallemant/synergia/internal/protocol"
)

// newTestGateway creates a Gateway with no store (nil-safe) and a real queue.
func newTestGateway(workerKey string) *Gateway {
	return New(workerKey, queue.New(), nil)
}

// generateWorker creates a fresh Ed25519 keypair and returns the public key,
// private key, fingerprint (hex SHA256 of pubKey), and base64-encoded pubKey.
func generateWorker(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string, string) {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	hash := sha256.Sum256(pubKey)
	fingerprint := hex.EncodeToString(hash[:])
	pubKeyB64 := base64.StdEncoding.EncodeToString(pubKey)
	return pubKey, privKey, fingerprint, pubKeyB64
}

// workerHeaders builds the minimum identity headers a worker sends on connect.
func workerHeaders(fingerprint, pubKeyB64 string) http.Header {
	h := http.Header{}
	h.Set("X-Worker-Fingerprint", fingerprint)
	h.Set("X-Worker-Public-Key", pubKeyB64)
	h.Set("X-Worker-Model", "SmolLM2")
	h.Set("X-Worker-Quantisation", "Q4_K_M")
	h.Set("X-Worker-Version", "0.0.1-test")
	h.Set("X-Worker-OS", "linux")
	h.Set("X-Worker-Arch", "amd64")
	return h
}

// dialWorker dials the test server with correct identity headers.
// bearer may be "" to omit the Authorization header.
func dialWorker(t *testing.T, srv *httptest.Server, fingerprint, pubKeyB64, bearer string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	headers := workerHeaders(fingerprint, pubKeyB64)
	if bearer != "" {
		headers.Set("Authorization", "Bearer "+bearer)
	}
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return websocket.DefaultDialer.Dial(wsURL, headers)
}

// waitForWorker polls HasWorker until true or deadline.
func waitForWorker(gw *Gateway, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if gw.HasWorker() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// ── key-auth mode ─────────────────────────────────────────────────────────────

func TestServeHTTP_KeyAuth_CorrectBearer_Upgrades(t *testing.T) {
	gw := newTestGateway("secret-key")
	srv := httptest.NewServer(gw)
	defer srv.Close()

	_, _, fp, pubB64 := generateWorker(t)
	conn, _, err := dialWorker(t, srv, fp, pubB64, "secret-key")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if !waitForWorker(gw, 2*time.Second) {
		t.Error("gateway should have a worker after successful key-auth connection")
	}
}

func TestServeHTTP_KeyAuth_WrongBearer_Returns401(t *testing.T) {
	gw := newTestGateway("secret-key")
	srv := httptest.NewServer(gw)
	defer srv.Close()

	_, _, fp, pubB64 := generateWorker(t)
	_, resp, err := dialWorker(t, srv, fp, pubB64, "wrong-key")
	if err == nil {
		t.Fatal("expected dial to fail with 401")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got response: %v", resp)
	}
}

func TestServeHTTP_KeyAuth_MissingBearer_Returns401(t *testing.T) {
	gw := newTestGateway("secret-key")
	srv := httptest.NewServer(gw)
	defer srv.Close()

	_, _, fp, pubB64 := generateWorker(t)
	_, resp, err := dialWorker(t, srv, fp, pubB64, "") // no bearer
	if err == nil {
		t.Fatal("expected dial to fail with 401 when bearer is absent")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got response: %v", resp)
	}
}

// ── TOFU mode ─────────────────────────────────────────────────────────────────

func TestServeHTTP_TOFU_ValidSignature_Upgrades(t *testing.T) {
	gw := newTestGateway("") // empty workerKey = TOFU
	srv := httptest.NewServer(gw)
	defer srv.Close()

	_, privKey, fp, pubB64 := generateWorker(t)
	conn, _, err := dialWorker(t, srv, fp, pubB64, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Gateway sends a challenge immediately after upgrade — respond correctly.
	clientRespond(conn, privKey)

	if !waitForWorker(gw, 2*time.Second) {
		t.Error("gateway should have a worker after successful TOFU handshake")
	}
}

func TestServeHTTP_TOFU_InvalidSignature_ClosesConn(t *testing.T) {
	gw := newTestGateway("") // TOFU mode
	srv := httptest.NewServer(gw)
	defer srv.Close()

	pubKey, _, fp, pubB64 := generateWorker(t)
	_ = pubKey
	_, wrongPrivKey, _, _ := generateWorker(t) // different keypair

	conn, _, err := dialWorker(t, srv, fp, pubB64, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Respond with the wrong private key — gateway rejects the signature.
	clientRespond(conn, wrongPrivKey)

	conn.SetReadDeadline(time.Now().Add(challengeTimeout + time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Error("expected connection to be closed after failed TOFU challenge")
	}
	if gw.HasWorker() {
		t.Error("gateway must not register worker after failed TOFU challenge")
	}
}

// ── header validation ─────────────────────────────────────────────────────────

func TestServeHTTP_MissingFingerprint_Returns400(t *testing.T) {
	gw := newTestGateway("secret")
	srv := httptest.NewServer(gw)
	defer srv.Close()

	_, _, _, pubB64 := generateWorker(t)
	headers := workerHeaders("", pubB64) // empty fingerprint
	headers.Set("Authorization", "Bearer secret")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err == nil {
		t.Fatal("expected dial to fail with 400")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got response: %v", resp)
	}
}

func TestServeHTTP_FingerprintMismatch_Returns400(t *testing.T) {
	gw := newTestGateway("secret")
	srv := httptest.NewServer(gw)
	defer srv.Close()

	_, _, _, pubB64 := generateWorker(t)
	wrongFP := strings.Repeat("a", 64) // valid hex length, but wrong
	headers := workerHeaders(wrongFP, pubB64)
	headers.Set("Authorization", "Bearer secret")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err == nil {
		t.Fatal("expected dial to fail with 400")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got response: %v", resp)
	}
}

// ── single-slot enforcement ───────────────────────────────────────────────────

func TestServeHTTP_SecondWorker_RejectedWithPolicyViolation(t *testing.T) {
	gw := newTestGateway("secret")
	srv := httptest.NewServer(gw)
	defer srv.Close()

	connect := func() *websocket.Conn {
		t.Helper()
		_, _, fp, pubB64 := generateWorker(t)
		conn, _, err := dialWorker(t, srv, fp, pubB64, "secret")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return conn
	}

	first := connect()
	defer first.Close()

	// Wait for the first worker to occupy the slot.
	if !waitForWorker(gw, 2*time.Second) {
		t.Fatal("first worker did not connect")
	}

	second := connect()
	defer second.Close()

	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := second.ReadMessage()
	if err == nil {
		t.Fatal("expected second worker to receive a close message")
	}
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.ClosePolicyViolation {
		t.Errorf("want ClosePolicyViolation, got %v", err)
	}
}

// ── disconnect clears the slot ────────────────────────────────────────────────

func TestServeHTTP_Disconnect_ClearsWorker(t *testing.T) {
	gw := newTestGateway("secret")
	srv := httptest.NewServer(gw)
	defer srv.Close()

	_, _, fp, pubB64 := generateWorker(t)
	conn, _, err := dialWorker(t, srv, fp, pubB64, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	if !waitForWorker(gw, 2*time.Second) {
		t.Fatal("worker did not connect")
	}

	conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !gw.HasWorker() {
			return // pass
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("HasWorker should return false after worker disconnects")
}

// ── push methods ──────────────────────────────────────────────────────────────

func TestPushModelUpdate_NoWorker_ReturnsError(t *testing.T) {
	gw := newTestGateway("secret")
	err := gw.PushModelUpdate("tester", "SmolLM2", "Q4_K_M", "smol.gguf", "hash1", "llmhash1", 2048, 1, -1, "chat", false)
	if err == nil {
		t.Error("PushModelUpdate must return an error when no worker is connected")
	}
}

func TestPushModelUpdate_WithWorker_DeliverMessage(t *testing.T) {
	gw := newTestGateway("secret")
	srv := httptest.NewServer(gw)
	defer srv.Close()

	_, _, fp, pubB64 := generateWorker(t)
	conn, _, err := dialWorker(t, srv, fp, pubB64, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if !waitForWorker(gw, 2*time.Second) {
		t.Fatal("worker did not connect")
	}

	if err := gw.PushModelUpdate("tester", "SmolLM2", "Q4_K_M", "smol.gguf", "hash1", "llmhash1", 2048, 1, -1, "chat", false); err != nil {
		t.Fatalf("PushModelUpdate: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read model update: %v", err)
	}
	var mu protocol.ModelUpdate
	if err := json.Unmarshal(raw, &mu); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mu.Type != protocol.TypeModelUpdate || mu.Role != "tester" || mu.Model != "SmolLM2" {
		t.Errorf("unexpected ModelUpdate: %+v", mu)
	}
}

// ── SetWorkerKey live change ──────────────────────────────────────────────────

func TestSetWorkerKey_LiveChange_ReflectedImmediately(t *testing.T) {
	gw := newTestGateway("original-key")

	// Verify initial value.
	if got := gw.WorkerKey(); got != "original-key" {
		t.Errorf("want original-key, got %q", got)
	}

	gw.SetWorkerKey("new-key")
	if got := gw.WorkerKey(); got != "new-key" {
		t.Errorf("want new-key after SetWorkerKey, got %q", got)
	}

	gw.SetWorkerKey("")
	if got := gw.WorkerKey(); got != "" {
		t.Errorf("want empty string (TOFU mode) after SetWorkerKey(\"\"), got %q", got)
	}
}
