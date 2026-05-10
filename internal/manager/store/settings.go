package store

// SystemSetting stores key-value pairs for manager-internal state that must
// survive restarts (e.g. the last-seen manager version for cache invalidation).
type SystemSetting struct {
	Key   string `gorm:"primaryKey;size:64"`
	Value string `gorm:"size:256"`
}

// GetSetting retrieves a setting value by key.
func (s *Store) GetSetting(key string) (string, error) {
	var setting SystemSetting
	if err := s.DB.First(&setting, "key = ?", key).Error; err != nil {
		return "", err
	}
	return setting.Value, nil
}

// SetSetting creates or replaces a setting value.
func (s *Store) SetSetting(key, value string) error {
	return s.DB.Save(&SystemSetting{Key: key, Value: value}).Error
}
