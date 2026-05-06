package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	launchAgentLabel = "com.synergia.client"
)

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

func guiDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

// IsSupported returns true on macOS.
func (m *Manager) IsSupported() bool {
	return true
}

// IsEnabled checks whether the LaunchAgent plist file exists.
// File presence means the service will start at next login.
func (m *Manager) IsEnabled() bool {
	path, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Enable creates a LaunchAgent plist so the client starts at next user login.
// Does NOT bootstrap the service immediately — avoids launching a duplicate instance.
func (m *Manager) Enable() error {
	path, err := plistPath()
	if err != nil {
		return err
	}

	// Ensure LaunchAgents directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}

	plist := generatePlist(m.execPath, m.args)
	if err := os.WriteFile(path, []byte(plist), 0644); err != nil {
		return fmt.Errorf("failed to write plist: %w", err)
	}

	return nil
}

// Disable unloads the LaunchAgent (if loaded) and removes the plist file.
func (m *Manager) Disable() error {
	path, err := plistPath()
	if err != nil {
		return err
	}

	// Try to bootout if currently loaded (ignore errors — may not be loaded)
	target := guiDomain() + "/" + launchAgentLabel
	cmd := exec.Command("launchctl", "bootout", target)
	_ = cmd.Run()

	// Remove the plist file
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist: %w", err)
	}
	return nil
}

func generatePlist(execPath string, args []string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchAgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + execPath + `</string>
`)
	for _, arg := range args {
		sb.WriteString("\t\t<string>" + arg + "</string>\n")
	}
	sb.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/tmp/synergia-client.log</string>
	<key>StandardErrorPath</key>
	<string>/tmp/synergia-client.log</string>
</dict>
</plist>
`)
	return sb.String()
}
