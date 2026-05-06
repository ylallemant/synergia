//go:build !darwin && !linux && !windows

package autostart

// IsSupported returns false on unsupported platforms.
func (m *Manager) IsSupported() bool {
	return false
}

// IsEnabled always returns false on unsupported platforms.
func (m *Manager) IsEnabled() bool {
	return false
}

// Enable is not supported on this platform.
func (m *Manager) Enable() error {
	return nil
}

// Disable is not supported on this platform.
func (m *Manager) Disable() error {
	return nil
}
