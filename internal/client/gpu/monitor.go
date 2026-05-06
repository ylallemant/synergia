package gpu

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// State represents the GPU availability state.
type State int

const (
	StateAvailable State = iota
	StateIdle            // GPU contention detected — not accepting work
)

func (s State) String() string {
	switch s {
	case StateAvailable:
		return "available"
	case StateIdle:
		return "idle"
	default:
		return "unknown"
	}
}

// StateChange is emitted when the GPU monitor detects a state transition.
type StateChange struct {
	From State
	To   State
}

// Prober is the platform-specific interface for reading GPU utilization.
type Prober interface {
	// Supported returns whether GPU monitoring is available on this platform.
	// If not supported, reason describes why (e.g., missing driver).
	Supported() (supported bool, reason string)
	// Utilization returns the current GPU utilization percentage (0-100).
	Utilization() (int, error)
	// DriverInfo returns the GPU driver name and version detected on this platform.
	// Returns empty strings if no driver is detected.
	DriverInfo() (name string, version string)
}

// Monitor watches GPU utilization and detects contention from gaming/rendering.
type Monitor struct {
	interval         time.Duration
	contentionThresh int
	resumeDelay      time.Duration
	prober           Prober

	mu              sync.RWMutex
	state           State
	lastUtilization int
	contentionSince time.Time // when contention was last detected
	baseline        int       // worker's own GPU usage baseline (%)

	StateChangeCh chan StateChange
}

func NewMonitor(interval time.Duration, contentionThresh int, resumeDelay time.Duration) *Monitor {
	return &Monitor{
		interval:         interval,
		contentionThresh: contentionThresh,
		resumeDelay:      resumeDelay,
		prober:           newPlatformProber(),
		state:            StateAvailable,
		StateChangeCh:    make(chan StateChange, 5),
	}
}

// CalibrateBaseline samples GPU utilization several times and sets the baseline
// to the maximum observed value plus a headroom percentage. This accounts for
// normal desktop compositing (Window Server, display rendering, etc.) that is
// always present and should not be treated as contention.
func (m *Monitor) CalibrateBaseline(samples int, delay time.Duration, headroom int) {
	var maxUtil int
	for i := 0; i < samples; i++ {
		if i > 0 {
			time.Sleep(delay)
		}
		util, err := m.prober.Utilization()
		if err != nil {
			log.Debug().Err(err).Int("sample", i+1).Msg("baseline sample failed")
			continue
		}
		if util > maxUtil {
			maxUtil = util
		}
	}

	baseline := maxUtil + headroom
	if baseline > 100 {
		baseline = 100
	}

	m.mu.Lock()
	m.baseline = baseline
	m.mu.Unlock()

	log.Info().
		Int("samples", samples).
		Int("max_observed", maxUtil).
		Int("headroom", headroom).
		Int("baseline", baseline).
		Msg("GPU baseline calibrated")
}

// Run starts the GPU monitoring loop. Blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	log.Info().Dur("interval", m.interval).Int("threshold", m.contentionThresh).Dur("resume_delay", m.resumeDelay).Msg("GPU monitor started")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.check()
		}
	}
}

// GetState returns the current GPU state.
func (m *Monitor) GetState() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// GetUtilization returns the last observed GPU utilization percentage.
func (m *Monitor) GetUtilization() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastUtilization
}

// GPUSupported returns whether GPU monitoring is available and a reason if not.
func (m *Monitor) GPUSupported() (bool, string) {
	return m.prober.Supported()
}

// GPUDriverInfo returns the GPU driver name and version detected on this system.
func (m *Monitor) GPUDriverInfo() (string, string) {
	return m.prober.DriverInfo()
}

// SetBaseline sets the worker's own expected GPU usage percentage.
func (m *Monitor) SetBaseline(pct int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.baseline = pct
}

func (m *Monitor) check() {
	utilization, err := m.prober.Utilization()
	if err != nil {
		log.Debug().Err(err).Msg("GPU utilization check failed")
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastUtilization = utilization

	externalLoad := utilization - m.baseline
	if externalLoad < 0 {
		externalLoad = 0
	}

	contention := externalLoad > m.contentionThresh

	switch m.state {
	case StateAvailable:
		if contention {
			m.state = StateIdle
			m.contentionSince = time.Now()
			log.Info().
				Int("utilization", utilization).
				Int("baseline", m.baseline).
				Int("external_load", externalLoad).
				Msg("GPU contention detected — transitioning to idle")
			m.StateChangeCh <- StateChange{From: StateAvailable, To: StateIdle}
		}

	case StateIdle:
		if contention {
			// Still contended — reset the timer
			m.contentionSince = time.Now()
		} else if time.Since(m.contentionSince) >= m.resumeDelay {
			// Contention resolved for long enough — resume
			m.state = StateAvailable
			log.Info().
				Int("utilization", utilization).
				Int("baseline", m.baseline).
				Dur("idle_duration", time.Since(m.contentionSince)).
				Msg("GPU contention resolved — transitioning to available")
			m.StateChangeCh <- StateChange{From: StateIdle, To: StateAvailable}
		}
	}
}
