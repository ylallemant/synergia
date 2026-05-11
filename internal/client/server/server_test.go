package server

import (
	"bytes"
	"context"
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
	"github.com/ylallemant/synergia/internal/client/consent"
	"github.com/ylallemant/synergia/internal/client/gpu"
	"github.com/ylallemant/synergia/internal/client/workerconfig"
	"github.com/ylallemant/synergia/internal/logbuffer"
)

// stubStatus satisfies StatusProvider with fixed zero/false values.
type stubStatus struct{}

func (s *stubStatus) IsConnected() bool             { return true }
func (s *stubStatus) IsProcessing() bool            { return false }
func (s *stubStatus) IsPaused() bool                { return false }
func (s *stubStatus) GPUState() gpu.State           { return gpu.StateAvailable }
func (s *stubStatus) GPUUtilization() int           { return 0 }
func (s *stubStatus) GPUSentAvg() int               { return 0 }
func (s *stubStatus) GPUStats() gpu.GPUStats        { return gpu.GPUStats{} }
func (s *stubStatus) GPUSupported() (bool, string)  { return true, "" }
func (s *stubStatus) GPUDriverInfo() (string, string) { return "metal", "3.0" }
func (s *stubStatus) LLMReachable() (bool, string)  { return true, "" }
func (s *stubStatus) Fingerprint() string           { return "fp-test" }
func (s *stubStatus) Model() string                 { return "SmolLM2" }
func (s *stubStatus) Quantisation() string          { return "Q4_K_M" }
func (s *stubStatus) UnitsProcessed() int64         { return 0 }
func (s *stubStatus) Uptime() time.Duration         { return time.Second }

// newTestServerFull builds a Server with a status provider, consent manager,
// and workerconfig manager — required by buildStatusPayload.
func newTestServerFull(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	buf := logbuffer.New(50)
	s := &Server{
		logBuf:      buf,
		logFilePath: "",
		dataDir:     dir,
		status:      &stubStatus{},
		consent:     consent.New(dir, "", "", "fp-test", false),
		config:      workerconfig.New(dir, "", "", "fp-test"),
		statusSubs:  make(map[int]chan []byte),
	}
	s.uninstallFn = func() {}
	return s
}

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

// ── buildStatusPayload ────────────────────────────────────────────────────────

func TestBuildStatusPayload_ValidJSON(t *testing.T) {
	s := newTestServerFull(t)
	payload := s.buildStatusPayload()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("buildStatusPayload returned invalid JSON: %v\nbody: %s", err, payload)
	}
	if _, ok := m["connected"]; !ok {
		t.Error("payload missing 'connected' field")
	}
}

// ── subscriber management + BroadcastStatus ───────────────────────────────────

func TestSubscribeStatus_ReceivesBroadcast(t *testing.T) {
	s := newTestServerFull(t)

	id, ch := s.subscribeStatus()
	defer s.unsubscribeStatus(id)

	s.BroadcastStatus("ready", "disconnected")

	select {
	case payload := <-ch:
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("broadcast payload is not valid JSON: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: subscriber did not receive broadcast")
	}
}

func TestUnsubscribeStatus_ClosesChannel(t *testing.T) {
	s := newTestServerFull(t)
	id, ch := s.subscribeStatus()
	s.unsubscribeStatus(id)

	_, open := <-ch
	if open {
		t.Error("channel should be closed after unsubscribe")
	}
}

func TestBroadcastStatus_MultipleSubscribers(t *testing.T) {
	s := newTestServerFull(t)

	id1, ch1 := s.subscribeStatus()
	id2, ch2 := s.subscribeStatus()
	defer s.unsubscribeStatus(id1)
	defer s.unsubscribeStatus(id2)

	s.BroadcastStatus("ready", "processing")

	for i, ch := range []<-chan []byte{ch1, ch2} {
		select {
		case payload := <-ch:
			if len(payload) == 0 {
				t.Errorf("subscriber %d received empty payload", i+1)
			}
		case <-time.After(time.Second):
			t.Errorf("timeout: subscriber %d did not receive broadcast", i+1)
		}
	}
}

func TestBroadcastStatus_NoSubscribers_NoPanic(t *testing.T) {
	s := newTestServerFull(t)
	// Should not panic with zero subscribers.
	s.BroadcastStatus("ready", "disconnected")
}

// ── handleStatusEvents SSE ────────────────────────────────────────────────────

func TestHandleStatusEvents_SendsCurrentStatusOnConnect(t *testing.T) {
	s := newTestServerFull(t)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/status-events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.handleStatusEvents(rec, req)
		close(done)
	}()

	// Allow the handler to write the initial snapshot and flush it.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("expected SSE data prefix, got: %q", body)
	}
	dataLine := strings.TrimPrefix(strings.SplitN(body, "\n", 2)[0], "data: ")
	var m map[string]any
	if err := json.Unmarshal([]byte(dataLine), &m); err != nil {
		t.Fatalf("initial SSE payload is not valid JSON: %v\nraw: %q", err, dataLine)
	}
	if _, ok := m["connected"]; !ok {
		t.Error("initial SSE payload missing 'connected' field")
	}
}

func TestHandleStatusEvents_StreamsBroadcastedChange(t *testing.T) {
	s := newTestServerFull(t)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/status-events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.handleStatusEvents(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // let initial snapshot flush

	// Push a second event via BroadcastStatus.
	s.BroadcastStatus("ready", "processing")
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	// Body should contain two SSE data blocks.
	events := strings.Count(rec.Body.String(), "data: ")
	if events < 2 {
		t.Errorf("expected at least 2 SSE events (initial + broadcast), got %d\nbody: %q",
			events, rec.Body.String())
	}
}

func TestHandleStatusEvents_WrongMethod_Returns405(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodPost, "/api/status-events", nil)
	rec := httptest.NewRecorder()
	s.handleStatusEvents(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}
