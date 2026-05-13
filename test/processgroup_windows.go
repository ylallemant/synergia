//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killOnExitJob is a Windows job object created at startup. Every child
// process the test spawns is assigned to it. The job is configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so when the test process exits and
// the last handle to the job closes, the OS terminates every member —
// including the `go run …` instances and the synergia/llama-server
// grandchildren they spawn. This replaces the Unix signal-propagation
// behaviour the test relied on, which doesn't exist on Windows and was
// leaking synergia-manager.exe / synergia-client.exe / llama-server.exe
// after a fatal() exit.
var killOnExitJob windows.Handle

func init() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(h)
		return
	}
	killOnExitJob = h
}

// registerForCleanup assigns cmd's process to the kill-on-exit job. Safe to
// call multiple times; safe before the OS lets a child spawn grandchildren
// because new descendants of a jobbed process automatically inherit the job
// (JOB_OBJECT_LIMIT_BREAKAWAY_OK is not set).
func registerForCleanup(cmd *exec.Cmd) {
	if killOnExitJob == 0 || cmd == nil || cmd.Process == nil {
		return
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(killOnExitJob, h)
}

// prepareGroup configures cmd so it can be targeted with a Ctrl-Break event
// independently of the parent test process. CREATE_NEW_PROCESS_GROUP puts
// the child in its own console process group whose group-leader PID we
// can later pass to GenerateConsoleCtrlEvent. Without this flag, a
// CTRL_BREAK_EVENT call would also hit the test process itself. Must be
// called BEFORE cmd.Start().
func prepareGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// gracefulOrKill asks the process to shut down cleanly (so it can call
// systray's NIM_DELETE, close log files, etc.) and falls back to a hard
// kill if it doesn't exit within the timeout.
//
// On Windows, "graceful" means a CTRL_BREAK_EVENT delivered to the child's
// own process group (set up by prepareGroup). Go maps that to os.Interrupt
// inside the child, which the cluster-client's `signal.NotifyContext` is
// already listening for — its existing shutdown handler then calls
// systray.Quit() and the tray icon is properly de-registered, preventing
// the Windows shell's "ghost icon" cache from holding it after exit.
func gracefulOrKill(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid)); err == nil {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
			return
		case <-time.After(3 * time.Second):
			// Fall through to forceful kill below.
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
