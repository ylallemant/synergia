package api

import (
	"bytes"
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

	"github.com/ylallemant/synergia/internal/manager/store"
)

// openTestStoreGoodbye opens an in-memory SQLite store for goodbye tests.
func openTestStoreGoodbye(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("openTestStore: %v", err)
	}
	return s
}

// seedWorkerWithKey generates a fresh Ed25519 keypair, seeds the given store
// with a worker record, and returns the public key, private key, and fingerprint.
func seedWorkerWithKey(t *testing.T, s *store.Store) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pubKeyB64 := base64.StdEncoding.EncodeToString(pubKey)
	hash := sha256.Sum256(pubKey)
	fingerprint := hex.EncodeToString(hash[:])

	if err := s.UpsertWorker(fingerprint, pubKeyB64, "TestModel", "Q4", "0.0.1", "linux", "amd64"); err != nil {
		t.Fatalf("UpsertWorker: %v", err)
	}
	return pubKey, privKey, fingerprint
}

// makeSignedBody creates a valid signed goodbye request body.
func makeSignedBody(t *testing.T, privKey ed25519.PrivateKey, fingerprint string, ts time.Time) []byte {
	t.Helper()
	payload := "goodbye:" + fingerprint + ":" + ts.UTC().Format(time.RFC3339)
	sig := hex.EncodeToString(ed25519.Sign(privKey, []byte(payload)))
	body, _ := json.Marshal(map[string]string{
		"fingerprint": fingerprint,
		"payload":     payload,
		"signature":   sig,
	})
	return body
}

func postGoodbye(t *testing.T, api *GoodbyeAPI, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/workers/goodbye", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	api.GoodbyeHandler(rec, req)
	return rec
}

func workerStatus(t *testing.T, s *store.Store, fingerprint string) string {
	t.Helper()
	w, err := s.GetWorker(fingerprint)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	return w.Status
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestGoodbye_ValidSignature_MarksDeleted(t *testing.T) {
	s := openTestStoreGoodbye(t)
	_, privKey, fp := seedWorkerWithKey(t, s)
	api := NewGoodbyeAPI(s)

	body := makeSignedBody(t, privKey, fp, time.Now())
	rec := postGoodbye(t, api, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if status := workerStatus(t, s, fp); status != "deleted" {
		t.Errorf("want status=deleted, got %q", status)
	}
}

func TestGoodbye_InvalidSignature_NoChange(t *testing.T) {
	s := openTestStoreGoodbye(t)
	_, _, fp := seedWorkerWithKey(t, s)
	api := NewGoodbyeAPI(s)

	// Sign with a different key.
	_, wrongKey, _ := ed25519.GenerateKey(nil)
	body := makeSignedBody(t, wrongKey, fp, time.Now())

	rec := postGoodbye(t, api, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (silent failure), got %d", rec.Code)
	}
	if status := workerStatus(t, s, fp); status == "deleted" {
		t.Error("worker must NOT be deleted when signature is invalid")
	}
}

func TestGoodbye_ReplayOutsideWindow_NoChange(t *testing.T) {
	s := openTestStoreGoodbye(t)
	_, privKey, fp := seedWorkerWithKey(t, s)
	api := NewGoodbyeAPI(s)

	// Timestamp 10 minutes in the past — outside the ±5 min window.
	staleTime := time.Now().Add(-10 * time.Minute)
	body := makeSignedBody(t, privKey, fp, staleTime)

	postGoodbye(t, api, body)

	if status := workerStatus(t, s, fp); status == "deleted" {
		t.Error("replayed goodbye must not mark worker as deleted")
	}
}

func TestGoodbye_UnknownFingerprint_NoChange(t *testing.T) {
	s := openTestStoreGoodbye(t)
	_, privKey, _ := seedWorkerWithKey(t, s) // seed some other worker
	api := NewGoodbyeAPI(s)

	unknownFP := strings.Repeat("a", 64)
	body := makeSignedBody(t, privKey, unknownFP, time.Now())

	rec := postGoodbye(t, api, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (silent failure), got %d", rec.Code)
	}
}

func TestGoodbye_MalformedPayload_NoChange(t *testing.T) {
	s := openTestStoreGoodbye(t)
	_, privKey, fp := seedWorkerWithKey(t, s)
	api := NewGoodbyeAPI(s)

	// Payload has wrong prefix.
	payload := "farewell:" + fp + ":" + time.Now().UTC().Format(time.RFC3339)
	sig := hex.EncodeToString(ed25519.Sign(privKey, []byte(payload)))
	body, _ := json.Marshal(map[string]string{
		"fingerprint": fp,
		"payload":     payload,
		"signature":   sig,
	})

	postGoodbye(t, api, body)

	if status := workerStatus(t, s, fp); status == "deleted" {
		t.Error("malformed payload must not mark worker as deleted")
	}
}

func TestGoodbye_FingerprintMismatch_NoChange(t *testing.T) {
	s := openTestStoreGoodbye(t)
	_, privKey, fp := seedWorkerWithKey(t, s)
	api := NewGoodbyeAPI(s)

	// Payload fingerprint doesn't match the request fingerprint.
	payload := "goodbye:differentfingerprint:" + time.Now().UTC().Format(time.RFC3339)
	sig := hex.EncodeToString(ed25519.Sign(privKey, []byte(payload)))
	body, _ := json.Marshal(map[string]string{
		"fingerprint": fp,
		"payload":     payload,
		"signature":   sig,
	})

	postGoodbye(t, api, body)

	if status := workerStatus(t, s, fp); status == "deleted" {
		t.Error("fingerprint mismatch in payload must not mark worker as deleted")
	}
}

func TestGoodbye_NonPostMethod_Returns200(t *testing.T) {
	s := openTestStoreGoodbye(t)
	api := NewGoodbyeAPI(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/workers/goodbye", nil)
	rec := httptest.NewRecorder()
	api.GoodbyeHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200 for wrong method (caller is gone), got %d", rec.Code)
	}
}
