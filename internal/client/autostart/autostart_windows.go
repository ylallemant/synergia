//go:build windows

package autostart

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/ylallemant/synergia/internal/client/proc"
)

const (
	registryKey   = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	registryValue = "SynergiaClusterClient"
)

// IsSupported returns true on Windows.
func (m *Manager) IsSupported() bool {
	return true
}

// IsEnabled checks whether the registry Run value exists.
func (m *Manager) IsEnabled() bool {
	cmd := exec.Command("reg", "query", registryKey, "/v", registryValue)
	proc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), registryValue)
}

// Enable creates a registry Run entry so the client starts at next user login.
func (m *Manager) Enable() error {
	execLine := `"` + m.execPath + `"`
	if len(m.args) > 0 {
		execLine += " " + strings.Join(m.args, " ")
	}

	cmd := exec.Command("reg", "add", registryKey, "/v", registryValue, "/t", "REG_SZ", "/d", execLine, "/f")
	proc.HideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add registry entry: %s: %w", string(out), err)
	}
	return nil
}

// Disable removes the registry Run entry.
func (m *Manager) Disable() error {
	cmd := exec.Command("reg", "delete", registryKey, "/v", registryValue, "/f")
	proc.HideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Ignore error if the value doesn't exist
		if !strings.Contains(string(out), "unable to find") {
			return fmt.Errorf("failed to remove registry entry: %s: %w", string(out), err)
		}
	}
	return nil
}
