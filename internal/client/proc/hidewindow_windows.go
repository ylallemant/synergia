//go:build windows

// Package proc collects helpers for shaping how the client spawns child
// processes. The only function here today (HideWindow) suppresses the
// console-window flash that every console-subsystem child would otherwise
// produce when launched from the release-built (GUI-subsystem) client.
package proc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideWindow marks cmd so the OS does not allocate a visible console
// window for the child process. Must be called before cmd.Start /
// cmd.Run / cmd.Output / cmd.CombinedOutput.
//
// Background: the release-built synergia-client.exe is linked with the
// Windows GUI subsystem (`-H windowsgui`) so double-clicking it from
// Explorer doesn't spawn an unwanted console. But because the parent has
// no console of its own, every console-subsystem child it launches
// (llama-server, typeperf, nvidia-smi, powershell, rundll32, cmd, reg,
// synergia-updater) inherits no console either — Windows then allocates
// a brand-new console window for the child, which flashes on screen.
// CREATE_NO_WINDOW disables that allocation entirely.
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
