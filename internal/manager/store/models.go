package store

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"gorm.io/gorm"
)

// ComputeLLMHash computes a deterministic hash from a role and the actual model file hash.
// modelFileHash must be the hex-encoded SHA256 of the model file — this is the only
// tamper-proof information that proves the worker has the correct model loaded.
func ComputeLLMHash(role, modelFileHash string) string {
	h := sha256.Sum256([]byte(role + ":" + modelFileHash))
	return hex.EncodeToString(h[:])
}

// Worker represents a registered worker in the cluster.
type Worker struct {
	gorm.Model
	Fingerprint       string `gorm:"uniqueIndex;size:64"`
	PublicKey         string `gorm:"size:88"` // base64-encoded Ed25519 public key
	LLMModel          string `gorm:"column:llm_model"`
	Quantisation      string
	ClientVersion     string `gorm:"size:32"`
	OS                string `gorm:"size:20"`
	Arch              string `gorm:"size:20"`
	LLMHash           string `gorm:"size:64"`                     // SHA256 hash of role:model_file_hash reported by the worker
	SyncStatus        string `gorm:"size:20;default:out-of-sync"` // synced, out-of-sync (manager-derived from llmHash comparison)
	BackendHash       string `gorm:"size:64"`                     // SHA256 of the llama-server binary
	BinarySyncStatus  string `gorm:"size:20;default:out-of-sync"` // synced, out-of-sync (version comparison)
	BackendSyncStatus string `gorm:"size:20;default:out-of-sync"` // synced, out-of-sync (backend hash comparison)
	TrustScore        int    `gorm:"default:0"`
	TotalRequests     int64  `gorm:"default:0"`
	TotalLatencyMs    int64  `gorm:"default:0"`
	LastSeenAt        time.Time
	Status            string `gorm:"default:offline;size:20"` // available, processing, updating, paused, idle, withdrawn, offline
}

// WorkUnit records a dispatched work unit and its outcome.
type WorkUnit struct {
	gorm.Model
	UnitID            string `gorm:"uniqueIndex;size:64"`
	WorkerFingerprint string `gorm:"index;size:64"`
	LLMModel          string `gorm:"column:llm_model"`
	Status            string `gorm:"index;size:20"` // pending, dispatched, completed, failed, timeout
	ProcessingTimeMs  int64
	ErrorMessage      string
	CreatedAt         time.Time
	CompletedAt       *time.Time
}

// WorkerConsent tracks whether a worker has accepted the data collection terms.
// The manager will not dispatch work units to workers that have not consented.
type WorkerConsent struct {
	gorm.Model
	Fingerprint       string `gorm:"uniqueIndex;size:64"`
	Accepted          bool   `gorm:"default:false"`
	AcceptedAt        *time.Time
	HardwareStats     bool `gorm:"default:false"` // consent to collect hardware statistics
	ConfigPreferences bool `gorm:"default:false"` // consent to store configuration preferences
	// Hardware info synced from the client
	HwOS           string `gorm:"size:32"`
	HwOSVer        string `gorm:"size:64"`
	HwGPU          string `gorm:"size:128"`
	HwGPUDriver    string `gorm:"size:64"`
	HwGPUDriverVer string `gorm:"size:64"`
	HwVRAMMB       int
	HwCPU          string `gorm:"size:128"`
	HwCPUCores     int
	HwRAMMB        int
}

// WorkerConfig stores configuration preferences for a worker, synced from the client.
type WorkerConfig struct {
	gorm.Model
	Fingerprint   string `gorm:"uniqueIndex;size:64"`
	PreferredRole string `gorm:"size:40"` // e.g., "inference", "embedding", "any"
	Nickname      string `gorm:"size:64"` // optional display name for community leaderboard
}

// HardwareInfo is used to pass hardware stats when setting consent.
type HardwareInfo struct {
	OS               string
	OSVer            string
	GPU              string
	GPUDriver        string
	GPUDriverVersion string
	VRAMMB           int
	CPU              string
	CPUCores         int
	RAMMB            int
}

