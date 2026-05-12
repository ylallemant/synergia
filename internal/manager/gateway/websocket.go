package gateway

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/backend"
	"github.com/ylallemant/synergia/internal/manager/queue"
	"github.com/ylallemant/synergia/internal/manager/store"
	"github.com/ylallemant/synergia/internal/protocol"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WorkerInfo holds metadata about a connected worker.
type WorkerInfo struct {
	Fingerprint  string
	PublicKey    ed25519.PublicKey
	Model        string
	Quantisation string
	Version      string
	OS           string
	Arch         string
	LLMHash      string
	ConnectedAt  time.Time
}

// Gateway manages WebSocket connections from workers.
type Gateway struct {
	workerKey string
	queue     *queue.Queue
	store     *store.Store

	mu     sync.RWMutex
	worker *workerConn // Phase 1: single worker

	// Known fingerprint → public key registry (in-memory cache)
	knownKeys   map[string]ed25519.PublicKey
	knownKeysMu sync.RWMutex
}

type workerConn struct {
	conn *websocket.Conn
	info WorkerInfo
}

func New(workerKey string, q *queue.Queue, s *store.Store) *Gateway {
	return &Gateway{
		workerKey: workerKey,
		queue:     q,
		store:     s,
		knownKeys: make(map[string]ed25519.PublicKey),
	}
}

// SetWorkerKey updates the worker authentication mode at runtime.
// An empty key enables TOFU mode; a non-empty key enables key-auth mode.
// Safe to call while the gateway is serving connections.
func (g *Gateway) SetWorkerKey(key string) {
	g.mu.Lock()
	g.workerKey = key
	g.mu.Unlock()
}

// WorkerKey returns the current worker key. Returns "" in TOFU mode.
// Worker-facing HTTP APIs call this at request time so they always reflect
// the live auth mode rather than the value captured at startup.
func (g *Gateway) WorkerKey() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.workerKey
}

