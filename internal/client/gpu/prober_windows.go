//go:build windows

package gpu

import "github.com/ylallemant/synergia/internal/client/gpu/windows"

func newPlatformProber() Prober {
	return windows.New()
}
