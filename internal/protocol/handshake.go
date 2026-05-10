package protocol

// Handshake message types — used when CLUSTER_WORKER_KEY is absent (TOFU mode).
const (
	TypeChallenge      = "challenge"       // manager → worker: random nonce to sign
	TypeSignedResponse = "signed_response" // worker → manager: Ed25519 signature of the nonce
)

// Challenge is sent by the manager immediately after WebSocket upgrade in TOFU mode.
// The worker must sign the nonce bytes with its Ed25519 private key and reply with a SignedResponse.
type Challenge struct {
	Type  string `json:"type"`  // TypeChallenge
	Nonce string `json:"nonce"` // base64(32 random bytes)
}

// SignedResponse is the worker's reply to a Challenge.
type SignedResponse struct {
	Type      string `json:"type"`      // TypeSignedResponse
	Nonce     string `json:"nonce"`     // echoed nonce for correlation
	Signature string `json:"signature"` // base64(Ed25519.Sign(privkey, nonce_bytes))
}
