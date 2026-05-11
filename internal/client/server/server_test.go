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

	"os"

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

// ── handleStatus ──────────────────────────────────────────────────────────────

func TestHandleStatus_GET_ReturnsValidJSON(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, field := range []string{"connected", "processing", "gpu_state", "llm_reachable", "model"} {
		if _, ok := m[field]; !ok {
			t.Errorf("missing field %q in status response", field)
		}
	}
}

func TestHandleStatus_POST_Returns405(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodPost, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// ── handleHealth ──────────────────────────────────────────────────────────────

func TestHandleHealth_ReturnsOK(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]string
	json.NewDecoder(rec.Body).Decode(&m)
	if m["status"] != "ok" {
		t.Errorf("want status=ok, got %q", m["status"])
	}
}

// ── handleGPU ─────────────────────────────────────────────────────────────────

func TestHandleGPU_GET_ReturnsValidJSON(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodGet, "/api/gpu", nil)
	rec := httptest.NewRecorder()
	s.handleGPU(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, field := range []string{"utilization", "state", "sent_to_manager", "stats"} {
		if _, ok := m[field]; !ok {
			t.Errorf("missing field %q in gpu response", field)
		}
	}
}

func TestHandleGPU_StateString_IsAvailable(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodGet, "/api/gpu", nil)
	rec := httptest.NewRecorder()
	s.handleGPU(rec, req)

	var m map[string]any
	json.NewDecoder(rec.Body).Decode(&m)
	if m["state"] != "available" {
		t.Errorf("want state=available (stub returns StateAvailable), got %q", m["state"])
	}
}

func TestHandleGPU_POST_Returns405(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodPost, "/api/gpu", nil)
	rec := httptest.NewRecorder()
	s.handleGPU(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// ── handleConsent ─────────────────────────────────────────────────────────────

func TestHandleConsent_GET_ReturnsState(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodGet, "/api/consent", nil)
	rec := httptest.NewRecorder()
	s.handleConsent(rec, req)

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

func TestHandleConsent_POST_Accept_Returns200(t *testing.T) {
	s := newTestServerFull(t)
	body := strings.NewReader(`{"accepted":true,"hardware_stats":true,"config_preferences":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/consent", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleConsent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]string
	json.NewDecoder(rec.Body).Decode(&m)
	if m["status"] != "ok" {
		t.Errorf("want status=ok, got %q", m["status"])
	}
}

func TestHandleConsent_POST_InvalidJSON_Returns400(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodPost, "/api/consent", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	s.handleConsent(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestHandleConsent_OPTIONS_Returns200(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodOptions, "/api/consent", nil)
	rec := httptest.NewRecorder()
	s.handleConsent(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200 for OPTIONS, got %d", rec.Code)
	}
}

func TestHandleConsent_DELETE_Returns405(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/consent", nil)
	rec := httptest.NewRecorder()
	s.handleConsent(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// ── handleConfig ──────────────────────────────────────────────────────────────

func TestHandleConfig_GET_ReturnsConfig(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestHandleConfig_POST_WithoutConsent_Returns403(t *testing.T) {
	s := newTestServerFull(t)
	// Consent not accepted — config POST must be forbidden.
	body := strings.NewReader(`{"preferred_role":"tester","nickname":""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", body)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403 without consent, got %d", rec.Code)
	}
}

func TestHandleConfig_POST_InvalidJSON_Returns400(t *testing.T) {
	s := newTestServerFull(t)
	// Accept consent first so the method check is skipped.
	s.consent.Accept(true, true) //nolint:errcheck
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for invalid JSON, got %d", rec.Code)
	}
}

// ── handleHardwareInfo ────────────────────────────────────────────────────────

func TestHandleHardwareInfo_GET_ReturnsPayload(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodGet, "/api/hardware-info", nil)
	rec := httptest.NewRecorder()
	s.handleHardwareInfo(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, field := range []string{"fingerprint", "hardware", "config"} {
		if _, ok := m[field]; !ok {
			t.Errorf("missing field %q in hardware-info response", field)
		}
	}
}

func TestHandleHardwareInfo_POST_Returns405(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodPost, "/api/hardware-info", nil)
	rec := httptest.NewRecorder()
	s.handleHardwareInfo(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

// ── handleManagerURL ─────────────────────────────────────────────────────────

func TestHandleManagerURL_POST_CallsCallback(t *testing.T) {
	s := newTestServerFull(t)
	var captured string
	s.onManagerURLSet = func(url string) { captured = url }

	body := strings.NewReader(`{"url":"wss://cluster.example.com/ws/worker"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/manager-url", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleManagerURL(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if captured != "wss://cluster.example.com/ws/worker" {
		t.Errorf("callback not called with correct URL, got %q", captured)
	}
}

func TestHandleManagerURL_POST_EmptyURL_Returns400(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodPost, "/api/manager-url", strings.NewReader(`{"url":""}`))
	rec := httptest.NewRecorder()
	s.handleManagerURL(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for empty URL, got %d", rec.Code)
	}
}

func TestHandleManagerURL_POST_InvalidJSON_Returns400(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodPost, "/api/manager-url", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	s.handleManagerURL(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for invalid JSON, got %d", rec.Code)
	}
}

func TestHandleManagerURL_GET_Returns405(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodGet, "/api/manager-url", nil)
	rec := httptest.NewRecorder()
	s.handleManagerURL(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}

func TestHandleManagerURL_OPTIONS_Returns200(t *testing.T) {
	s := newTestServerFull(t)
	req := httptest.NewRequest(http.MethodOptions, "/api/manager-url", nil)
	rec := httptest.NewRecorder()
	s.handleManagerURL(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200 for OPTIONS, got %d", rec.Code)
	}
}

// ── handleLogsFile ────────────────────────────────────────────────────────────

func TestHandleLogsFile_NoPath_Returns404(t *testing.T) {
	s := newTestServer(t)
	// logFilePath is empty — no log file configured.
	req := httptest.NewRequest(http.MethodGet, "/api/logs-file", nil)
	rec := httptest.NewRecorder()
	s.handleLogsFile(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404 when no log file, got %d", rec.Code)
	}
}

func TestHandleLogsFile_WithFile_ServesContent(t *testing.T) {
	s := newTestServer(t)
	// Write a temporary log file.
	logPath := strings.Join([]string{t.TempDir(), "test.log"}, "/")
	if err := os.WriteFile(logPath, []byte(`{"level":"info","msg":"test"}`), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	s.logFilePath = logPath

	req := httptest.NewRequest(http.MethodGet, "/api/logs-file", nil)
	rec := httptest.NewRecorder()
	s.handleLogsFile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"msg":"test"`) {
		t.Errorf("log content not served: %q", rec.Body.String())
	}
}

func TestHandleLogsFile_POST_Returns405(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/logs-file", nil)
	rec := httptest.NewRecorder()
	s.handleLogsFile(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", rec.Code)
	}
}
