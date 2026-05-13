//go:build !windows

package main

import (
	"os"
	"os/exec"
	"time"
)

// registerForCleanup is a no-op on Unix-like systems. The test relies on the
// standard Unix signal-propagation behaviour to terminate children when the
// test process exits; Windows needs an explicit Job Object — see the
// windows build-tagged sibling for that mechanism.
func registerForCleanup(_ *exec.Cmd) {}

// prepareGroup is a no-op on Unix. Process groups are configured by the
// kernel via setpgid()/setsid() at fork time; the test doesn't need a
// dedicated group to deliver signals because cmd.Process.Signal works
// directly on a single PID.
func prepareGroup(_ *exec.Cmd) {}

// gracefulOrKill sends SIGINT, waits up to 5 s for the process to exit
// cleanly, then escalates to SIGKILL. This matches the pre-Windows-refactor
// cleanup behaviour exactly; the Windows path lives in the build-tagged
// sibling and uses CTRL_BREAK_EVENT instead (Go's cmd.Process.Signal
// doesn't actually deliver os.Interrupt on Windows).
func gracefulOrKill(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
