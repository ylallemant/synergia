//go:build !darwin && !linux && !windows

package gpu

// noopProber is a fallback for unsupported platforms — always reports 0% utilization.
type noopProber struct{}

func newPlatformProber() Prober {
	return &noopProber{}
}

func (p *noopProber) Supported() (bool, string) {
	return false, "GPU monitoring is not supported on this platform"
}

func (p *noopProber) Utilization() (int, error) {
	return 0, nil
}

func (p *noopProber) DriverInfo() (string, string) {
	return "", ""
}
