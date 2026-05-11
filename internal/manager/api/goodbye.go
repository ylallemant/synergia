package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// GoodbyeAPI handles the worker self-uninstall notification.
type GoodbyeAPI struct {
	store *store.Store
}

func NewGoodbyeAPI(s *store.Store) *GoodbyeAPI { return &GoodbyeAPI{store: s} }

// goodbyeRequest is the signed uninstall notification sent by the worker.
type goodbyeRequest struct {
	Fingerprint string `json:"fingerprint"`
	// Payload is "goodbye:<fingerprint>:<RFC3339-timestamp>".
	// Including the fingerprint binds the signature to this specific worker;
	// the timestamp limits the replay window to ±5 minutes.
	Payload   string `json:"payload"`
	Signature string `json:"signature"` // hex(ed25519.Sign(privateKey, payload))
}

// GoodbyeHandler handles POST /v1/workers/goodbye.
// The worker signs its farewell with its Ed25519 private key. The manager
// verifies the signature and, if valid, marks the record as "deleted".
// The endpoint always returns 200 — the client is exiting and nobody awaits
// the response. Invalid or forged requests are silently logged and ignored;
// the only consequence of a bad signature is that no deletion occurs.
func (a *GoodbyeAPI) GoodbyeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK) // caller is gone anyway
		return
	}

	// Always respond 200 immediately; the client is already uninstalling.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	var req goodbyeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Fingerprint == "" {
		log.Debug().Msg("goodbye: malformed request body — ignored")
		return
	}

	// Validate payload structure: "goodbye:<fingerprint>:<RFC3339-timestamp>"
	parts := strings.SplitN(req.Payload, ":", 3)
	if len(parts) != 3 || parts[0] != "goodbye" || parts[1] != req.Fingerprint {
		log.Warn().Str("fingerprint", req.Fingerprint[:min(12, len(req.Fingerprint))]).
			Msg("goodbye: malformed payload — ignored")
		return
	}
	ts, err := time.Parse(time.RFC3339, parts[2])
	if err != nil || time.Since(ts).Abs() > 5*time.Minute {
		log.Warn().Str("fingerprint", req.Fingerprint[:min(12, len(req.Fingerprint))]).
			Msg("goodbye: timestamp out of range — ignored (possible replay)")
		return
	}

	// Look up the registered public key for this fingerprint.
	worker, err := a.store.GetWorker(req.Fingerprint)
	if err != nil {
		log.Warn().Str("fingerprint", req.Fingerprint[:min(12, len(req.Fingerprint))]).
			Msg("goodbye: unknown fingerprint — ignored")
		return
	}
	pubKeyBytes, err := base64.StdEncoding.DecodeString(worker.PublicKey)
	if err != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
		log.Error().Str("fingerprint", req.Fingerprint[:min(12, len(req.Fingerprint))]).
			Msg("goodbye: stored public key invalid")
		return
	}
	sigBytes, err := hex.DecodeString(req.Signature)
	if err != nil {
		log.Warn().Str("fingerprint", req.Fingerprint[:min(12, len(req.Fingerprint))]).
			Msg("goodbye: signature hex invalid — ignored")
		return
	}

	// Verify the signature — an invalid signature means no deletion, logged silently.
	if !ed25519.Verify(ed25519.PublicKey(pubKeyBytes), []byte(req.Payload), sigBytes) {
		log.Warn().Str("fingerprint", req.Fingerprint[:min(12, len(req.Fingerprint))]).
			Msg("goodbye: signature verification failed — ignored")
		return
	}

	if err := a.store.SetWorkerDeleted(req.Fingerprint); err != nil {
		log.Error().Err(err).Str("fingerprint", req.Fingerprint).
			Msg("goodbye: failed to mark worker as deleted")
		return
	}

	log.Info().Str("fingerprint", req.Fingerprint[:min(12, len(req.Fingerprint))]+"…").
		Msg("worker uninstalled — marked as deleted")
}