// ServeHTTP handles the WebSocket upgrade for /ws/worker.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Auth mode:
	//   key-auth — workerKey configured: Bearer token must match before upgrade
	//   TOFU     — workerKey empty: challenge-response after upgrade
	authHeader := r.Header.Get("Authorization")
	g.mu.RLock()
	workerKey := g.workerKey
	g.mu.RUnlock()
	if workerKey != "" {
		if authHeader != "Bearer "+workerKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Extract worker identity
	fingerprint := r.Header.Get("X-Worker-Fingerprint")
	pubKeyB64 := r.Header.Get("X-Worker-Public-Key")
	model := r.Header.Get("X-Worker-Model")
	quantisation := r.Header.Get("X-Worker-Quantisation")
	clientVersion := r.Header.Get("X-Worker-Version")
	llmHash := r.Header.Get("X-Worker-LLM-Hash")
	backendHash := r.Header.Get("X-Worker-Backend-Hash")
	workerOS := r.Header.Get("X-Worker-OS")
	workerArch := r.Header.Get("X-Worker-Arch")

	if fingerprint == "" || pubKeyB64 == "" {
		http.Error(w, "X-Worker-Fingerprint and X-Worker-Public-Key headers required", http.StatusBadRequest)
		return
	}

	// Decode and verify public key matches fingerprint
	pubKeyBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		http.Error(w, "invalid X-Worker-Public-Key encoding", http.StatusBadRequest)
		return
	}
	if len(pubKeyBytes) != ed25519.PublicKeySize {
		http.Error(w, "invalid public key size", http.StatusBadRequest)
		return
	}
	pubKey := ed25519.PublicKey(pubKeyBytes)

	// Verify fingerprint = SHA256(public_key)
	hash := sha256.Sum256(pubKeyBytes)
	expectedFingerprint := hex.EncodeToString(hash[:])
	if fingerprint != expectedFingerprint {
		http.Error(w, "fingerprint does not match public key", http.StatusBadRequest)
		return
	}

	// Check known keys registry
	if !g.verifyOrRegisterKey(fingerprint, pubKey) {
		http.Error(w, "fingerprint registered with different public key", http.StatusForbidden)
		return
	}

	// Upgrade to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Msg("websocket upgrade failed")
		return
	}

	if workerKey == "" {
		// TOFU mode: challenge-response after upgrade
		log.Debug().Str("fingerprint", fingerprint).Msg("handshake: TOFU mode — sending challenge")
		if !doChallenge(conn, pubKey) {
			log.Warn().Str("fingerprint", fingerprint).Msg("handshake: TOFU challenge-response failed — closing connection")
			conn.Close()
			return
		}
		log.Info().Str("fingerprint", fingerprint).Msg("handshake: TOFU challenge-response succeeded")
	} else {
		log.Debug().Str("fingerprint", fingerprint).Msg("handshake: key-auth mode — Bearer token accepted")
	}

	wc := &workerConn{
		conn: conn,
		info: WorkerInfo{
			Fingerprint:  fingerprint,
			PublicKey:    pubKey,
			Model:        model,
			Quantisation: quantisation,
			Version:      clientVersion,
			OS:           workerOS,
			Arch:         workerArch,
			LLMHash:      llmHash,
			ConnectedAt:  time.Now(),
		},
	}

	g.mu.Lock()
	if g.worker != nil {
		// Phase 1: single worker slot — reject the new connection so the two
		// workers don't ping-pong each other off the slot. The rejected worker
		// will back off and retry; when the current worker disconnects it can
		// take the slot.
		g.mu.Unlock()
		log.Warn().Str("fingerprint", fingerprint).Msg("worker slot occupied — rejecting new connection")
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "slot occupied"))
		conn.Close()
		return
	}
	g.worker = wc
	g.mu.Unlock()

	// Persist worker in DB
	if g.store != nil {
		if err := g.store.UpsertWorker(fingerprint, pubKeyB64, model, quantisation, clientVersion, workerOS, workerArch); err != nil {
			log.Error().Err(err).Msg("failed to persist worker")
		}
		if llmHash != "" {
			if err := g.store.SetWorkerLLMHash(fingerprint, llmHash); err != nil {
				log.Error().Err(err).Msg("failed to persist worker LLM hash")
			}
			g.store.UpdateWorkerSyncStatus(fingerprint)
		}
		if backendHash != "" {
			if err := g.store.SetWorkerBackendHash(fingerprint, backendHash); err != nil {
				log.Error().Err(err).Msg("failed to persist worker backend hash")
			}
		}
		// Always initialise backend_sync_status so the DB default "out-of-sync"
		// does not persist on fresh workers that connect with backend_hash="".
		g.store.UpdateWorkerBackendSyncStatus(fingerprint)

		// Initialise binary_sync_status from the client's reported version so the
		// DB default "out-of-sync" doesn't block aggregated status when a version
		// target is configured (e.g. development mode sets one on startup).
		if cfg, err := g.store.GetClientVersionConfig(); err == nil {
			g.store.UpdateWorkerBinarySyncStatus(fingerprint, cfg.TargetVersion)
		}
		g.store.SetWorkerOnlineIfAllowed(fingerprint)
	}

	log.Info().
		Str("model", model).
		Str("quantisation", quantisation).
		Str("llm_hash", llmHash).
		Str("backend_hash", backendHash).
		Str("fingerprint", fingerprint).
		Msg("worker connected")

	// Start reading messages from the worker
	go g.readLoop(wc)
}

// Dispatch sends a work unit to the connected worker. Returns an error if no worker is connected,
// if the worker has not accepted the data collection terms, or if the worker's LLM hash does not
// match the expected hash for the role.
func (g *Gateway) Dispatch(unit *protocol.WorkUnit) error {
	g.mu.RLock()
	wc := g.worker
	g.mu.RUnlock()

	if wc == nil {
		return fmt.Errorf("no worker connected")
	}

	// Verify worker consent before dispatching
	if g.store != nil && !g.store.HasConsent(wc.info.Fingerprint) {
		return fmt.Errorf("worker %s has not accepted data collection terms", wc.info.Fingerprint[:12])
	}

	return wc.conn.WriteJSON(unit)
}

