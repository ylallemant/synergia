package store

import (
	"time"

	"gorm.io/gorm"
)

// OidcConfig persists OIDC provider settings configured via the admin UI.
// A single row is kept (upsert pattern). Values here override env vars at startup.
type OidcConfig struct {
	ID           uint   `gorm:"primaryKey"`
	Enabled      bool
	ClientID     string `gorm:"size:256"`
	ClientSecret string `gorm:"size:256"`
	ProviderURL  string `gorm:"size:512"`
	RedirectURL  string `gorm:"size:512"`
	UpdatedAt    time.Time
}

func (s *Store) GetOIDCConfig() (*OidcConfig, error) {
	var cfg OidcConfig
	result := s.DB.First(&cfg)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &cfg, nil
}

// SetOIDCConfig upserts the OIDC configuration. Pass an empty clientSecret to
// leave the stored secret unchanged.
func (s *Store) SetOIDCConfig(enabled bool, clientID, clientSecret, providerURL, redirectURL string) error {
	var cfg OidcConfig
	result := s.DB.First(&cfg)

	if result.Error == gorm.ErrRecordNotFound {
		cfg = OidcConfig{
			Enabled:      enabled,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			ProviderURL:  providerURL,
			RedirectURL:  redirectURL,
			UpdatedAt:    time.Now(),
		}
		return s.DB.Create(&cfg).Error
	}
	if result.Error != nil {
		return result.Error
	}

	updates := map[string]any{
		"enabled":      enabled,
		"client_id":    clientID,
		"provider_url": providerURL,
		"redirect_url": redirectURL,
		"updated_at":   time.Now(),
	}
	if clientSecret != "" {
		updates["client_secret"] = clientSecret
	}
	return s.DB.Model(&cfg).Updates(updates).Error
}
