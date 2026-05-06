//go:build linux

package gpu

import "github.com/ylallemant/synergia/internal/client/gpu/linux"

func newPlatformProber() Prober {
	return linux.New()
}