// DispatchWithHashCheck sends a work unit only if the worker's LLM hash matches the expected role hash.
func (g *Gateway) DispatchWithHashCheck(unit *protocol.WorkUnit, expectedHash string) error {
	g.mu.RLock()
	wc := g.worker
	g.mu.RUnlock()

	if wc == nil {
		return fmt.Errorf("no worker connected")
	}

	// Verify worker consent before dispatching
	if g.store != nil && !g.store.HasConsent(wc.info.Fingerprint) {
		return fmt.Errorf("worker %s has not accepted data collection terms", wc.info.Fingerprint[:12])
	}

	// Verify LLM hash matches central configuration
	if expectedHash != "" && g.store != nil && !g.store.WorkerLLMHashMatches(wc.info.Fingerprint, expectedHash) {
		return fmt.Errorf("worker %s LLM hash mismatch (expected %s)", wc.info.Fingerprint[:12], expectedHash[:12])
	}

	return wc.conn.WriteJSON(unit)
}

// HasWorker returns true if a worker is currently connected.
func (g *Gateway) HasWorker() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.worker != nil
}

// HasAvailableWorker returns true if a worker is connected AND in "available" state
// (not paused, idle, or processing).
func (g *Gateway) HasAvailableWorker() bool {
	g.mu.RLock()
	wc := g.worker
	g.mu.RUnlock()

	if wc == nil {
		return false
	}
	if g.store == nil {
		return true // no store, assume available
	}
	return g.store.IsWorkerAvailable(wc.info.Fingerprint)
}

// WorkerStatus returns info about the current worker, or nil.
func (g *Gateway) WorkerStatus() *WorkerInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.worker == nil {
		return nil
	}
	info := g.worker.info
	return &info
}

