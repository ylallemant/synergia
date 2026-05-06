package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// ComputeLLMHash computes a deterministic hash from a role and the actual model file hash.
// modelFileHash must be the hex-encoded SHA256 of the model file.
func ComputeLLMHash(role, modelFileHash string) string {
	h := sha256.Sum256([]byte(role + ":" + modelFileHash))
	return hex.EncodeToString(h[:])
}

// HashFile computes the SHA256 hex hash of a file on disk.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash file %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Message types exchanged over WebSocket (mirrors cluster-manager/internal/protocol).
const (
	TypeWorkUnit      = "work_unit"
	TypeResult        = "result"
	TypeError         = "error"
	TypeHeartbeat     = "heartbeat"
	TypeStatus        = "status"
	TypeModelUpdate   = "model_update"    // manager → worker: new model configuration
	TypeLLMHashReport = "llm_hash_report" // worker → manager: report current LLM hash
)

// Envelope is the top-level WebSocket message wrapper for type routing.
type Envelope struct {
	Type string `json:"type"`
}

// WorkUnit is received from the cluster manager.
type WorkUnit struct {
	Type     string         `json:"type"`
	ID       string         `json:"id"`
	Model    string         `json:"model"`
	Params   WorkUnitParams `json:"params"`
	Messages []ChatMessage  `json:"messages"`
}

type WorkUnitParams struct {
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Result is sent to the cluster manager after processing.
type Result struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Fingerprint      string          `json:"fingerprint"`
	Output           json.RawMessage `json:"output"`
	ProcessingTimeMs int64           `json:"processing_time_ms"`
	Signature        string          `json:"signature"`
}

// Error is sent to the cluster manager when processing fails.
type Error struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Error string `json:"error"`
}

// Heartbeat is sent bidirectionally for liveness.
type Heartbeat struct {
	Type string `json:"type"`
}

// Status is sent to the cluster manager to report worker state changes.
type Status struct {
	Type    string `json:"type"`
	State   string `json:"state"`              // "available", "idle", "processing"
	LLMHash string `json:"llm_hash,omitempty"` // worker's current LLM hash
}

// ModelUpdate is received from the cluster manager when the role's model configuration changes.
type ModelUpdate struct {
	Type          string `json:"type"`
	Role          string `json:"role"`
	Model         string `json:"model"`
	Quantisation  string `json:"quantisation"`
	Filename      string `json:"filename"`        // model filename to download from manager
	ModelFileHash string `json:"model_file_hash"` // SHA256 hex of the model file (for verification)
	LLMHash       string `json:"llm_hash"`        // expected llmHash after update
}

// LLMHashReport is sent to the cluster manager to confirm the worker's current LLM hash.
type LLMHashReport struct {
	Type    string `json:"type"`
	LLMHash string `json:"llm_hash"`
}

// ChatCompletionRequest is the payload sent to the local llama-server.
type ChatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []ChatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}
