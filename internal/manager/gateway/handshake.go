package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/protocol"
)

const challengeTimeout = 5 * time.Second

// doChallenge performs the TOFU challenge-response handshake on an already-upgraded
// WebSocket connection. It generates a 32-byte nonce, sends it as a Challenge message,
// waits for a SignedResponse, and verifies the Ed25519 signature against pubKey.
// Returns true on success; the caller must close the connection on false.
func doChallenge(conn *websocket.Conn, pubKey ed25519.PublicKey) bool {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		log.Error().Err(err).Msg("handshake: failed to generate nonce")
		return false
	}
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)

	if err := conn.WriteJSON(&protocol.Challenge{
		Type:  protocol.TypeChallenge,
		Nonce: nonceB64,
	}); err != nil {
		log.Error().Err(err).Msg("handshake: failed to send challenge")
		return false
	}
	log.Debug().Msg("handshake: challenge sent, awaiting signed response")

	conn.SetReadDeadline(time.Now().Add(challengeTimeout))
	defer conn.SetReadDeadline(time.Time{})

	_, message, err := conn.ReadMessage()
	if err != nil {
		log.Error().Err(err).Msg("handshake: failed to read signed response")
		return false
	}
	log.Debug().Msg("handshake: signed response received, verifying signature")

	var resp protocol.SignedResponse
	if err := json.Unmarshal(message, &resp); err != nil || resp.Type != protocol.TypeSignedResponse {
		log.Error().Msg("handshake: invalid signed response message")
		return false
	}
	if resp.Nonce != nonceB64 {
		log.Error().Msg("handshake: nonce mismatch")
		return false
	}

	sigBytes, err := base64.StdEncoding.DecodeString(resp.Signature)
	if err != nil {
		log.Error().Err(err).Msg("handshake: invalid signature encoding")
		return false
	}

	if !ed25519.Verify(pubKey, nonce, sigBytes) {
		log.Error().Msg("handshake: signature verification failed")
		return false
	}

	return true
}