func (g *Gateway) readLoop(wc *workerConn) {
	defer func() {
		g.mu.Lock()
		if g.worker == wc {
			g.worker = nil
		}
		g.mu.Unlock()
		wc.conn.Close()
		if g.store != nil {
			_ = g.store.SetWorkerOffline(wc.info.Fingerprint)
		}
		log.Info().Str("fingerprint", wc.info.Fingerprint).Msg("worker disconnected")
	}()

	for {
		_, message, err := wc.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Error().Err(err).Msg("worker read error")
			}
			return
		}

		var envelope protocol.Envelope
		if err := json.Unmarshal(message, &envelope); err != nil {
			log.Warn().Err(err).Msg("invalid message from worker")
			continue
		}

		switch envelope.Type {
		case protocol.TypeResult:
			var result protocol.Result
			if err := json.Unmarshal(message, &result); err != nil {
				log.Warn().Err(err).Msg("invalid result message")
				continue
			}
			// Mark worker available BEFORE resolving the pending slot so the
			// HTTP handler (which returns to the caller) sees an available worker
			// if the caller immediately sends a follow-up request.
			if g.store != nil {
				g.store.SetWorkerAvailableIfProcessing(wc.info.Fingerprint)
			}
			if !g.queue.Resolve(result.ID, &result) {
				log.Warn().Str("id", result.ID).Msg("result for unknown work unit")
			}

		case protocol.TypeError:
			var errMsg protocol.Error
			if err := json.Unmarshal(message, &errMsg); err != nil {
				log.Warn().Err(err).Msg("invalid error message")
				continue
			}
			if !g.queue.Reject(errMsg.ID, &errMsg) {
				log.Warn().Str("id", errMsg.ID).Msg("error for unknown work unit")
			}
			// Mark worker available if it was processing (avoids overwriting paused/idle states)
			if g.store != nil {
				g.store.SetWorkerAvailableIfProcessing(wc.info.Fingerprint)
			}

		case protocol.TypeHeartbeat:
			// Respond with heartbeat ack
			_ = wc.conn.WriteJSON(&protocol.Heartbeat{Type: protocol.TypeHeartbeat})

		case protocol.TypeStatus:
			var statusMsg protocol.Status
			if err := json.Unmarshal(message, &statusMsg); err != nil {
				log.Warn().Err(err).Msg("invalid status message")
				continue
			}
			log.Debug().
				Str("state", statusMsg.State).
				Str("backend_hash", statusMsg.BackendHash).
				Str("backend_version", statusMsg.BackendVersion).
				Str("fingerprint", wc.info.Fingerprint).
				Msg("worker status update")
			// Update LLM hash if provided in status message
			if statusMsg.LLMHash != "" && g.store != nil {
				if err := g.store.SetWorkerLLMHash(wc.info.Fingerprint, statusMsg.LLMHash); err != nil {
					log.Error().Err(err).Msg("failed to update worker LLM hash from status")
				}
				modelSyncStatus := g.store.UpdateWorkerSyncStatus(wc.info.Fingerprint)
				log.Info().
					Str("model_sync_status", modelSyncStatus).
					Str("fingerprint", wc.info.Fingerprint[:8]).
					Msg("model sync status updated")
			}
			// Update backend hash and version if provided — workers report these after installing a new binary.
			if g.store != nil && (statusMsg.BackendHash != "" || statusMsg.BackendVersion != "") {
				if statusMsg.BackendHash != "" {
					if err := g.store.SetWorkerBackendHash(wc.info.Fingerprint, statusMsg.BackendHash); err != nil {
						log.Error().Err(err).Msg("failed to update worker backend hash from status")
					}
				}
				if statusMsg.BackendVersion != "" {
					if err := g.store.SetWorkerBackendVersion(wc.info.Fingerprint, statusMsg.BackendVersion); err != nil {
						log.Error().Err(err).Msg("failed to update worker backend version from status")
					}
				}
				backendSyncStatus := g.store.UpdateWorkerBackendSyncStatus(wc.info.Fingerprint)
				log.Info().
					Str("backend_version", statusMsg.BackendVersion).
					Str("backend_sync_status", backendSyncStatus).
					Str("fingerprint", wc.info.Fingerprint[:8]).
					Msg("backend sync status updated")
			}
			// Store GPU baseline avg only when the worker has given consent —
			// we collect the aggregate (no timeline, no peaks), never raw load curves.
			if statusMsg.GPUAvg > 0 && g.store != nil && g.store.HasConsent(wc.info.Fingerprint) {
				if err := g.store.SetWorkerGPUAvg(wc.info.Fingerprint, statusMsg.GPUAvg); err != nil {
					log.Error().Err(err).Msg("failed to update worker GPU avg")
				}
			}
			if g.store != nil {
				// Prevent workers with withdrawn consent from becoming available
				if (statusMsg.State == "available" || statusMsg.State == "online") && !g.store.HasConsent(wc.info.Fingerprint) {
					log.Warn().
						Str("requested_state", statusMsg.State).
						Str("fingerprint", wc.info.Fingerprint).
						Msg("ignoring status update — consent withdrawn")
					continue
				}
				// Use conditional update to avoid race with consent withdrawal
				g.store.SetWorkerStatusIfNotWithdrawn(wc.info.Fingerprint, statusMsg.State)

				// Log status transition with aggregated state
				syncStatus := g.store.GetWorkerSyncStatus(wc.info.Fingerprint)
				aggregated := store.AggregatedStatus(statusMsg.State, syncStatus)
				log.Debug().
					Str("client_status", statusMsg.State).
					Str("sync_status", syncStatus).
					Str("aggregated", aggregated).
					Str("fingerprint", wc.info.Fingerprint).
					Msg("worker status transition")
			}

		case protocol.TypeLLMHashReport:
			var hashReport protocol.LLMHashReport
			if err := json.Unmarshal(message, &hashReport); err != nil {
				log.Warn().Err(err).Msg("invalid llm_hash_report message")
				continue
			}
			log.Debug().
				Str("llm_hash", hashReport.LLMHash).
				Str("fingerprint", wc.info.Fingerprint).
				Msg("worker LLM hash report received")
			if g.store != nil {
				if err := g.store.SetWorkerLLMHash(wc.info.Fingerprint, hashReport.LLMHash); err != nil {
					log.Error().Err(err).Msg("failed to update worker LLM hash")
				}
				// Recompute sync status after hash update
				syncStatus := g.store.UpdateWorkerSyncStatus(wc.info.Fingerprint)
				log.Info().
					Str("model_sync_status", syncStatus).
					Str("llm_hash", hashReport.LLMHash[:16]+"...").
					Str("fingerprint", wc.info.Fingerprint[:8]).
					Msg("model sync status updated from hash report")
				// Debug: log full aggregated state transition
				var w store.Worker
				clientStatus := "unknown"
				if err := g.store.DB.Select("status").Where("fingerprint = ?", wc.info.Fingerprint).First(&w).Error; err == nil {
					clientStatus = w.Status
				}
				log.Debug().
					Str("client_status", clientStatus).
					Str("sync_status", syncStatus).
					Str("aggregated", store.AggregatedStatus(clientStatus, syncStatus)).
					Str("fingerprint", wc.info.Fingerprint).
					Msg("worker status transition")
			}

		case protocol.TypeInitialSync:
			var syncReq protocol.InitialSync
			if err := json.Unmarshal(message, &syncReq); err != nil {
				log.Warn().Err(err).Msg("invalid initial_sync message")
				continue
			}
			log.Debug().
				Bool("has_binary", syncReq.HasBinary).
				Bool("has_model", syncReq.HasModel).
				Str("role", syncReq.Role).
				Str("fingerprint", wc.info.Fingerprint).
				Msg("worker requesting initial sync")

			if g.store == nil {
				continue
			}

			// Push backend binary if worker doesn't have it and a version is configured.
			if !syncReq.HasBinary {
				if cfg, err := g.store.GetBackendVersionConfig(); err == nil && cfg.DownloadURL != "" {
					downloadURL := backend.ExpandURL(cfg.DownloadURL, cfg.Version, wc.info.OS, wc.info.Arch)
					fallbackURL := fmt.Sprintf("/v1/backend/download?version=%s&os=%s&arch=%s",
						cfg.Version, wc.info.OS, wc.info.Arch)
					log.Info().
						Str("version", cfg.Version).
						Str("fingerprint", wc.info.Fingerprint[:8]).
						Msg("initial_sync: pushing backend binary to worker")
					if err := g.PushBackendUpdate(cfg.Version, downloadURL, fallbackURL, cfg.SHA256); err != nil {
						log.Error().Err(err).Msg("initial_sync: failed to push backend update")
					}
				} else {
					log.Info().Str("fingerprint", wc.info.Fingerprint[:8]).
						Msg("initial_sync: no backend version configured — worker cannot receive llama-server")
				}
			}

			// Push model update when: binary is missing (no running llama-server),
			// model file is missing, or the LLM hash doesn't match.
			// Always push when binary is absent — the model update triggers
			// restartLLM which starts llama-server once the binary is installed.
			if syncReq.Role != "" {
				if rm, err := g.store.GetRoleModel(syncReq.Role); err == nil && rm != nil {
					// Compute expected LLM hash only when the file hash is known.
					// When it is empty (model not yet downloaded by the manager) we
					// still push the ModelUpdate so the client can download the file
					// and report back its own hash.
					llmHash := ""
					workerHash := ""
					if rm.ModelFileHash != "" {
						llmHash = protocol.ComputeLLMHash(syncReq.Role, rm.ModelFileHash)
						var w store.Worker
						if err2 := g.store.DB.Select("llm_hash").Where("fingerprint = ?", wc.info.Fingerprint).First(&w).Error; err2 == nil {
							workerHash = w.LLMHash
						}
					}
					needsModel := !syncReq.HasBinary || !syncReq.HasModel || (llmHash != "" && workerHash != llmHash)
					if needsModel {
						log.Info().
							Str("role", syncReq.Role).
							Str("model", rm.LLMModel).
							Str("filename", rm.ModelFilename).
							Str("fingerprint", wc.info.Fingerprint[:8]).
							Msg("initial_sync: pushing model update to worker")
						if err := g.PushModelUpdate(syncReq.Role, rm.LLMModel, rm.Quantisation,
							rm.ModelFilename, rm.ModelFileHash, llmHash,
							rm.ContextSize, rm.ParallelSlots, rm.GPULayers,
							rm.EndpointType, rm.FlashAttention); err != nil {
							log.Error().Err(err).Msg("initial_sync: failed to push model update")
						}
					} else {
						log.Info().
							Str("role", syncReq.Role).
							Str("fingerprint", wc.info.Fingerprint[:8]).
							Msg("initial_sync: model already in sync, no push needed")
					}
				} else {
					log.Info().Str("role", syncReq.Role).Str("fingerprint", wc.info.Fingerprint[:8]).
						Msg("initial_sync: no model configured for role, skipping")
				}
			}

		default:
			log.Warn().Str("type", string(envelope.Type)).Msg("unknown message type from worker")
		}
	}
}

