package autostart

// Manager handles OS-level auto-start registration for the cluster client.
// Status is always read directly from the OS (no config file or manager sync).
type Manager struct {
	execPath string
	args     []string
}

// New creates an autostart manager. execPath is the absolute path to the binary;
// args are the CLI arguments to pass on startup.
func New(execPath string, args []string) *Manager {
	return &Manager{
		execPath: execPath,
		args:     args,
	}
}

// ExecPath returns the absolute path to the binary.
func (m *Manager) ExecPath() string {
	return m.execPath
}
