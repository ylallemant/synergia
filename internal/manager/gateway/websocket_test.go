package gateway

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ylallemant/synergia/internal/manager/queue"
	"github.com/ylallemant/synergia/internal/manager/store"
	"github.com/ylallemant/synergia/internal/protocol"
)

// newTestGateway creates a Gateway with no store (nil-safe) and a real queue.
func newTestGateway(workerKey string) *Gateway {
	return New(workerKey, queue.New(), nil)
}

// dbCounter gives each test its own named in-memory SQLite database.
// Plain ":memory:" fails under concurrent goroutines because the connection
// pool opens a second connection that sees a fresh empty database.
// A named URI with cache=shared ensures all connections share the same data.
var dbCounter atomic.Int64

// openTestStore opens a per-test named in-memory SQLite store.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	id := dbCounter.Add(1)
	dsn := fmt.Sprintf("file:gw_test_%d?mode=memory&cache=shared", id)
	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("openTestStore: %v", err)
	}
	return s
}

// newTestGatewayWithStore creates a Gateway backed by a real store.
func newTestGatewayWithStore(workerKey string, s *store.Store) *Gateway {
	return New(workerKey, queue.New(), s)
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

// ── TypeStatus GPU avg consent gating ─────────────────────────────────────────

// connectWorkerWithStore connects a worker to a gateway backed by a real store,
// waits for it to register, and returns the client websocket and fingerprint.
func connectWorkerWithStore(t *testing.T, gw *Gateway, srv *httptest.Server) (*websocket.Conn, string) {
	t.Helper()
	_, _, fp, pubB64 := generateWorker(t)
	conn, _, err := dialWorker(t, srv, fp, pubB64, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if !waitForWorker(gw, 2*time.Second) {
		t.Fatal("worker did not connect")
	}
	return conn, fp
}

// waitForGPUAvg polls the store until the worker's GPUAvg matches want or the deadline passes.
func waitForGPUAvg(s *store.Store, fingerprint string, want int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		w, err := s.GetWorker(fingerprint)
		if err == nil && w != nil && w.GPUAvg == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestTypeStatus_GPUAvg_StoredWhenConsented(t *testing.T) {
	s := openTestStore(t)
	gw := newTestGatewayWithStore("secret", s)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	conn, fp := connectWorkerWithStore(t, gw, srv)
	defer conn.Close()

	// Grant consent before the status message arrives.
	if err := s.SetConsent(fp, true, true, true, nil); err != nil {
		t.Fatalf("SetConsent: %v", err)
	}

	if err := conn.WriteJSON(protocol.Status{
		Type:   protocol.TypeStatus,
		State:  "available",
		GPUAvg: 55,
	}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	if !waitForGPUAvg(s, fp, 55, 2*time.Second) {
		w, _ := s.GetWorker(fp)
		got := 0
		if w != nil {
			got = w.GPUAvg
		}
		t.Errorf("want GPUAvg=55 stored, got %d", got)
	}
}

func TestTypeStatus_GPUAvg_NotStoredWithoutConsent(t *testing.T) {
	s := openTestStore(t)
	gw := newTestGatewayWithStore("secret", s)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	conn, fp := connectWorkerWithStore(t, gw, srv)
	defer conn.Close()

	// No consent — GPUAvg must not be written.
	if err := conn.WriteJSON(protocol.Status{
		Type:   protocol.TypeStatus,
		State:  "available",
		GPUAvg: 99,
	}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// Give the gateway time to process and confirm it was NOT stored.
	time.Sleep(100 * time.Millisecond)
	w, err := s.GetWorker(fp)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.GPUAvg != 0 {
		t.Errorf("GPUAvg must not be stored without consent, got %d", w.GPUAvg)
	}
}

func TestTypeStatus_GPUAvg_ZeroNotStored(t *testing.T) {
	s := openTestStore(t)
	gw := newTestGatewayWithStore("secret", s)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	conn, fp := connectWorkerWithStore(t, gw, srv)
	defer conn.Close()

	// Consent given; set a known value first, then send GPUAvg=0.
	s.SetConsent(fp, true, true, true, nil) //nolint:errcheck
	s.SetWorkerGPUAvg(fp, 42)              //nolint:errcheck

	if err := conn.WriteJSON(protocol.Status{
		Type:   protocol.TypeStatus,
		State:  "available",
		GPUAvg: 0, // omitted — worker has no data yet
	}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// GPUAvg=0 must not overwrite the previously stored value.
	time.Sleep(100 * time.Millisecond)
	w, _ := s.GetWorker(fp)
	if w == nil || w.GPUAvg != 42 {
		got := 0
		if w != nil {
			got = w.GPUAvg
		}
		t.Errorf("GPUAvg=0 must not overwrite stored value, got %d", got)
	}
}