func (g *Gateway) verifyOrRegisterKey(fingerprint string, pubKey ed25519.PublicKey) bool {
	g.knownKeysMu.Lock()
	defer g.knownKeysMu.Unlock()

	existing, ok := g.knownKeys[fingerprint]
	if !ok {
		// First time seeing this fingerprint — register it
		g.knownKeys[fingerprint] = pubKey
		log.Info().Str("fingerprint", fingerprint).Msg("registered new worker fingerprint")
		return true
	}

	// Verify the public key matches what we have on file
	if !existing.Equal(pubKey) {
		log.Error().Str("fingerprint", fingerprint).Msg("fingerprint/key mismatch")
		return false
	}
	return true
}

// PushModelUpdate sends a model_update message to the connected worker,
// instructing it to switch to the new model configuration.
func (g *Gateway) PushModelUpdate(role, model, quantisation, filename, modelFileHash, llmHash string, contextSize, parallelSlots, gpuLayers int, endpointType string, flashAttention bool) error {
	g.mu.RLock()
	wc := g.worker
	g.mu.RUnlock()

	if wc == nil {
		return fmt.Errorf("no worker connected")
	}

	update := &protocol.ModelUpdate{
		Type:           protocol.TypeModelUpdate,
		Role:           role,
		Model:          model,
		Quantisation:   quantisation,
		Filename:       filename,
		ModelFileHash:  modelFileHash,
		LLMHash:        llmHash,
		ContextSize:    contextSize,
		EndpointType:   endpointType,
		ParallelSlots:  parallelSlots,
		GPULayers:      gpuLayers,
		FlashAttention: flashAttention,
	}

	log.Info().
		Str("role", role).
		Str("model", model).
		Str("quantisation", quantisation).
		Str("filename", filename).
		Str("llm_hash", llmHash).
		Str("fingerprint", wc.info.Fingerprint).
		Msg("pushing model update to worker")

	return wc.conn.WriteJSON(update)
}

