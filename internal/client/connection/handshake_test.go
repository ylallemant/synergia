package connection

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/ylallemant/synergia/internal/protocol"
)

var testUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsTestPair creates a matched server/client WebSocket pair via an in-process
// httptest.Server. Torn down automatically via t.Cleanup.
func wsTestPair(t *testing.T) (serverConn, clientConn *websocket.Conn) {
	t.Helper()
	connCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		connCh <- conn
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	serverConn = <-connCh
	t.Cleanup(func() { serverConn.Close(); clientConn.Close() })
	return
}

// makeChallengeMsg builds the raw JSON bytes for a Challenge message.
func makeChallengeMsg(t *testing.T, nonce []byte) []byte {
	t.Helper()
	b, err := json.Marshal(protocol.Challenge{
		Type:  protocol.TypeChallenge,
		Nonce: base64.StdEncoding.EncodeToString(nonce),
	})
	if err != nil {
		t.Fatalf("marshal challenge: %v", err)
	}
	return b
}

// ── handleChallenge (worker side) ────────────────────────────────────────────

func TestHandleChallenge_SignsCorrectly_ReturnsTrue(t *testing.T) {
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	srv, cli := wsTestPair(t)

	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	msg := makeChallengeMsg(t, nonce)

	result := make(chan bool, 1)
	go func() { result <- handleChallenge(cli, msg, privKey) }()

	// Manager reads back the SignedResponse.
	_, raw, err := srv.ReadMessage()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if ok := <-result; !ok {
		t.Fatal("handleChallenge should return true")
	}

	// Verify the signature is cryptographically correct.
	var sr protocol.SignedResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		t.Fatalf("unmarshal SignedResponse: %v", err)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sr.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !ed25519.Verify(pubKey, nonce, sigBytes) {
		t.Error("signature produced by handleChallenge does not verify with the public key")
	}
}

func TestHandleChallenge_EchoesNonce(t *testing.T) {
	_, privKey, _ := ed25519.GenerateKey(nil)
	srv, cli := wsTestPair(t)

	nonce := []byte("deterministic-test-nonce-bytes32")
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)
	msg := makeChallengeMsg(t, nonce)

	go handleChallenge(cli, msg, privKey) //nolint:errcheck

	_, raw, _ := srv.ReadMessage()
	var sr protocol.SignedResponse
	json.Unmarshal(raw, &sr) //nolint:errcheck
	if sr.Nonce != nonceB64 {
		t.Errorf("nonce not echoed: want %q got %q", nonceB64, sr.Nonce)
	}
	if sr.Type != protocol.TypeSignedResponse {
		t.Errorf("wrong type: want %q got %q", protocol.TypeSignedResponse, sr.Type)
	}
}

func TestHandleChallenge_MalformedJSON_ReturnsFalse(t *testing.T) {
	_, privKey, _ := ed25519.GenerateKey(nil)
	_, cli := wsTestPair(t)

	ok := handleChallenge(cli, []byte("not json at all"), privKey)
	if ok {
		t.Error("handleChallenge must return false for malformed JSON")
	}
}

func TestHandleChallenge_InvalidBase64Nonce_ReturnsFalse(t *testing.T) {
	_, privKey, _ := ed25519.GenerateKey(nil)
	_, cli := wsTestPair(t)

	msg, _ := json.Marshal(protocol.Challenge{
		Type:  protocol.TypeChallenge,
		Nonce: "!!!not-valid-base64!!!",
	})
	ok := handleChallenge(cli, msg, privKey)
	if ok {
		t.Error("handleChallenge must return false for invalid base64 nonce")
	}
}

// TestHandleChallenge_PairedWithManagerSide verifies both halves compose correctly:
// the worker signs the nonce and the manager can verify it — all in-process.
func TestHandleChallenge_PairedWithManagerSide(t *testing.T) {
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	srv, cli := wsTestPair(t)

	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(i + 7)
	}
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)

	managerVerified := make(chan bool, 1)
	go func() {
		// Manager: send challenge.
		srv.WriteJSON(protocol.Challenge{Type: protocol.TypeChallenge, Nonce: nonceB64}) //nolint:errcheck
		// Manager: read and verify response.
		_, raw, err := srv.ReadMessage()
		if err != nil {
			managerVerified <- false
			return
		}
		var sr protocol.SignedResponse
		if err := json.Unmarshal(raw, &sr); err != nil || sr.Nonce != nonceB64 {
			managerVerified <- false
			return
		}
		sigBytes, _ := base64.StdEncoding.DecodeString(sr.Signature)
		managerVerified <- ed25519.Verify(pubKey, nonce, sigBytes)
	}()

	// Worker: read and respond.
	_, msg, err := cli.ReadMessage()
	if err != nil {
		t.Fatalf("worker read: %v", err)
	}
	workerOK := handleChallenge(cli, msg, privKey)
	managerOK := <-managerVerified

	if !workerOK {
		t.Error("handleChallenge (worker) returned false")
	}
	if !managerOK {
		t.Error("manager could not verify the worker's signature")
	}
}
