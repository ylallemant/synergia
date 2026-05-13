//go:build !windows

// Package proc collects helpers for shaping how the client spawns child
// processes. See hidewindow_windows.go for the Windows-specific behaviour.
package proc

import "os/exec"

// HideWindow is a no-op on Unix-like systems. Console-spawning is a
// Windows-only quirk; on macOS / Linux a child process simply inherits
// the parent's stdio without any window-allocation step to suppress.
func HideWindow(_ *exec.Cmd) {}