// PushBinaryUpdate sends a binary update notification to the connected worker.
func (g *Gateway) PushBinaryUpdate(version, downloadURL, fallbackURL, sha256 string) error {
	g.mu.RLock()
	wc := g.worker
	g.mu.RUnlock()

	if wc == nil {
		return fmt.Errorf("no worker connected")
	}

	update := &protocol.BinaryUpdate{
		Type:        protocol.TypeBinaryUpdate,
		Version:     version,
		DownloadURL: downloadURL,
		FallbackURL: fallbackURL,
		SHA256:      sha256,
	}

	log.Info().
		Str("version", version).
		Str("fingerprint", wc.info.Fingerprint).
		Msg("pushing binary update to worker")

	return wc.conn.WriteJSON(update)
}

// PushBackendUpdate sends a backend update notification to the connected worker.
func (g *Gateway) PushBackendUpdate(version, downloadURL, fallbackURL, sha256 string) error {
	g.mu.RLock()
	wc := g.worker
	g.mu.RUnlock()

	if wc == nil {
		return fmt.Errorf("no worker connected")
	}

	update := &protocol.BackendUpdate{
		Type:        protocol.TypeBackendUpdate,
		Version:     version,
		DownloadURL: downloadURL,
		FallbackURL: fallbackURL,
		SHA256:      sha256,
	}

	log.Info().
		Str("version", version).
		Str("fingerprint", wc.info.Fingerprint).
		Msg("pushing backend update to worker")

	return wc.conn.WriteJSON(update)
}
