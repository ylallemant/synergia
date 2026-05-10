package store

import (
	"time"

	"gorm.io/gorm"
)

// WorkerAuthConfig persists worker authentication settings configured via the admin UI.
// A single row is kept (upsert pattern). Values here override env vars at startup.
type WorkerAuthConfig struct {
	ID          uint   `gorm:"primaryKey"`
	TOFUEnabled bool   // true = challenge-response; false = shared key
	WorkerKey   string `gorm:"size:256"` // the shared key (key-auth mode)
	UpdatedAt   time.Time
}

func (s *Store) GetWorkerAuthConfig() (*WorkerAuthConfig, error) {
	var cfg WorkerAuthConfig
	result := s.DB.First(&cfg)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &cfg, nil
}

// SetWorkerAuthConfig upserts the worker auth configuration.
// Pass an empty workerKey to leave the stored key unchanged.
func (s *Store) SetWorkerAuthConfig(tofuEnabled bool, workerKey string) error {
	var cfg WorkerAuthConfig
	result := s.DB.First(&cfg)

	if result.Error == gorm.ErrRecordNotFound {
		cfg = WorkerAuthConfig{
			TOFUEnabled: tofuEnabled,
			WorkerKey:   workerKey,
			UpdatedAt:   time.Now(),
		}
		return s.DB.Create(&cfg).Error
	}
	if result.Error != nil {
		return result.Error
	}

	updates := map[string]any{
		"tofu_enabled": tofuEnabled,
		"updated_at":   time.Now(),
	}
	if workerKey != "" {
		updates["worker_key"] = workerKey
	}
	return s.DB.Model(&cfg).Updates(updates).Error
}
