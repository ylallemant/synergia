package api

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/gateway"
	"github.com/ylallemant/synergia/internal/manager/latency"
	"github.com/ylallemant/synergia/internal/manager/protocol"
	"github.com/ylallemant/synergia/internal/manager/queue"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// batchObject is the OpenAI-compatible batch response returned by all endpoints.
type batchObject struct {
	ID            string          `json:"id"`
	Object        string          `json:"object"` // always "batch"
	Endpoint      string          `json:"endpoint"`
	Status        string          `json:"status"` // pending, in_progress, completed, failed, cancelled, expired
	Model         string          `json:"model"`
	CreatedAt     int64           `json:"created_at"`
	InProgressAt  *int64          `json:"in_progress_at"`
	CompletedAt   *int64          `json:"completed_at"`
	FailedAt      *int64          `json:"failed_at"`
	CancelledAt   *int64          `json:"cancelled_at"`
	RequestCounts requestCounts   `json:"request_counts"`
	Output        json.RawMessage `json:"output,omitempty"`
	Error         *batchError     `json:"error,omitempty"`
}

type requestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

type batchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BatchHandler serves the /v1/batches endpoint (OpenAI-compatible batch API).
// Requests are stored in the database and processed FIFO when a worker becomes available.
type BatchHandler struct {
	apiKey         string
	gateway        *gateway.Gateway
	queue          *queue.Queue
	store          *store.Store
	timeout        time.Duration
	latencyMonitor *latency.Monitor
	development    bool // sequential processing with random delays
	stopCh         chan struct{}
}

func NewBatchHandler(apiKey string, gw *gateway.Gateway, q *queue.Queue, s *store.Store, timeout time.Duration, development bool) *BatchHandler {
	h := &BatchHandler{
		apiKey:      apiKey,
		gateway:     gw,
		queue:       q,
		store:       s,
		timeout:     timeout,
		development: development,
		stopCh:      make(chan struct{}),
	}
	if development {
		log.Info().Msg("batch handler: development mode — sequential processing with random delays")
	}
	go h.processingLoop()
	return h
}

// SetLatencyMonitor attaches the latency monitor for recording samples on batch completion.
func (h *BatchHandler) SetLatencyMonitor(m *latency.Monitor) {
	h.latencyMonitor = m
}

// Stop terminates the background processing loop.
func (h *BatchHandler) Stop() {
	close(h.stopCh)
}

