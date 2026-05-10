package protocol

import "encoding/json"

// Message types exchanged over WebSocket between manager and worker.
const (
	TypeWorkUnit      = "work_unit"
	TypeResult        = "result"
	TypeError         = "error"
	TypeHeartbeat     = "heartbeat"
	TypeStatus        = "status"
	TypeModelUpdate   = "model_update"    // manager → worker: new model configuration
	TypeLLMHashReport = "llm_hash_report" // worker → manager: report current LLM hash
	TypeBinaryUpdate  = "binary_update"   // manager → worker: new client binary available
	TypeBackendUpdate = "backend_update"  // manager → worker: new backend binary available
)

// Envelope is the top-level WebSocket message wrapper for type routing.
type Envelope struct {
	Type string `json:"type"`
}

// WorkUnit is sent from manager to worker.
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

// Result is sent from worker to manager after processing.
type Result struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Fingerprint      string          `json:"fingerprint"`
	Output           json.RawMessage `json:"output"`
	ProcessingTimeMs int64           `json:"processing_time_ms"`
	Signature        string          `json:"signature"`
}

// Error is sent from worker to manager when processing fails.
type Error struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Error string `json:"error"`
}

// Heartbeat is sent bidirectionally for liveness.
type Heartbeat struct {
	Type string `json:"type"`
}

// Status is sent from worker to manager to report state changes.
type Status struct {
	Type    string `json:"type"`
	State   string `json:"state"`              // "available", "processing", "idle", "updating", "paused", "withdrawn"
	LLMHash string `json:"llm_hash,omitempty"` // worker's current LLM hash
}

// ModelUpdate is sent from manager to worker when a role-model mapping changes.
type ModelUpdate struct {
	Type          string `json:"type"`
	Role          string `json:"role"`
	Model         string `json:"model"`
	Quantisation  string `json:"quantisation"`
	Filename      string `json:"filename"`        // model filename in the model store
	ModelFileHash string `json:"model_file_hash"` // expected SHA256 of the model file
	LLMHash       string `json:"llm_hash"`        // expected llmHash after update
}

// LLMHashReport is sent from worker to manager to confirm the current LLM hash.
type LLMHashReport struct {
	Type    string `json:"type"`
	LLMHash string `json:"llm_hash"`
}

// BinaryUpdate is sent from manager to worker when a new client binary is available.
type BinaryUpdate struct {
	Type        string `json:"type"`
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"` // primary: GitHub release URL
	FallbackURL string `json:"fallback_url"` // fallback: manager proxy endpoint
	SHA256      string `json:"sha256"`
}

// BackendUpdate is sent from manager to worker when a new backend binary is available.
type BackendUpdate struct {
	Type        string `json:"type"`
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	FallbackURL string `json:"fallback_url"` // fallback: manager cached proxy
	SHA256      string `json:"sha256"`
}

// ChatCompletionRequest mirrors the OpenAI API request format.
type ChatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []ChatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

// ChatCompletionResponse mirrors the OpenAI API response format.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *ChatCompletionUsage   `json:"usage,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type ChatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
