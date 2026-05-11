package connection

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/protocol"
)

// buildChallengeResponse parses message and signs the nonce, returning the
// SignedResponse that must be sent back to the manager.
// Returns (nil, false) on any parse or encoding error.
// The caller is responsible for writing the response through a mutex-safe
// send path (Connection.Send) to avoid concurrent-write panics.
func buildChallengeResponse(message []byte, privKey ed25519.PrivateKey) (*protocol.SignedResponse, bool) {
	var challenge protocol.Challenge
	if err := json.Unmarshal(message, &challenge); err != nil {
		log.Error().Err(err).Msg("handshake: failed to parse challenge")
		return nil, false
	}
	nonce, err := base64.StdEncoding.DecodeString(challenge.Nonce)
	if err != nil {
		log.Error().Err(err).Msg("handshake: invalid nonce encoding")
		return nil, false
	}
	log.Debug().Msg("handshake: challenge received, signing nonce")
	sig := ed25519.Sign(privKey, nonce)
	return &protocol.SignedResponse{
		Type:      protocol.TypeSignedResponse,
		Nonce:     challenge.Nonce,
		Signature: base64.StdEncoding.EncodeToString(sig),
	}, true
}

// handleChallenge is a convenience wrapper used in tests and direct (single-writer)
// contexts: it builds the response and writes it to conn immediately.
// Do NOT call this from readLoop — use buildChallengeResponse + Connection.Send instead
// so the write goes through the connection mutex.
func handleChallenge(conn *websocket.Conn, message []byte, privKey ed25519.PrivateKey) bool {
	resp, ok := buildChallengeResponse(message, privKey)
	if !ok {
		return false
	}
	if err := conn.WriteJSON(resp); err != nil {
		log.Error().Err(err).Msg("handshake: failed to send signed response")
		return false
	}
	log.Debug().Msg("handshake: challenge-response completed")
	return true
}
