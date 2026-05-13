//go:build windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/ylallemant/synergia/internal/client/proc"
)

// selfReplace on Windows spawns the synergia-updater helper to perform the swap.
// The running exe is locked by the OS, so a separate process must do the replacement
// after this process exits.
func selfReplace(execPath, newPath string) error {
	// Locate the updater helper next to the main binary
	dir := filepath.Dir(execPath)
	updaterPath := filepath.Join(dir, "synergia-updater.exe")

	if _, err := os.Stat(updaterPath); err != nil {
		// Fallback: try renaming directly (works if binary isn't locked, e.g. during tests)
		return fallbackReplace(execPath, newPath)
	}

	// Spawn the updater helper: it will wait for us to exit, then swap binaries
	pid := os.Getpid()
	cmd := exec.Command(updaterPath,
		"--pid", strconv.Itoa(pid),
		"--src", newPath,
		"--dst", execPath,
		"--restart=true",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	proc.HideWindow(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start synergia-updater: %w", err)
	}

	// The updater is now running; caller should exit so the updater can proceed
	return nil
}

// fallbackReplace attempts a direct rename when the helper is not available.
func fallbackReplace(execPath, newPath string) error {
	bakPath := execPath + ".bak"
	os.Remove(bakPath)

	if err := os.Rename(execPath, bakPath); err != nil {
		return fmt.Errorf("rename current binary to .bak: %w", err)
	}

	if err := os.Rename(newPath, execPath); err != nil {
		_ = os.Rename(bakPath, execPath)
		return fmt.Errorf("rename new binary into place: %w", err)
	}

	os.Remove(bakPath)
	return nil
}