// BrandingConfig stores customizable CSS served to worker dashboards.
type BrandingConfig struct {
	ID        uint   `gorm:"primaryKey"`
	CSS       string `gorm:"type:text"`
	UpdatedAt time.Time
}

// RoleModel maps a cluster role to its required model and minimum VRAM.
// The manager uses this to determine which roles a worker can assume.
type RoleModel struct {
	gorm.Model
	Role          string `gorm:"uniqueIndex;size:40"`       // e.g., "embedding", "inference", "ingestion"
	LLMModel      string `gorm:"column:llm_model;size:128"` // e.g., "mistral-small-3.1-24b-instruct"
	Quantisation  string `gorm:"size:20"`                   // e.g., "Q4_K_M"
	ModelFilename string `gorm:"size:256"`                  // GGUF filename served by model store
	ModelFileHash string `gorm:"size:64"`                   // SHA256 hex of the model file (central truth)
	MinVRAMMB     int    `gorm:"column:min_vram_mb"`        // minimum VRAM in MB required to run this model
	Description   string `gorm:"size:256"`                  // human-readable description shown in UI
}

// ClientError stores errors reported by workers.
type ClientError struct {
	gorm.Model
	Fingerprint  string `gorm:"index;size:64"`
	Version      string `gorm:"size:32"`
	ErrorMessage string `gorm:"type:text"`
	Stack        string `gorm:"type:text"`
	ReportedAt   time.Time
}

// BatchRequest stores a queued request for asynchronous processing (like Scaleway batch API).
// Requests are enqueued when no worker is immediately available and processed in FIFO order.
type BatchRequest struct {
	gorm.Model
	RequestID  string `gorm:"uniqueIndex;size:64"`
	LLMModel   string `gorm:"column:llm_model;size:128"`
	Payload    string `gorm:"type:text"`                     // JSON-encoded ChatCompletionRequest
	Status     string `gorm:"index;size:20;default:pending"` // pending, processing, completed, failed
	Result     string `gorm:"type:text"`                     // JSON-encoded response (set on completion)
	ErrMessage string `gorm:"type:text"`                     // error message (set on failure)
}

// LatencySample records a single latency observation for a completed work unit.
type LatencySample struct {
	ID           uint   `gorm:"primaryKey"`
	Fingerprint  string `gorm:"index:idx_latency_samples_role_created;index:idx_latency_samples_fp;size:64"`
	Role         string `gorm:"index:idx_latency_samples_role_created;index:idx_latency_samples_role_payload;size:40"`
	PayloadBytes int    `gorm:"index:idx_latency_samples_role_payload"`
	LatencyMs    int64
	CreatedAt    time.Time `gorm:"index:idx_latency_samples_role_created"`
}

// LatencyHourlyStat stores per-role hourly payload size aggregates.
type LatencyHourlyStat struct {
	ID               uint      `gorm:"primaryKey"`
	Role             string    `gorm:"uniqueIndex:idx_hourly_role_hour;size:40"`
	Hour             time.Time `gorm:"uniqueIndex:idx_hourly_role_hour"`
	Count            int
	MinPayloadBytes  int
	MaxPayloadBytes  int
	MeanPayloadBytes int
}

// ClientVersionConfig stores the centrally managed target client version.
type ClientVersionConfig struct {
	ID                uint   `gorm:"primaryKey"`
	TargetVersion     string `gorm:"size:32"`
	RolloutMode       string `gorm:"size:20;default:all"` // "all" or "percentage"
	RolloutPercentage int    `gorm:"default:100"`         // 0-100, used when mode=percentage
	UpdatedAt         time.Time
}

// BackendVersionConfig stores the centrally managed target backend (llama-server) version.
type BackendVersionConfig struct {
	ID          uint   `gorm:"primaryKey"`
	Name        string `gorm:"size:64"`  // backend name (e.g. "llama.cpp")
	Version     string `gorm:"size:64"`  // e.g. "b5170"
	DownloadURL string `gorm:"size:512"` // URL template: {version}, {os}, {arch} placeholders
	SHA256      string `gorm:"size:64"`  // expected hash of the binary
	UpdatedAt   time.Time
}
