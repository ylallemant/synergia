package browser

import (
	"os/exec"
	"runtime"
)

// Open opens the given URL in the user's default browser.
func Open(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		return nil // unsupported platform, silently skip
	}
	return cmd.Start()
}
