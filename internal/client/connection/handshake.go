package connection

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/protocol"
)

// handleChallenge responds to a TypeChallenge message from the manager.
// It signs the nonce with the worker's Ed25519 private key and sends back a SignedResponse.
// Returns true on success.
func handleChallenge(conn *websocket.Conn, message []byte, privKey ed25519.PrivateKey) bool {
	var challenge protocol.Challenge
	if err := json.Unmarshal(message, &challenge); err != nil {
		log.Error().Err(err).Msg("handshake: failed to parse challenge")
		return false
	}

	nonce, err := base64.StdEncoding.DecodeString(challenge.Nonce)
	if err != nil {
		log.Error().Err(err).Msg("handshake: invalid nonce encoding")
		return false
	}

	log.Debug().Msg("handshake: challenge received, signing nonce")
	sig := ed25519.Sign(privKey, nonce)

	resp := &protocol.SignedResponse{
		Type:      protocol.TypeSignedResponse,
		Nonce:     challenge.Nonce,
		Signature: base64.StdEncoding.EncodeToString(sig),
	}
	if err := conn.WriteJSON(resp); err != nil {
		log.Error().Err(err).Msg("handshake: failed to send signed response")
		return false
	}

	log.Debug().Msg("handshake: challenge-response completed")
	return true
}
