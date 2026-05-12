package store

import (
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/protocol"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store provides persistent storage for the cluster manager.
type Store struct {
	DB *gorm.DB
}

// Open initializes the database using SQLite (file path) and runs auto-migrations.
func Open(dbPath string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database %q: %w", dbPath, err)
	}

	return migrate(db, "sqlite", dbPath)
}

// OpenPostgres initializes the database using a PostgreSQL DSN and runs auto-migrations.
func OpenPostgres(dsn string) (*Store, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres database: %w", err)
	}

	return migrate(db, "postgres", dsn)
}

func migrate(db *gorm.DB, driver, info string) (*Store, error) {
	if err := db.AutoMigrate(&Worker{}, &WorkUnit{}, &WorkerConsent{}, &WorkerConfig{}, &BrandingConfig{}, &RoleModel{}, &ClientError{}, &BatchRequest{}, &LatencySample{}, &LatencyHourlyStat{}, &ClientVersionConfig{}, &BackendVersionConfig{}, &OidcConfig{}, &WorkerAuthConfig{}, &SystemSetting{}); err != nil {
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	log.Info().Str("driver", driver).Msg("database initialized")
	return &Store{DB: db}, nil
}

// UpsertWorker creates or updates a worker record on connection.
func (s *Store) UpsertWorker(fingerprint, publicKey, model, quantisation, clientVersion, os, arch string) error {
	var worker Worker
	result := s.DB.Where("fingerprint = ?", fingerprint).First(&worker)

	if result.Error == gorm.ErrRecordNotFound {
		worker = Worker{
			Fingerprint:   fingerprint,
			PublicKey:     publicKey,
			LLMModel:      model,
			Quantisation:  quantisation,
			ClientVersion: clientVersion,
			OS:            os,
			Arch:          arch,
			TrustScore:    0,
			LastSeenAt:    time.Now(),
			Status:        "online",
		}
		return s.DB.Create(&worker).Error
	}

	if result.Error != nil {
		return result.Error
	}

	return s.DB.Model(&worker).Updates(map[string]any{
		"llm_model":      model,
		"quantisation":   quantisation,
		"client_version": clientVersion,
		"os":             os,
		"arch":           arch,
		"last_seen_at":   time.Now(),
	}).Error
}

// setWorkerOnlineIfAllowed sets status to "online" unless consent has been withdrawn.
// Called after UpsertWorker during connection setup.
func (s *Store) SetWorkerOnlineIfAllowed(fingerprint string) {
	s.DB.Model(&Worker{}).
		Where("fingerprint = ? AND status != ?", fingerprint, "withdrawn").
		Update("status", "online")
}

// SetWorkerOffline marks a worker as offline.
// A deleted worker's status is not overwritten — the record is kept as-is.
func (s *Store) SetWorkerOffline(fingerprint string) error {
	return s.DB.Model(&Worker{}).
		Where("fingerprint = ? AND status != ?", fingerprint, "deleted").
		Update("status", "offline").Error
}

// SetWorkerDeleted marks a worker as permanently uninstalled.
// The record is kept in the database for historical reference; the status will
// not be overwritten by a subsequent disconnect (SetWorkerOffline guards it).
// If the same worker reconnects later (reinstall), UpsertWorker sets it back
// to "online" as part of the normal handshake.
func (s *Store) SetWorkerDeleted(fingerprint string) error {
	return s.DB.Model(&Worker{}).
		Where("fingerprint = ?", fingerprint).
		Update("status", "deleted").Error
}

// SetWorkerStatus updates the worker's status (e.g., "available", "busy", "processing").
func (s *Store) SetWorkerStatus(fingerprint, status string) error {
	return s.DB.Model(&Worker{}).
		Where("fingerprint = ?", fingerprint).
		Update("status", status).Error
}

// SetWorkerAvailableIfProcessing sets the worker to "available" only if currently "processing".
// This avoids overwriting intentional states like "paused" or "busy".
func (s *Store) SetWorkerAvailableIfProcessing(fingerprint string) {
	s.DB.Model(&Worker{}).
		Where("fingerprint = ? AND status = ?", fingerprint, "processing").
		Update("status", "available")
}

// SetWorkerAvailableIfWithdrawn sets the worker to "available" only if currently "withdrawn".
// Called when consent is re-accepted.
func (s *Store) SetWorkerAvailableIfWithdrawn(fingerprint string) {
	s.DB.Model(&Worker{}).
		Where("fingerprint = ? AND status = ?", fingerprint, "withdrawn").
		Update("status", "available")
}

// SetWorkerStatusIfNotWithdrawn updates the worker's status only if it is not currently "withdrawn".
// This prevents race conditions where a WebSocket status update could overwrite a consent withdrawal.
func (s *Store) SetWorkerStatusIfNotWithdrawn(fingerprint, status string) {
	s.DB.Model(&Worker{}).
		Where("fingerprint = ? AND status != ?", fingerprint, "withdrawn").
		Update("status", status)
}

// GetWorker retrieves a worker by fingerprint.
func (s *Store) GetWorker(fingerprint string) (*Worker, error) {
	var worker Worker
	result := s.DB.Where("fingerprint = ?", fingerprint).First(&worker)
	if result.Error != nil {
		return nil, result.Error
	}
	return &worker, nil
}

// RecordWorkUnit creates a work unit record.
func (s *Store) RecordWorkUnit(unitID, workerFingerprint, model string) error {
	wu := WorkUnit{
		UnitID:            unitID,
		WorkerFingerprint: workerFingerprint,
		LLMModel:          model,
		Status:            "dispatched",
	}
	return s.DB.Create(&wu).Error
}

// CompleteWorkUnit marks a work unit as completed.
func (s *Store) CompleteWorkUnit(unitID string, processingTimeMs int64) error {
	now := time.Now()
	return s.DB.Model(&WorkUnit{}).
		Where("unit_id = ?", unitID).
		Updates(map[string]any{
			"status":             "completed",
			"processing_time_ms": processingTimeMs,
			"completed_at":       &now,
		}).Error
}

// FailWorkUnit marks a work unit as failed.
func (s *Store) FailWorkUnit(unitID, errorMessage string) error {
	now := time.Now()
	return s.DB.Model(&WorkUnit{}).
		Where("unit_id = ?", unitID).
		Updates(map[string]any{
			"status":        "failed",
			"error_message": errorMessage,
			"completed_at":  &now,
		}).Error
}

// TimeoutWorkUnit marks a work unit as timed out.
func (s *Store) TimeoutWorkUnit(unitID string) error {
	now := time.Now()
	return s.DB.Model(&WorkUnit{}).
		Where("unit_id = ?", unitID).
		Updates(map[string]any{
			"status":       "timeout",
			"completed_at": &now,
		}).Error
}

// GetConsent retrieves the consent record for a worker, or nil if none exists.
func (s *Store) GetConsent(fingerprint string) (*WorkerConsent, error) {
	var consent WorkerConsent
	result := s.DB.Where("fingerprint = ?", fingerprint).First(&consent)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &consent, nil
}

// SetConsent creates or updates a worker's consent record.
func (s *Store) SetConsent(fingerprint string, accepted, hardwareStats, configPreferences bool, hw *HardwareInfo) error {
	var consent WorkerConsent
	result := s.DB.Where("fingerprint = ?", fingerprint).First(&consent)

	now := time.Now()
	if result.Error == gorm.ErrRecordNotFound {
		consent = WorkerConsent{
			Fingerprint:       fingerprint,
			Accepted:          accepted,
			HardwareStats:     hardwareStats,
			ConfigPreferences: configPreferences,
		}
		if accepted {
			consent.AcceptedAt = &now
		}
		if hw != nil {
			consent.HwOS = hw.OS
			consent.HwOSVer = hw.OSVer
			consent.HwGPU = hw.GPU
			consent.HwGPUDriver = hw.GPUDriver
			consent.HwGPUDriverVer = hw.GPUDriverVersion
			consent.HwVRAMMB = hw.VRAMMB
			consent.HwCPU = hw.CPU
			consent.HwCPUCores = hw.CPUCores
			consent.HwRAMMB = hw.RAMMB
		}
		return s.DB.Create(&consent).Error
	}
	if result.Error != nil {
		return result.Error
	}

	updates := map[string]any{
		"accepted":           accepted,
		"hardware_stats":     hardwareStats,
		"config_preferences": configPreferences,
	}
	if accepted && consent.AcceptedAt == nil {
		updates["accepted_at"] = &now
	}
	if hw != nil {
		updates["hw_os"] = hw.OS
		updates["hw_os_ver"] = hw.OSVer
		updates["hw_gpu"] = hw.GPU
		updates["hw_gpu_driver"] = hw.GPUDriver
		updates["hw_gpu_driver_ver"] = hw.GPUDriverVersion
		updates["hw_v_ram_mb"] = hw.VRAMMB
		updates["hw_cpu"] = hw.CPU
		updates["hw_cpu_cores"] = hw.CPUCores
		updates["hw_ram_mb"] = hw.RAMMB
	}
	return s.DB.Model(&consent).Updates(updates).Error
}

// HasConsent returns true if the worker has accepted the data collection terms.
func (s *Store) HasConsent(fingerprint string) bool {
	var consent WorkerConsent
	result := s.DB.Where("fingerprint = ? AND accepted = ?", fingerprint, true).First(&consent)
	return result.Error == nil
}

// GetWorkerConfig retrieves the configuration for a worker, or nil if none exists.
func (s *Store) GetWorkerConfig(fingerprint string) (*WorkerConfig, error) {
	var config WorkerConfig
	result := s.DB.Where("fingerprint = ?", fingerprint).First(&config)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &config, nil
}

// SetWorkerConfig creates or updates a worker's configuration.
func (s *Store) SetWorkerConfig(fingerprint, preferredRole, nickname string) error {
	var config WorkerConfig
	result := s.DB.Where("fingerprint = ?", fingerprint).First(&config)

	if result.Error == gorm.ErrRecordNotFound {
		config = WorkerConfig{
			Fingerprint:   fingerprint,
			PreferredRole: preferredRole,
			Nickname:      nickname,
		}
		return s.DB.Create(&config).Error
	}
	if result.Error != nil {
		return result.Error
	}

	return s.DB.Model(&config).Updates(map[string]any{
		"preferred_role": preferredRole,
		"nickname":       nickname,
	}).Error
}

// GetRoleModels returns all configured role-model mappings.
func (s *Store) GetRoleModels() ([]RoleModel, error) {
	var roles []RoleModel
	result := s.DB.Order("min_vram_mb ASC").Find(&roles)
	return roles, result.Error
}

// UpsertRoleModel creates or updates a role-model mapping.
func (s *Store) UpsertRoleModel(role, model, quantisation, modelFilename, modelFileHash string, minVRAMMB int, description string) error {
	var rm RoleModel
	result := s.DB.Where("role = ?", role).First(&rm)

	if result.Error == gorm.ErrRecordNotFound {
		rm = RoleModel{
			Role:          role,
			LLMModel:      model,
			Quantisation:  quantisation,
			ModelFilename: modelFilename,
			ModelFileHash: modelFileHash,
			MinVRAMMB:     minVRAMMB,
			Description:   description,
		}
		return s.DB.Create(&rm).Error
	}
	if result.Error != nil {
		return result.Error
	}

	return s.DB.Model(&rm).Updates(map[string]any{
		"llm_model":       model,
		"quantisation":    quantisation,
		"model_filename":  modelFilename,
		"model_file_hash": modelFileHash,
		"min_vram_mb":     minVRAMMB,
		"description":     description,
	}).Error
}

// SetRoleLlamaConfig updates the llama-server operational parameters for an existing role.
// Called separately from UpsertRoleModel so seed callers are unaffected.
func (s *Store) SetRoleLlamaConfig(role string, contextSize, parallelSlots, gpuLayers int, endpointType string, flashAttention bool) error {
	return s.DB.Model(&RoleModel{}).Where("role = ?", role).Updates(map[string]any{
		"context_size":    contextSize,
		"parallel_slots":  parallelSlots,
		"gpu_layers":      gpuLayers,
		"endpoint_type":   endpointType,
		"flash_attention": flashAttention,
	}).Error
}

// DeleteRoleModel removes a role-model mapping by role name.
func (s *Store) DeleteRoleModel(role string) error {
	return s.DB.Where("role = ?", role).Delete(&RoleModel{}).Error
}

// SeedTestRoles populates role-model mappings suitable for integration testing
// (tiny model, minimal VRAM requirement).
func (s *Store) SeedTestRoles() error {
	testRoles := []RoleModel{
		{Role: "embedding", LLMModel: "SmolLM2-135M-Instruct", Quantisation: "Q4_K_M", ModelFilename: "SmolLM2-135M-Instruct-Q4_K_M.gguf", MinVRAMMB: 512, Description: "Vector embeddings (test mode — minimal model)"},
		{Role: "inference", LLMModel: "SmolLM2-135M-Instruct", Quantisation: "Q4_K_M", ModelFilename: "SmolLM2-135M-Instruct-Q4_K_M.gguf", MinVRAMMB: 512, Description: "Query inference (test mode — minimal model)"},
		{Role: "ingestion", LLMModel: "SmolLM2-135M-Instruct", Quantisation: "Q4_K_M", ModelFilename: "SmolLM2-135M-Instruct-Q4_K_M.gguf", MinVRAMMB: 512, Description: "Document ingestion (test mode — minimal model)"},
		{Role: "tester", LLMModel: "SmolLM2-135M-Instruct", Quantisation: "Q4_K_M", ModelFilename: "SmolLM2-135M-Instruct-Q4_K_M.gguf", MinVRAMMB: 512, Description: "Connectivity testing — minimal model for any hardware"},
	}
	for _, r := range testRoles {
		if err := s.UpsertRoleModel(r.Role, r.LLMModel, r.Quantisation, r.ModelFilename, r.ModelFileHash, r.MinVRAMMB, r.Description); err != nil {
			return err
		}
	}
	return nil
}

// SeedProductionRoles populates role-model mappings for production use
// based on the LLM comparison document.
func (s *Store) SeedProductionRoles() error {
	prodRoles := []RoleModel{
		{Role: "embedding", LLMModel: "bge-m3", Quantisation: "Q4_K_M", ModelFilename: "bge-m3-Q4_K_M.gguf", MinVRAMMB: 4096, Description: "Vector embeddings (bge-m3, ~2 GB model + KV cache)"},
		{Role: "inference", LLMModel: "mistral-nemo-12b-instruct", Quantisation: "Q4_K_M", ModelFilename: "mistral-nemo-12b-instruct-Q4_K_M.gguf", MinVRAMMB: 10240, Description: "Query inference (Mistral Nemo 12B, ~7 GB model + KV cache)"},
		{Role: "ingestion", LLMModel: "mistral-small-3.1-24b-instruct", Quantisation: "Q4_K_M", ModelFilename: "mistral-small-3.1-24b-instruct-Q4_K_M.gguf", MinVRAMMB: 20480, Description: "Document ingestion (Mistral Small 3.1 24B, ~14 GB model + KV cache)"},
		{Role: "tester", LLMModel: "SmolLM2-135M-Instruct", Quantisation: "Q4_K_M", ModelFilename: "SmolLM2-135M-Instruct-Q4_K_M.gguf", MinVRAMMB: 512, Description: "Connectivity testing — minimal model for any hardware"},
	}
	for _, r := range prodRoles {
		if err := s.UpsertRoleModel(r.Role, r.LLMModel, r.Quantisation, r.ModelFilename, r.ModelFileHash, r.MinVRAMMB, r.Description); err != nil {
			return err
		}
	}
	return nil
}

// SeedTesterRole ensures the tester role always exists, regardless of mode.
// This allows any hardware to participate in the cluster.
func (s *Store) SeedTesterRole() error {
	return s.UpsertRoleModel("tester", "SmolLM2-135M-Instruct", "Q4_K_M", "SmolLM2-135M-Instruct-Q4_K_M.gguf", "", 512, "Connectivity testing — minimal model for any hardware")
}

// GetBrandingCSS returns the stored custom CSS, or empty string if none.
func (s *Store) GetBrandingCSS() (string, error) {
	var branding BrandingConfig
	result := s.DB.First(&branding, 1)
	if result.Error != nil {
		if result.Error == gorm.ErrRecordNotFound {
			return "", nil
		}
		return "", result.Error
	}
	return branding.CSS, nil
}

// SetBrandingCSS upserts the custom CSS.
func (s *Store) SetBrandingCSS(css string) error {
	var branding BrandingConfig
	result := s.DB.First(&branding, 1)
	if result.Error == gorm.ErrRecordNotFound {
		branding = BrandingConfig{ID: 1, CSS: css, UpdatedAt: time.Now()}
		return s.DB.Create(&branding).Error
	}
	if result.Error != nil {
		return result.Error
	}
	return s.DB.Model(&branding).Updates(map[string]any{
		"css":        css,
		"updated_at": time.Now(),
	}).Error
}

// CreateClientError stores an error reported by a worker.
func (s *Store) CreateClientError(fingerprint, version, errMsg, stack string, reportedAt time.Time) error {
	ce := ClientError{
		Fingerprint:  fingerprint,
		Version:      version,
		ErrorMessage: errMsg,
		Stack:        stack,
		ReportedAt:   reportedAt,
	}
	return s.DB.Create(&ce).Error
}

// GetClientErrors returns all client errors ordered by most recent first.
func (s *Store) GetClientErrors() ([]ClientError, error) {
	var errors []ClientError
	result := s.DB.Order("reported_at desc").Limit(100).Find(&errors)
	return errors, result.Error
}

// RecordLatencySample stores a latency observation and increments worker lifetime counters.
func (s *Store) RecordLatencySample(fingerprint, role string, payloadBytes int, latencyMs int64) error {
	sample := LatencySample{
		Fingerprint:  fingerprint,
		Role:         role,
		PayloadBytes: payloadBytes,
		LatencyMs:    latencyMs,
		CreatedAt:    time.Now(),
	}
	if err := s.DB.Create(&sample).Error; err != nil {
		return err
	}

	// Increment worker lifetime counters
	return s.DB.Model(&Worker{}).
		Where("fingerprint = ?", fingerprint).
		Updates(map[string]any{
			"total_requests":   gorm.Expr("total_requests + 1"),
			"total_latency_ms": gorm.Expr("total_latency_ms + ?", latencyMs),
		}).Error
}

// GetLatencySamples retrieves samples within a time window, optionally filtered by role.
func (s *Store) GetLatencySamples(since time.Time, role string) ([]LatencySample, error) {
	var samples []LatencySample
	q := s.DB.Where("created_at > ?", since)
	if role != "" {
		q = q.Where("role = ?", role)
	}
	result := q.Order("latency_ms ASC").Find(&samples)
	return samples, result.Error
}

// GetLatencySamplesInRange retrieves samples within a time window and payload byte range for a given role.
func (s *Store) GetLatencySamplesInRange(since time.Time, role string, minBytes, maxBytes int) ([]LatencySample, error) {
	var samples []LatencySample
	q := s.DB.Where("created_at > ? AND role = ? AND payload_bytes >= ?", since, role, minBytes)
	if maxBytes > 0 {
		q = q.Where("payload_bytes < ?", maxBytes)
	}
	result := q.Order("latency_ms ASC").Find(&samples)
	return samples, result.Error
}

// ComputeHourlyStat computes and upserts the hourly stat for a given role and hour.
func (s *Store) ComputeHourlyStat(role string, hour time.Time) error {
	nextHour := hour.Add(time.Hour)

	var result struct {
		Count int
		Min   int
		Max   int
		Mean  float64
	}
	err := s.DB.Model(&LatencySample{}).
		Select("COUNT(*) as count, COALESCE(MIN(payload_bytes), 0) as min, COALESCE(MAX(payload_bytes), 0) as max, COALESCE(AVG(payload_bytes), 0) as mean").
		Where("role = ? AND created_at >= ? AND created_at < ?", role, hour, nextHour).
		Scan(&result).Error
	if err != nil {
		return err
	}

	if result.Count == 0 {
		return nil
	}

	stat := LatencyHourlyStat{
		Role:             role,
		Hour:             hour,
		Count:            result.Count,
		MinPayloadBytes:  result.Min,
		MaxPayloadBytes:  result.Max,
		MeanPayloadBytes: int(result.Mean),
	}

	// Upsert: try update first, then create
	existing := s.DB.Model(&LatencyHourlyStat{}).
		Where("role = ? AND hour = ?", role, hour).
		Updates(map[string]any{
			"count":              stat.Count,
			"min_payload_bytes":  stat.MinPayloadBytes,
			"max_payload_bytes":  stat.MaxPayloadBytes,
			"mean_payload_bytes": stat.MeanPayloadBytes,
		})
	if existing.RowsAffected == 0 {
		return s.DB.Create(&stat).Error
	}
	return existing.Error
}

// GetHourlyStats retrieves hourly stats within a time window, optionally filtered by role.
func (s *Store) GetHourlyStats(since time.Time, role string) ([]LatencyHourlyStat, error) {
	var stats []LatencyHourlyStat
	q := s.DB.Where("hour >= ?", since)
	if role != "" {
		q = q.Where("role = ?", role)
	}
	result := q.Order("hour ASC").Find(&stats)
	return stats, result.Error
}

// GetDistinctRoles returns all distinct roles present in hourly stats.
func (s *Store) GetDistinctRoles(since time.Time) ([]string, error) {
	var roles []string
	result := s.DB.Model(&LatencyHourlyStat{}).
		Where("hour >= ?", since).
		Distinct("role").
		Pluck("role", &roles)
	return roles, result.Error
}

// GetDistinctSampleRoles returns distinct roles from latency_samples within the time window.
// Used as a fallback when latency_hourly_stats has no data yet.
func (s *Store) GetDistinctSampleRoles(since time.Time) ([]string, error) {
	var roles []string
	result := s.DB.Model(&LatencySample{}).
		Where("created_at > ?", since).
		Distinct("role").
		Pluck("role", &roles)
	return roles, result.Error
}

// GetSamplePayloadRange returns the min and max payload_bytes from latency_samples
// for a given role within the time window.
func (s *Store) GetSamplePayloadRange(since time.Time, role string) (min int, max int, err error) {
	var result struct {
		Min int
		Max int
	}
	err = s.DB.Model(&LatencySample{}).
		Select("COALESCE(MIN(payload_bytes), 0) as min, COALESCE(MAX(payload_bytes), 0) as max").
		Where("created_at > ? AND role = ?", since, role).
		Scan(&result).Error
	return result.Min, result.Max, err
}

// PurgeOldLatencyData deletes samples and hourly stats older than the given time.
func (s *Store) PurgeOldLatencyData(before time.Time) error {
	if err := s.DB.Where("created_at < ?", before).Delete(&LatencySample{}).Error; err != nil {
		return err
	}
	return s.DB.Where("hour < ?", before).Delete(&LatencyHourlyStat{}).Error
}

// CreateBatchRequest stores a new batch request in pending state.
func (s *Store) CreateBatchRequest(requestID, model, payload string) error {
	br := BatchRequest{
		RequestID: requestID,
		LLMModel:  model,
		Payload:   payload,
		Status:    "pending",
	}
	return s.DB.Create(&br).Error
}

// GetBatchRequest retrieves a batch request by its request ID.
func (s *Store) GetBatchRequest(requestID string) (*BatchRequest, error) {
	var br BatchRequest
	result := s.DB.Where("request_id = ?", requestID).First(&br)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &br, nil
}

// GetPendingBatchRequests returns batch requests in pending state, ordered FIFO.
func (s *Store) GetPendingBatchRequests(limit int) ([]BatchRequest, error) {
	var requests []BatchRequest
	result := s.DB.Where("status = ?", "pending").Order("created_at ASC").Limit(limit).Find(&requests)
	return requests, result.Error
}

// SetBatchRequestProcessing marks a batch request as being processed.
func (s *Store) SetBatchRequestProcessing(requestID string) error {
	return s.DB.Model(&BatchRequest{}).
		Where("request_id = ? AND status = ?", requestID, "pending").
		Update("status", "processing").Error
}

// CompleteBatchRequest stores the result and marks the request as completed.
func (s *Store) CompleteBatchRequest(requestID, result string) error {
	return s.DB.Model(&BatchRequest{}).
		Where("request_id = ?", requestID).
		Updates(map[string]any{
			"status": "completed",
			"result": result,
		}).Error
}

// FailBatchRequest stores the error and marks the request as failed.
func (s *Store) FailBatchRequest(requestID, errMsg string) error {
	return s.DB.Model(&BatchRequest{}).
		Where("request_id = ?", requestID).
		Updates(map[string]any{
			"status":      "failed",
			"err_message": errMsg,
		}).Error
}

// ListBatchRequests returns recent batch requests ordered by most recent first.
func (s *Store) ListBatchRequests(limit int) ([]BatchRequest, error) {
	var requests []BatchRequest
	result := s.DB.Order("created_at DESC").Limit(limit).Find(&requests)
	return requests, result.Error
}

// IsWorkerAvailable returns true if the given fingerprint has client status "available" or "online"
// AND sync_status "synced" (aggregated availability for work dispatch).
func (s *Store) IsWorkerAvailable(fingerprint string) bool {
	var worker Worker
	result := s.DB.Where("fingerprint = ? AND status IN ? AND sync_status = ?", fingerprint, []string{"available", "online"}, "synced").First(&worker)
	return result.Error == nil
}

// HasAnyAvailableWorker returns true if at least one worker has status "available" or "online"
// AND sync_status "synced".
func (s *Store) HasAnyAvailableWorker() bool {
	var count int64
	s.DB.Model(&Worker{}).Where("status IN ? AND sync_status = ?", []string{"available", "online"}, "synced").Count(&count)
	return count > 0
}

// SetWorkerLLMHash updates the LLM hash reported by a worker.
func (s *Store) SetWorkerLLMHash(fingerprint, llmHash string) error {
	return s.DB.Model(&Worker{}).
		Where("fingerprint = ?", fingerprint).
		Update("llm_hash", llmHash).Error
}

// SetWorkerGPUAvg stores the worker's rolling GPU baseline mean.
// Only called when the worker has given data-collection consent.
func (s *Store) SetWorkerGPUAvg(fingerprint string, avg int) error {
	return s.DB.Model(&Worker{}).
		Where("fingerprint = ?", fingerprint).
		Update("gpu_avg", avg).Error
}

// UpdateWorkerSyncStatus recomputes the worker's sync_status by comparing its llm_hash
// against the expected hash for its role from the role-model mapping.
// Returns the new sync_status value ("synced" or "out-of-sync").
func (s *Store) UpdateWorkerSyncStatus(fingerprint string) string {
	var worker Worker
	if err := s.DB.Where("fingerprint = ?", fingerprint).First(&worker).Error; err != nil {
		return "out-of-sync"
	}

	// Determine the worker's role from config (default "inference")
	role := "inference"
	var wc WorkerConfig
	if err := s.DB.Where("fingerprint = ?", fingerprint).First(&wc).Error; err == nil && wc.PreferredRole != "" {
		role = wc.PreferredRole
	}

	// If the role has no model hash on the server there is nothing to enforce —
	// treat the worker as synced. This covers the tester role (connectivity-only,
	// no local LLM required) and any role whose model files haven't been uploaded
	// to the server yet.
	var rm RoleModel
	if err := s.DB.Where("role = ?", role).First(&rm).Error; err == nil && rm.ModelFileHash == "" {
		s.DB.Model(&Worker{}).Where("fingerprint = ?", fingerprint).Update("sync_status", "synced")
		return "synced"
	}

	// Beyond this point the role has a known expected hash; the worker must
	// report a matching llm_hash to be considered synced.
	if worker.LLMHash == "" {
		s.DB.Model(&Worker{}).Where("fingerprint = ?", fingerprint).Update("sync_status", "out-of-sync")
		return "out-of-sync"
	}

	// Try preferred role first
	if rm.ModelFileHash != "" {
		expectedHash := protocol.ComputeLLMHash(role, rm.ModelFileHash)
		if worker.LLMHash == expectedHash {
			s.DB.Model(&Worker{}).Where("fingerprint = ?", fingerprint).Update("sync_status", "synced")
			return "synced"
		}
	}

	// Fallback: check all roles (handles race where worker-config hasn't synced yet
	// but the LLM hash already encodes the actual role)
	var allRoles []RoleModel
	s.DB.Find(&allRoles)
	for _, r := range allRoles {
		if r.ModelFileHash == "" {
			continue
		}
		expectedHash := protocol.ComputeLLMHash(r.Role, r.ModelFileHash)
		if worker.LLMHash == expectedHash {
			s.DB.Model(&Worker{}).Where("fingerprint = ?", fingerprint).Update("sync_status", "synced")
			return "synced"
		}
	}

	log.Debug().
		Str("fingerprint", fingerprint[:8]).
		Str("worker_llm_hash", worker.LLMHash[:16]).
		Str("preferred_role", role).
		Int("roles_checked", len(allRoles)).
		Msg("sync_status: no role hash matched worker LLM hash")
	s.DB.Model(&Worker{}).Where("fingerprint = ?", fingerprint).Update("sync_status", "out-of-sync")
	return "out-of-sync"
}

// GetWorkerSyncStatus returns the overall combined sync status for a worker.
func (s *Store) GetWorkerSyncStatus(fingerprint string) string {
	return s.GetWorkerOverallSyncStatus(fingerprint)
}

// AggregatedStatus computes the aggregated status from client_status and sync statuses.
// A worker is "available" only when status=available AND all syncs are "synced".
// A worker in "updating" status stays "updating" regardless of sync state.
func AggregatedStatus(clientStatus, syncStatus string) string {
	if clientStatus == "updating" {
		return "updating"
	}
	if clientStatus == "offline" {
		return "offline"
	}
	if clientStatus == "available" && syncStatus == "synced" {
		return "available"
	}
	if clientStatus == "processing" && syncStatus == "synced" {
		return "processing"
	}
	return "unavailable"
}

// ComputeOverallSyncStatus combines model, binary, and backend sync into one.
func ComputeOverallSyncStatus(modelSync, binarySync, backendSync string) string {
	if modelSync == "synced" && binarySync == "synced" && backendSync == "synced" {
		return "synced"
	}
	return "out-of-sync"
}

// GetWorkerOverallSyncStatus computes the combined sync status for a worker.
func (s *Store) GetWorkerOverallSyncStatus(fingerprint string) string {
	var worker Worker
	if err := s.DB.Select("sync_status, binary_sync_status, backend_sync_status").Where("fingerprint = ?", fingerprint).First(&worker).Error; err != nil {
		return "out-of-sync"
	}
	modelSync := worker.SyncStatus
	if modelSync == "" {
		modelSync = "out-of-sync"
	}

	// Binary sync: if no client version target is configured, treat as synced
	binarySync := worker.BinarySyncStatus
	if binarySync == "" {
		binarySync = "out-of-sync"
	}
	if _, err := s.GetClientVersionConfig(); err != nil {
		binarySync = "synced" // no version target = synced
	}

	// Backend sync: if no backend version target is configured, treat as synced
	backendSync := worker.BackendSyncStatus
	if backendSync == "" {
		backendSync = "synced" // no backend target = synced
	}
	if _, err := s.GetBackendVersionConfig(); err != nil {
		backendSync = "synced" // no backend target = synced
	}

	return ComputeOverallSyncStatus(modelSync, binarySync, backendSync)
}

// WorkerLLMHashMatches returns true if the worker's reported LLM hash matches the expected hash.
func (s *Store) WorkerLLMHashMatches(fingerprint, expectedHash string) bool {
	var worker Worker
	result := s.DB.Where("fingerprint = ? AND llm_hash = ?", fingerprint, expectedHash).First(&worker)
	return result.Error == nil
}

// GetRoleModel retrieves a single role-model mapping by role name.
func (s *Store) GetRoleModel(role string) (*RoleModel, error) {
	var rm RoleModel
	result := s.DB.Where("role = ?", role).First(&rm)
	if result.Error != nil {
		return nil, result.Error
	}
	return &rm, nil
}

// GetClientVersionConfig retrieves the current target version config.
func (s *Store) GetClientVersionConfig() (*ClientVersionConfig, error) {
	var cfg ClientVersionConfig
	result := s.DB.First(&cfg)
	if result.Error != nil {
		return nil, result.Error
	}
	return &cfg, nil
}

// SetClientVersionConfig creates or updates the target client version config.
func (s *Store) SetClientVersionConfig(version, rolloutMode string, rolloutPercentage int) error {
	var cfg ClientVersionConfig
	result := s.DB.First(&cfg)

	if result.Error == gorm.ErrRecordNotFound {
		cfg = ClientVersionConfig{
			TargetVersion:     version,
			RolloutMode:       rolloutMode,
			RolloutPercentage: rolloutPercentage,
			UpdatedAt:         time.Now(),
		}
		return s.DB.Create(&cfg).Error
	}

	if result.Error != nil {
		return result.Error
	}

	return s.DB.Model(&cfg).Updates(map[string]any{
		"target_version":     version,
		"rollout_mode":       rolloutMode,
		"rollout_percentage": rolloutPercentage,
		"updated_at":         time.Now(),
	}).Error
}

// UpdateWorkerBinarySyncStatus sets binary_sync_status based on version comparison.
func (s *Store) UpdateWorkerBinarySyncStatus(fingerprint, targetVersion string) string {
	var worker Worker
	if err := s.DB.Where("fingerprint = ?", fingerprint).First(&worker).Error; err != nil {
		return "out-of-sync"
	}

	status := "out-of-sync"
	if worker.ClientVersion == targetVersion {
		status = "synced"
	}

	s.DB.Model(&worker).Update("binary_sync_status", status)
	return status
}

// GetOutdatedWorkers returns connected workers whose version != target.
func (s *Store) GetOutdatedWorkers(targetVersion string) ([]Worker, error) {
	var workers []Worker
	err := s.DB.Where("status != ? AND client_version != ?", "offline", targetVersion).Find(&workers).Error
	return workers, err
}

// --- Backend version config ---

// GetBackendVersionConfig retrieves the current target backend version config.
func (s *Store) GetBackendVersionConfig() (*BackendVersionConfig, error) {
	var cfg BackendVersionConfig
	result := s.DB.First(&cfg)
	if result.Error != nil {
		return nil, result.Error
	}
	return &cfg, nil
}

// SetBackendVersionConfig creates or updates the target backend version config.
func (s *Store) SetBackendVersionConfig(name, version, downloadURL, sha256 string) error {
	var cfg BackendVersionConfig
	result := s.DB.First(&cfg)

	if result.Error == gorm.ErrRecordNotFound {
		cfg = BackendVersionConfig{
			Name:        name,
			Version:     version,
			DownloadURL: downloadURL,
			SHA256:      sha256,
			UpdatedAt:   time.Now(),
		}
		return s.DB.Create(&cfg).Error
	}

	if result.Error != nil {
		return result.Error
	}

	return s.DB.Model(&cfg).Updates(map[string]any{
		"name":         name,
		"version":      version,
		"download_url": downloadURL,
		"sha256":       sha256,
		"updated_at":   time.Now(),
	}).Error
}

// SetWorkerBackendHash updates the worker's reported backend hash.
func (s *Store) SetWorkerBackendHash(fingerprint, hash string) error {
	return s.DB.Model(&Worker{}).Where("fingerprint = ?", fingerprint).Update("backend_hash", hash).Error
}

// UpdateWorkerBackendSyncStatus compares the worker's backend hash against the target.
func (s *Store) UpdateWorkerBackendSyncStatus(fingerprint string) string {
	var worker Worker
	if err := s.DB.Where("fingerprint = ?", fingerprint).First(&worker).Error; err != nil {
		return "out-of-sync"
	}

	cfg, err := s.GetBackendVersionConfig()
	if err != nil || cfg.SHA256 == "" {
		// No target configured — consider synced (nothing to enforce)
		s.DB.Model(&worker).Update("backend_sync_status", "synced")
		return "synced"
	}

	status := "out-of-sync"
	if worker.BackendHash == cfg.SHA256 {
		status = "synced"
	}

	s.DB.Model(&worker).Update("backend_sync_status", status)
	return status
}
