//go:build !windows

package updater

// ensureUpdater is a no-op on non-Windows platforms — the sidecar exists only
// to handle locked-file replacement on Windows.
func (u *Updater) ensureUpdater(_, _, _ string) error {
	return nil
}
