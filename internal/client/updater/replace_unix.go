//go:build !windows

package updater

import (
	"fmt"
	"os"
)

// selfReplace atomically replaces the running binary on Unix systems.
// Strategy: rename old binary to .bak, rename new binary to target, remove .bak on success.
func selfReplace(execPath, newPath string) error {
	bakPath := execPath + ".bak"

	// Remove any leftover .bak from previous update
	os.Remove(bakPath)

	// Rename current binary to .bak (preserves inode for running process)
	if err := os.Rename(execPath, bakPath); err != nil {
		return fmt.Errorf("rename current binary to .bak: %w", err)
	}

	// Move new binary into place
	if err := os.Rename(newPath, execPath); err != nil {
		// Rollback: restore old binary
		_ = os.Rename(bakPath, execPath)
		return fmt.Errorf("rename new binary into place: %w", err)
	}

	// Cleanup old binary (best effort — running process still holds the inode)
	os.Remove(bakPath)
	return nil
}
