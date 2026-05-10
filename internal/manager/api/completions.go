package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/gateway"
	"github.com/ylallemant/synergia/internal/manager/latency"
	"github.com/ylallemant/synergia/internal/protocol"
	"github.com/ylallemant/synergia/internal/manager/queue"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// CompletionsHandler serves the OpenAI-compatible /v1/chat/completions endpoint.
type CompletionsHandler struct {
	apiKey         string
	gateway        *gateway.Gateway
	queue          *queue.Queue
	store          *store.Store
	timeout        time.Duration
	latencyMonitor *latency.Monitor
}

func NewCompletionsHandler(apiKey string, gw *gateway.Gateway, q *queue.Queue, s *store.Store, timeout time.Duration) *CompletionsHandler {
	return &CompletionsHandler{
		apiKey:  apiKey,
		gateway: gw,
		queue:   q,
		store:   s,
		timeout: timeout,
	}
}

// SetLatencyMonitor attaches the latency monitor for recording samples on completion.
func (h *CompletionsHandler) SetLatencyMonitor(m *latency.Monitor) {
	h.latencyMonitor = m
}

func (h *CompletionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate
	if !h.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Check worker connectivity
	if !h.gateway.HasWorker() {
		writeError(w, http.StatusServiceUnavailable, "no worker connected")
		return
	}

	// Parse request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var req protocol.ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	// Check worker availability (skip for PAUSE trigger — must reach worker to toggle state)
	if !h.gateway.HasAvailableWorker() && !containsPauseTrigger(req.Messages) {
		writeError(w, http.StatusTooManyRequests, "no worker available — all workers are busy, paused, idle, updating, or withdrawn")
		return
	}

	// Build work unit
	unitID := h.queue.NextID()
	workUnit := &protocol.WorkUnit{
		Type:  protocol.TypeWorkUnit,
		ID:    unitID,
		Model: req.Model,
		Params: protocol.WorkUnitParams{
			Temperature:    req.Temperature,
			MaxTokens:      req.MaxTokens,
			ResponseFormat: req.ResponseFormat,
		},
		Messages: req.Messages,
	}

	// Register pending slot
	pending := h.queue.Register(unitID)

	// Dispatch to worker
	if err := h.gateway.Dispatch(workUnit); err != nil {
		h.queue.Cancel(unitID)
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("failed to dispatch: %v", err))
		return
	}

	// Record in DB
	workerFingerprint := ""
	workerRole := ""
	if info := h.gateway.WorkerStatus(); info != nil {
		workerFingerprint = info.Fingerprint
		if h.store != nil {
			if wc, _ := h.store.GetWorkerConfig(info.Fingerprint); wc != nil {
				workerRole = wc.PreferredRole
			}
		}
	}
	if workerRole == "" {
		workerRole = "inference" // default role
	}
	payloadBytes := len(body)
	if h.store != nil {
		_ = h.store.RecordWorkUnit(unitID, workerFingerprint, req.Model)
	}

	log.Info().Str("id", unitID).Str("model", req.Model).Msg("dispatched work unit")

	// Wait for result with timeout
	select {
	case result := <-pending.ResultCh:
		if h.store != nil {
			_ = h.store.CompleteWorkUnit(unitID, result.ProcessingTimeMs)
		}
		if h.latencyMonitor != nil && workerFingerprint != "" {
			h.latencyMonitor.Record(workerFingerprint, workerRole, payloadBytes, result.ProcessingTimeMs)
			log.Debug().
				Str("id", unitID).
				Str("role", workerRole).
				Int("payload_bytes", payloadBytes).
				Int64("latency_ms", result.ProcessingTimeMs).
				Str("fingerprint", workerFingerprint[:12]).
				Msg("latency recorded")
		}
		h.writeResult(w, result, req.Model)

	case errMsg := <-pending.ErrorCh:
		if h.store != nil {
			_ = h.store.FailWorkUnit(unitID, errMsg.Error)
		}
		log.Warn().Str("id", unitID).Str("error", errMsg.Error).Msg("worker returned error")
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("worker error: %s", errMsg.Error))

	case <-time.After(h.timeout):
		h.queue.Cancel(unitID)
		if h.store != nil {
			_ = h.store.TimeoutWorkUnit(unitID)
		}
		log.Warn().Str("id", unitID).Dur("timeout", h.timeout).Msg("work unit timed out")
		writeError(w, http.StatusGatewayTimeout, "worker did not respond in time")
	}
}

func (h *CompletionsHandler) writeResult(w http.ResponseWriter, result *protocol.Result, model string) {
	// The worker returns the raw OpenAI-style output — try to use it directly
	// If the output is already a full ChatCompletionResponse, pass it through.
	// Otherwise, wrap a raw assistant message.
	var response protocol.ChatCompletionResponse
	if err := json.Unmarshal(result.Output, &response); err == nil && len(response.Choices) > 0 {
		// Worker returned a full response object — pass through
		response.ID = result.ID
		response.Object = "chat.completion"
		response.Created = time.Now().Unix()
		response.Model = model
	} else {
		// Try to interpret output as just the choices array or a single message
		// Fallback: wrap output as-is in a response envelope
		response = protocol.ChatCompletionResponse{
			ID:      result.ID,
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []protocol.ChatCompletionChoice{
				{
					Index: 0,
					Message: protocol.ChatMessage{
						Role:    "assistant",
						Content: string(result.Output),
					},
					FinishReason: "stop",
				},
			},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	log.Info().
		Str("id", result.ID).
		Int64("processing_time_ms", result.ProcessingTimeMs).
		Str("fingerprint", result.Fingerprint).
		Msg("returned result")
}

func (h *CompletionsHandler) authenticate(r *http.Request) bool {
	// Support both "Authorization: Bearer <key>" and "X-API-Key: <key>"
	if apiKey := r.Header.Get("X-API-Key"); apiKey == h.apiKey {
		return true
	}
	auth := r.Header.Get("Authorization")
	return strings.TrimPrefix(auth, "Bearer ") == h.apiKey
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
			"code":    status,
		},
	})
}

const pauseTrigger = "##############PAUSE##############"

// containsPauseTrigger checks if any message contains the PAUSE trigger.
// The PAUSE trigger must bypass availability checks to reach the worker and toggle its state.
func containsPauseTrigger(messages []protocol.ChatMessage) bool {
	for _, msg := range messages {
		if strings.Contains(msg.Content, pauseTrigger) {
			return true
		}
	}
	return false
}
