package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/manager/store"
)

// ErrorsAPI handles client error reporting endpoints.
type ErrorsAPI struct {
	workerKey string
	store     *store.Store
}

func NewErrorsAPI(workerKey string, s *store.Store) *ErrorsAPI {
	return &ErrorsAPI{
		workerKey: workerKey,
		store:     s,
	}
}

// ErrorReportRequest is the JSON body submitted by clients.
type ErrorReportRequest struct {
	Fingerprint string `json:"fingerprint"`
	Version     string `json:"version"`
	Error       string `json:"error"`
	Stack       string `json:"stack"`
	Timestamp   string `json:"timestamp"`
}

// ErrorsHandler handles POST /v1/errors (submit) and GET /v1/errors (list).
func (a *ErrorsAPI) ErrorsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.handlePost(w, r)
	case http.MethodGet:
		a.handleGet(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *ErrorsAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	// Authenticate with worker key or API key
	authHeader := r.Header.Get("Authorization")
	if authHeader != "Bearer "+a.workerKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	errors, err := a.store.GetClientErrors()
	if err != nil {
		log.Error().Err(err).Msg("failed to query client errors")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	type errorEntry struct {
		ID           uint   `json:"id"`
		Fingerprint  string `json:"fingerprint"`
		Version      string `json:"version"`
		ErrorMessage string `json:"error"`
		Stack        string `json:"stack"`
		ReportedAt   string `json:"reported_at"`
	}

	var result []errorEntry
	for _, e := range errors {
		result = append(result, errorEntry{
			ID:           e.ID,
			Fingerprint:  e.Fingerprint,
			Version:      e.Version,
			ErrorMessage: e.ErrorMessage,
			Stack:        e.Stack,
			ReportedAt:   e.ReportedAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"errors": result,
		"total":  len(result),
	})
}

func (a *ErrorsAPI) handlePost(w http.ResponseWriter, r *http.Request) {
	// Authenticate
	authHeader := r.Header.Get("Authorization")
	if authHeader != "Bearer "+a.workerKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024)) // 64 KB max
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req ErrorReportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Fingerprint == "" || req.Error == "" {
		http.Error(w, "fingerprint and error are required", http.StatusBadRequest)
		return
	}

	reportedAt, err := time.Parse(time.RFC3339, req.Timestamp)
	if err != nil {
		reportedAt = time.Now().UTC()
	}

	if err := a.store.CreateClientError(req.Fingerprint, req.Version, req.Error, req.Stack, reportedAt); err != nil {
		log.Error().Err(err).Str("fingerprint", req.Fingerprint).Msg("failed to store client error")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Warn().
		Str("version", req.Version).
		Str("error", req.Error).
		Str("fingerprint", req.Fingerprint).
		Msg("client error reported")

	w.WriteHeader(http.StatusCreated)
}