// ServeHTTP routes requests for /v1/batches and /v1/batches/{id}[/cancel].
func (h *BatchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse path: /v1/batches, /v1/batches/{id}, /v1/batches/{id}/cancel
	path := strings.TrimPrefix(r.URL.Path, "/v1/batches")
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "" && r.Method == http.MethodPost:
		h.createBatch(w, r)
	case path == "" && r.Method == http.MethodGet:
		h.listBatches(w, r)
	case strings.HasSuffix(path, "/cancel") && r.Method == http.MethodPost:
		batchID := strings.TrimSuffix(path, "/cancel")
		h.cancelBatch(w, r, batchID)
	case r.Method == http.MethodGet:
		h.retrieveBatch(w, r, path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// POST /v1/batches — create a new batch request.
func (h *BatchHandler) createBatch(w http.ResponseWriter, r *http.Request) {
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

	requestID := fmt.Sprintf("batch_%d_%d", time.Now().UnixMilli(), time.Now().UnixNano()%10000)

	if err := h.store.CreateBatchRequest(requestID, req.Model, string(body)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue request")
		return
	}

	log.Info().Str("id", requestID).Str("model", req.Model).Msg("batch request enqueued")

	now := time.Now().Unix()
	obj := batchObject{
		ID:        requestID,
		Object:    "batch",
		Endpoint:  "/v1/chat/completions",
		Status:    "pending",
		Model:     req.Model,
		CreatedAt: now,
		RequestCounts: requestCounts{
			Total:     1,
			Completed: 0,
			Failed:    0,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(obj)
}

// GET /v1/batches — list batch requests (most recent first).
func (h *BatchHandler) listBatches(w http.ResponseWriter, r *http.Request) {
	requests, err := h.store.ListBatchRequests(100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list batch requests")
		return
	}

	objects := make([]batchObject, 0, len(requests))
	for _, br := range requests {
		objects = append(objects, h.toBatchObject(&br))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   objects,
	})
}

// GET /v1/batches/{id} — retrieve a specific batch by ID.
func (h *BatchHandler) retrieveBatch(w http.ResponseWriter, r *http.Request, batchID string) {
	br, err := h.store.GetBatchRequest(batchID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query batch request")
		return
	}
	if br == nil {
		writeError(w, http.StatusNotFound, "batch not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.toBatchObject(br))
}

// POST /v1/batches/{id}/cancel — cancel a pending batch request.
func (h *BatchHandler) cancelBatch(w http.ResponseWriter, r *http.Request, batchID string) {
	br, err := h.store.GetBatchRequest(batchID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query batch request")
		return
	}
	if br == nil {
		writeError(w, http.StatusNotFound, "batch not found")
		return
	}

	if br.Status != "pending" {
		writeError(w, http.StatusConflict, fmt.Sprintf("cannot cancel batch in status %q", br.Status))
		return
	}

	if err := h.store.FailBatchRequest(batchID, "cancelled by user"); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to cancel batch")
		return
	}

	// Refresh after update
	br, _ = h.store.GetBatchRequest(batchID)
	if br == nil {
		writeError(w, http.StatusInternalServerError, "failed to retrieve cancelled batch")
		return
	}
	// Override status to "cancelled" in response
	obj := h.toBatchObject(br)
	obj.Status = "cancelled"
	now := time.Now().Unix()
	obj.CancelledAt = &now

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(obj)
}

// toBatchObject converts a store.BatchRequest to an OpenAI-style batchObject.
func (h *BatchHandler) toBatchObject(br *store.BatchRequest) batchObject {
	obj := batchObject{
		ID:        br.RequestID,
		Object:    "batch",
		Endpoint:  "/v1/chat/completions",
		Model:     br.LLMModel,
		CreatedAt: br.CreatedAt.Unix(),
		RequestCounts: requestCounts{
			Total: 1,
		},
	}

	switch br.Status {
	case "pending":
		obj.Status = "pending"
	case "processing":
		obj.Status = "in_progress"
		inProgress := br.UpdatedAt.Unix()
		obj.InProgressAt = &inProgress
	case "completed":
		obj.Status = "completed"
		completed := br.UpdatedAt.Unix()
		obj.CompletedAt = &completed
		obj.RequestCounts.Completed = 1
		if br.Result != "" {
			obj.Output = json.RawMessage(br.Result)
		}
	case "failed":
		obj.Status = "failed"
		failed := br.UpdatedAt.Unix()
		obj.FailedAt = &failed
		obj.RequestCounts.Failed = 1
		if br.ErrMessage != "" {
			obj.Error = &batchError{
				Code:    "processing_failed",
				Message: br.ErrMessage,
			}
		}
	}

	return obj
}

// processingLoop runs in the background, polling for pending batch requests
// and dispatching them when a worker is available.
func (h *BatchHandler) processingLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.processNext()
		}
	}
}

func (h *BatchHandler) processNext() {
	// Only process if a worker is available
	if !h.gateway.HasAvailableWorker() {
		return
	}

	pending, err := h.store.GetPendingBatchRequests(1)
	if err != nil || len(pending) == 0 {
		return
	}

	// Development mode: add random 1-5s delay before processing
	if h.development {
		delay := time.Duration(1+rand.Intn(5)) * time.Second
		log.Debug().Dur("delay", delay).Msg("batch development mode: adding random delay")
		select {
		case <-time.After(delay):
		case <-h.stopCh:
			return
		}
	}

	br := pending[0]
	if err := h.store.SetBatchRequestProcessing(br.RequestID); err != nil {
		return
	}

	var req protocol.ChatCompletionRequest
	if err := json.Unmarshal([]byte(br.Payload), &req); err != nil {
		_ = h.store.FailBatchRequest(br.RequestID, fmt.Sprintf("invalid stored payload: %v", err))
		return
	}

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

	pending2 := h.queue.Register(unitID)

	if err := h.gateway.Dispatch(workUnit); err != nil {
		h.queue.Cancel(unitID)
		_ = h.store.FailBatchRequest(br.RequestID, fmt.Sprintf("dispatch failed: %v", err))
		return
	}

	workerFingerprint := ""
	workerRole := ""
	if info := h.gateway.WorkerStatus(); info != nil {
		workerFingerprint = info.Fingerprint
		if wc, _ := h.store.GetWorkerConfig(info.Fingerprint); wc != nil {
			workerRole = wc.PreferredRole
		}
	}
	if workerRole == "" {
		workerRole = "inference"
	}
	payloadBytes := len(br.Payload)

	if h.store != nil {
		_ = h.store.RecordWorkUnit(unitID, workerFingerprint, req.Model)
	}

	log.Info().Str("id", unitID).Str("batch_id", br.RequestID).Str("model", req.Model).Msg("batch request dispatched")

	// Wait for result
	select {
	case result := <-pending2.ResultCh:
		if h.store != nil {
			_ = h.store.CompleteWorkUnit(unitID, result.ProcessingTimeMs)
		}
		if h.latencyMonitor != nil && workerFingerprint != "" {
			h.latencyMonitor.Record(workerFingerprint, workerRole, payloadBytes, result.ProcessingTimeMs)
		}
		// Build response
		respJSON := h.buildBatchResult(result, req.Model)
		_ = h.store.CompleteBatchRequest(br.RequestID, string(respJSON))
		log.Info().Str("batch_id", br.RequestID).Int64("processing_time_ms", result.ProcessingTimeMs).Msg("batch request completed")

	case errMsg := <-pending2.ErrorCh:
		if h.store != nil {
			_ = h.store.FailWorkUnit(unitID, errMsg.Error)
		}
		_ = h.store.FailBatchRequest(br.RequestID, errMsg.Error)
		log.Warn().Str("batch_id", br.RequestID).Str("error", errMsg.Error).Msg("batch request failed")

	case <-time.After(h.timeout):
		h.queue.Cancel(unitID)
		if h.store != nil {
			_ = h.store.TimeoutWorkUnit(unitID)
		}
		_ = h.store.FailBatchRequest(br.RequestID, "worker did not respond in time")
		log.Warn().Str("batch_id", br.RequestID).Msg("batch request timed out")

	case <-h.stopCh:
		h.queue.Cancel(unitID)
		return
	}
}

func (h *BatchHandler) buildBatchResult(result *protocol.Result, model string) []byte {
	var response protocol.ChatCompletionResponse
	if err := json.Unmarshal(result.Output, &response); err == nil && len(response.Choices) > 0 {
		response.ID = result.ID
		response.Object = "chat.completion"
		response.Created = time.Now().Unix()
		response.Model = model
	} else {
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
	data, _ := json.Marshal(response)
	return data
}

func (h *BatchHandler) authenticate(r *http.Request) bool {
	if apiKey := r.Header.Get("X-API-Key"); apiKey == h.apiKey {
		return true
	}
	auth := r.Header.Get("Authorization")
	return strings.TrimPrefix(auth, "Bearer ") == h.apiKey
}
