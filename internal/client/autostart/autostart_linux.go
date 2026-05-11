package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	serviceFileName = "synergia-client.service"
)

func servicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", serviceFileName), nil
}

// IsSupported returns true on Linux (assumes systemd is available).
func (m *Manager) IsSupported() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// IsEnabled checks whether the systemd user service is enabled.
func (m *Manager) IsEnabled() bool {
	out, err := exec.Command("systemctl", "--user", "is-enabled", serviceFileName).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "enabled"
}

// Enable creates a systemd user service and enables it.
func (m *Manager) Enable() error {
	path, err := servicePath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create systemd user directory: %w", err)
	}

	unit := generateServiceUnit(m.execPath, m.args)
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		return fmt.Errorf("failed to write service unit: %w", err)
	}

	// Reload systemd and enable the service
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload failed: %s: %w", string(out), err)
	}
	if out, err := exec.Command("systemctl", "--user", "enable", serviceFileName).CombinedOutput(); err != nil {
		return fmt.Errorf("enable failed: %s: %w", string(out), err)
	}

	return nil
}

// Disable disables and removes the systemd user service.
func (m *Manager) Disable() error {
	_ = exec.Command("systemctl", "--user", "disable", serviceFileName).Run()

	path, err := servicePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove service file: %w", err)
	}

	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func generateServiceUnit(execPath string, args []string) string {
	execLine := execPath
	if len(args) > 0 {
		execLine += " " + strings.Join(args, " ")
	}

	return `[Unit]
Description=Synergia Cluster Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + execLine + `
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`
}
