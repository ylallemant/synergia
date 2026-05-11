package connection

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ylallemant/synergia/internal/client/identity"
	"github.com/ylallemant/synergia/internal/protocol"
)

// makeIdentity generates a fresh Ed25519 identity for tests.
func makeIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return &identity.Identity{
		PrivateKey:  privKey,
		PublicKey:   pubKey,
		Fingerprint: "test-fingerprint",
	}
}

// testConn wires a minimal *Connection to the client side of a wsTestPair.
// Returns the Connection and the server-side conn to inject messages from.
func testConn(t *testing.T, id *identity.Identity) (*Connection, *websocket.Conn) {
	t.Helper()
	srv, cli := wsTestPair(t)
	c := &Connection{
		identity:        id,
		conn:            cli,
		WorkUnitCh:      make(chan *protocol.WorkUnit, 10),
		ModelUpdateCh:   make(chan *protocol.ModelUpdate, 5),
		BinaryUpdateCh:  make(chan *protocol.BinaryUpdate, 1),
		BackendUpdateCh: make(chan *protocol.BackendUpdate, 1),
		done:            make(chan struct{}),
		connectedCh:     make(chan struct{}),
	}
	return c, srv
}

// runReadLoop starts c.readLoop in a goroutine and returns its error channel.
func runReadLoop(c *Connection) chan error {
	ch := make(chan error, 1)
	go func() { ch <- c.readLoop(context.Background()) }()
	return ch
}

// ── readLoop dispatch ─────────────────────────────────────────────────────────

func TestReadLoop_WorkUnit_RoutesToWorkUnitCh(t *testing.T) {
	c, srv := testConn(t, makeIdentity(t))
	errCh := runReadLoop(c)

	srv.WriteJSON(protocol.WorkUnit{Type: protocol.TypeWorkUnit, ID: "test-wu-1"}) //nolint:errcheck
	srv.Close()

	select {
	case got := <-c.WorkUnitCh:
		if got.ID != "test-wu-1" {
			t.Errorf("want ID %q, got %q", "test-wu-1", got.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for WorkUnit")
	}
	<-errCh
}

func TestReadLoop_ModelUpdate_RoutesToModelUpdateCh(t *testing.T) {
	c, srv := testConn(t, makeIdentity(t))
	errCh := runReadLoop(c)

	srv.WriteJSON(protocol.ModelUpdate{Type: protocol.TypeModelUpdate, Role: "tester", Model: "SmolLM2"}) //nolint:errcheck
	srv.Close()

	select {
	case got := <-c.ModelUpdateCh:
		if got.Role != "tester" || got.Model != "SmolLM2" {
			t.Errorf("unexpected ModelUpdate: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ModelUpdate")
	}
	<-errCh
}

func TestReadLoop_BinaryUpdate_RoutesToBinaryUpdateCh(t *testing.T) {
	c, srv := testConn(t, makeIdentity(t))
	errCh := runReadLoop(c)

	srv.WriteJSON(protocol.BinaryUpdate{Type: protocol.TypeBinaryUpdate, Version: "0.0.15", SHA256: "deadbeef"}) //nolint:errcheck
	srv.Close()

	select {
	case got := <-c.BinaryUpdateCh:
		if got.Version != "0.0.15" {
			t.Errorf("want version 0.0.15, got %q", got.Version)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for BinaryUpdate")
	}
	<-errCh
}

func TestReadLoop_BackendUpdate_RoutesToBackendUpdateCh(t *testing.T) {
	c, srv := testConn(t, makeIdentity(t))
	errCh := runReadLoop(c)

	srv.WriteJSON(protocol.BackendUpdate{Type: protocol.TypeBackendUpdate, Version: "b4321"}) //nolint:errcheck
	srv.Close()

	select {
	case got := <-c.BackendUpdateCh:
		if got.Version != "b4321" {
			t.Errorf("want version b4321, got %q", got.Version)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for BackendUpdate")
	}
	<-errCh
}

func TestReadLoop_Challenge_SignsAndLoopContinues(t *testing.T) {
	id := makeIdentity(t)
	pubKey := id.PublicKey
	c, srv := testConn(t, id)
	errCh := runReadLoop(c)

	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	srv.WriteJSON(protocol.Challenge{ //nolint:errcheck
		Type:  protocol.TypeChallenge,
		Nonce: base64.StdEncoding.EncodeToString(nonce),
	})

	// readLoop calls handleChallenge which sends back a SignedResponse on the same conn.
	_, raw, err := srv.ReadMessage()
	if err != nil {
		t.Fatalf("read signed response: %v", err)
	}
	var sr protocol.SignedResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		t.Fatalf("unmarshal SignedResponse: %v", err)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sr.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !ed25519.Verify(pubKey, nonce, sigBytes) {
		t.Error("signature produced by readLoop challenge handler does not verify")
	}

	srv.Close()
	<-errCh
}

func TestReadLoop_Heartbeat_NoCrash(t *testing.T) {
	c, srv := testConn(t, makeIdentity(t))
	errCh := runReadLoop(c)

	raw, _ := json.Marshal(map[string]string{"type": protocol.TypeHeartbeat})
	srv.WriteMessage(websocket.TextMessage, raw) //nolint:errcheck
	srv.Close()

	select {
	case <-errCh:
		// readLoop exited without panicking — pass
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: readLoop did not exit after heartbeat + close")
	}
}

func TestReadLoop_UnknownType_NoCrash(t *testing.T) {
	c, srv := testConn(t, makeIdentity(t))
	errCh := runReadLoop(c)

	raw, _ := json.Marshal(map[string]string{"type": "future_message_type_v99"})
	srv.WriteMessage(websocket.TextMessage, raw) //nolint:errcheck
	srv.Close()

	select {
	case <-errCh:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: readLoop did not exit after unknown type + close")
	}
}

func TestReadLoop_MalformedJSON_ContinuesLoop(t *testing.T) {
	c, srv := testConn(t, makeIdentity(t))
	errCh := runReadLoop(c)

	// Garbage first — loop must continue and route the next valid message.
	srv.WriteMessage(websocket.TextMessage, []byte("not json at all")) //nolint:errcheck
	srv.WriteJSON(protocol.WorkUnit{Type: protocol.TypeWorkUnit, ID: "after-bad-json"}) //nolint:errcheck
	srv.Close()

	select {
	case got := <-c.WorkUnitCh:
		if got.ID != "after-bad-json" {
			t.Errorf("want ID %q, got %q", "after-bad-json", got.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: loop did not continue after malformed JSON")
	}
	<-errCh
}

// ── Send ──────────────────────────────────────────────────────────────────────

func TestSend_WhileDisconnected_ReturnsErrNotConnected(t *testing.T) {
	c := &Connection{
		WorkUnitCh:      make(chan *protocol.WorkUnit, 10),
		ModelUpdateCh:   make(chan *protocol.ModelUpdate, 5),
		BinaryUpdateCh:  make(chan *protocol.BinaryUpdate, 1),
		BackendUpdateCh: make(chan *protocol.BackendUpdate, 1),
		done:            make(chan struct{}),
		connectedCh:     make(chan struct{}),
		// conn is nil — not connected
	}
	err := c.Send(&protocol.Heartbeat{Type: protocol.TypeHeartbeat})
	if !errors.Is(err, ErrNotConnected) {
		t.Errorf("want ErrNotConnected, got %v", err)
	}
}
