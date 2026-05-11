package gateway

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ylallemant/synergia/internal/protocol"
)

var testUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsTestPair creates a matched server/client WebSocket pair using an in-process
// httptest.Server. No external network; connections are torn down via t.Cleanup.
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

// clientRespond reads a Challenge from cli, signs the nonce with privKey, and
// sends a SignedResponse. Mirrors what handleChallenge does in the connection package.
func clientRespond(cli *websocket.Conn, privKey ed25519.PrivateKey) {
	_, msg, err := cli.ReadMessage()
	if err != nil {
		return
	}
	var challenge protocol.Challenge
	if err := json.Unmarshal(msg, &challenge); err != nil {
		return
	}
	nonce, err := base64.StdEncoding.DecodeString(challenge.Nonce)
	if err != nil {
		return
	}
	sig := ed25519.Sign(privKey, nonce)
	cli.WriteJSON(&protocol.SignedResponse{ //nolint:errcheck
		Type:      protocol.TypeSignedResponse,
		Nonce:     challenge.Nonce,
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
}

// ── doChallenge tests ─────────────────────────────────────────────────────────

func TestDoChallenge_ValidSignature_ReturnsTrue(t *testing.T) {
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	srv, cli := wsTestPair(t)

	result := make(chan bool, 1)
	go func() { result <- doChallenge(srv, pubKey) }()
	go clientRespond(cli, privKey)

	if ok := <-result; !ok {
		t.Error("doChallenge should return true for a valid signature")
	}
}

func TestDoChallenge_WrongKey_ReturnsFalse(t *testing.T) {
	pubKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	_, wrongPrivKey, _ := ed25519.GenerateKey(nil)
	srv, cli := wsTestPair(t)

	result := make(chan bool, 1)
	go func() { result <- doChallenge(srv, pubKey) }()
	go clientRespond(cli, wrongPrivKey)

	if ok := <-result; ok {
		t.Error("doChallenge should return false when signed with the wrong key")
	}
}

func TestDoChallenge_TamperedNonce_ReturnsFalse(t *testing.T) {
	pubKey, privKey, _ := ed25519.GenerateKey(nil)
	srv, cli := wsTestPair(t)

	result := make(chan bool, 1)
	go func() { result <- doChallenge(srv, pubKey) }()
	go func() {
		// Read the real challenge but respond with a different nonce — replay attack.
		_, _, err := cli.ReadMessage()
		if err != nil {
			return
		}
		fakeNonce := make([]byte, 32) // all zeros — different from server's random nonce
		sig := ed25519.Sign(privKey, fakeNonce)
		cli.WriteJSON(&protocol.SignedResponse{ //nolint:errcheck
			Type:      protocol.TypeSignedResponse,
			Nonce:     base64.StdEncoding.EncodeToString(fakeNonce),
			Signature: base64.StdEncoding.EncodeToString(sig),
		})
	}()

	select {
	case ok := <-result:
		if ok {
			t.Error("doChallenge must return false when nonce is tampered")
		}
	case <-time.After(challengeTimeout + 2*time.Second):
		t.Error("test exceeded deadline")
	}
}

func TestDoChallenge_ClientDoesNotRespond_ReturnsFalse(t *testing.T) {
	pubKey, _, _ := ed25519.GenerateKey(nil)
	srv, cli := wsTestPair(t)
	_ = cli // worker stays silent

	result := make(chan bool, 1)
	go func() { result <- doChallenge(srv, pubKey) }()

	select {
	case ok := <-result:
		if ok {
			t.Error("doChallenge must return false when client does not respond")
		}
	case <-time.After(challengeTimeout + 2*time.Second):
		t.Errorf("doChallenge did not enforce the %s timeout", challengeTimeout)
	}
}

func TestDoChallenge_MalformedResponse_ReturnsFalse(t *testing.T) {
	pubKey, _, _ := ed25519.GenerateKey(nil)
	srv, cli := wsTestPair(t)

	result := make(chan bool, 1)
	go func() { result <- doChallenge(srv, pubKey) }()
	go func() {
		cli.ReadMessage() //nolint:errcheck
		cli.WriteMessage(websocket.TextMessage, []byte("not json at all")) //nolint:errcheck
	}()

	if ok := <-result; ok {
		t.Error("doChallenge must return false for malformed response JSON")
	}
}
