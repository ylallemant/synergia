//go:build darwin

package gpu

import "github.com/ylallemant/synergia/internal/client/gpu/darwin"

func newPlatformProber() Prober {
	return darwin.New()
}
