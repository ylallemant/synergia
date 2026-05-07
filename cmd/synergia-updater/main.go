//go:build windows

// synergia-updater is a Windows helper binary that performs binary replacement
// when the main synergia-client executable is locked by the OS.
//
// Usage: synergia-updater.exe --pid <parent_pid> --src <new_binary> --dst <target_path>
//
// It waits for the parent process to exit, replaces the target binary, and
// optionally restarts the client.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
)

func main() {
	pid := flag.Int("pid", 0, "PID of the parent process to wait for")
	src := flag.String("src", "", "path to the new binary")
	dst := flag.String("dst", "", "target install path")
	restart := flag.Bool("restart", true, "restart the client after update")
	flag.Parse()

	if *pid == 0 || *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: synergia-updater --pid <PID> --src <new_binary> --dst <target_path>")
		os.Exit(1)
	}

	// Wait for parent process to exit
	if err := waitForProcess(*pid); err != nil {
		fmt.Fprintf(os.Stderr, "warning: wait for process %d: %v\n", *pid, err)
	}

	// Small extra delay to ensure file handles are released
	time.Sleep(500 * time.Millisecond)

	bakPath := *dst + ".bak"

	// Remove any leftover .bak
	os.Remove(bakPath)

	// Rename current binary to .bak
	if err := os.Rename(*dst, bakPath); err != nil {
		// Target might already be gone if previous update was interrupted
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "rename %s -> %s: %v\n", *dst, bakPath, err)
			os.Exit(1)
		}
	}

	// Move new binary into place
	if err := os.Rename(*src, *dst); err != nil {
		// Rollback
		_ = os.Rename(bakPath, *dst)
		fmt.Fprintf(os.Stderr, "rename %s -> %s: %v\n", *src, *dst, err)
		os.Exit(1)
	}

	// Cleanup .bak (best effort)
	os.Remove(bakPath)

	// Restart the client
	if *restart {
		cmd := exec.Command(*dst)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to restart %s: %v\n", *dst, err)
			os.Exit(1)
		}
	}
}

// waitForProcess waits up to 30 seconds for the given PID to exit.
func waitForProcess(pid int) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Process may already be gone
		return nil
	}
	defer windows.CloseHandle(handle)

	event, err := windows.WaitForSingleObject(handle, 30000) // 30s timeout
	if err != nil {
		return fmt.Errorf("WaitForSingleObject: %w", err)
	}
	if event == uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("timeout waiting for process %d to exit", pid)
	}
	return nil
}
