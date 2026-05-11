package gpu

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// GPUStats summarises utilisation over the rolling window.
// BaselineMean excludes the top 15 % of samples (gaming / rendering peaks)
// so the manager can derive stable contention thresholds from real idle load.
type GPUStats struct {
	Mean         int `json:"mean"`          // mean of all samples in window
	BaselineMean int `json:"baseline_mean"` // mean of bottom 85 % (excl. peaks)
	Min          int `json:"min"`
	Max          int `json:"max"`
	SampleCount  int `json:"sample_count"` // how many samples are in the window
}

// State represents the GPU availability state.
type State int

const (
	StateAvailable State = iota
	StateBusy            // GPU contention detected — not accepting work
)

func (s State) String() string {
	switch s {
	case StateAvailable:
		return "available"
	case StateBusy:
		return "busy"
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

	// processing is set true while the worker is running an inference request.
	// When true, check() skips the contention state machine and baseline
	// recording: the GPU spike IS the worker's own load (llama-server).
	processing atomic.Bool


	// Rolling window for baseline stats (180 samples ≈ 15 min at 5 s interval).
	windowBuf  [180]int
	windowHead int
	windowFull bool

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
	for i := range samples {
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

	baseline := min(maxUtil+headroom, 100)

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

// SetProcessing tells the monitor whether the worker is actively running an
// inference request.
//   - true  → check() skips contention evaluation and baseline recording
//             (the GPU spike is the worker's own llama-server load)
//   - false → re-enables contention detection immediately; callers in the
//             normal success path should call WaitForBaseline first so the GPU
//             has already settled before detection resumes
func (m *Monitor) SetProcessing(v bool) {
	m.processing.Store(v)
}

// WaitForBaseline polls GPU utilisation every 500 ms until it drops back to
// baseline level (external load ≤ contention threshold) or timeout elapses.
// Returns true when baseline is reached, false on timeout or ctx cancellation.
//
// Call this after inference completes and before SetProcessing(false) so the
// GPU has cooled down before contention detection resumes. If it times out the
// caller should still clear processing — the monitor will detect any real
// external contention on its next tick and emit the appropriate state change.
func (m *Monitor) WaitForBaseline(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			// Read a fresh utilisation directly from the prober so we get
			// sub-second resolution rather than waiting for the 5 s tick.
			util, err := m.prober.Utilization()
			m.mu.Lock()
			if err == nil {
				m.lastUtilization = util
			} else {
				util = m.lastUtilization
			}
			base := m.baseline
			thresh := m.contentionThresh
			m.mu.Unlock()

			externalLoad := max(util-base, 0)
			if externalLoad <= thresh {
				log.Debug().
					Int("utilization", util).
					Int("baseline", base).
					Int("external_load", externalLoad).
					Msg("GPU returned to baseline after processing")
				return true
			}
			if time.Now().After(deadline) {
				log.Debug().
					Int("utilization", util).
					Int("baseline", base).
					Int("external_load", externalLoad).
					Msg("GPU did not return to baseline within timeout — external contention suspected")
				return false
			}
		}
	}
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

	// While processing or cooling down after inference, the GPU is (or was just)
	// driven by the worker's own llama-server — skip both baseline recording
	// and contention evaluation so inference spikes neither pollute the rolling
	// stats nor trigger false contention transitions.
	if m.processing.Load() {
		return
	}

	// Record in rolling window — only when not processing/cooling down.
	m.windowBuf[m.windowHead] = utilization
	m.windowHead = (m.windowHead + 1) % len(m.windowBuf)
	if m.windowHead == 0 {
		m.windowFull = true
	}

	externalLoad := max(utilization-m.baseline, 0)

	contention := externalLoad > m.contentionThresh

	switch m.state {
	case StateAvailable:
		if contention {
			m.state = StateBusy
			m.contentionSince = time.Now()
			log.Info().
				Int("utilization", utilization).
				Int("baseline", m.baseline).
				Int("external_load", externalLoad).
				Msg("GPU contention detected — transitioning to idle")
			m.StateChangeCh <- StateChange{From: StateAvailable, To: StateBusy}
		}

	case StateBusy:
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
			m.StateChangeCh <- StateChange{From: StateBusy, To: StateAvailable}
		}
	}
}

// Stats returns utilisation statistics over the rolling window (last ~15 min).
// BaselineMean excludes the top 15 % of samples to give a stable idle-load
// figure that ignores gaming / rendering spikes — useful for manager threshold tuning.
func (m *Monitor) Stats() GPUStats {
	m.mu.RLock()
	n := m.windowHead
	full := m.windowFull
	buf := m.windowBuf
	m.mu.RUnlock()

	size := n
	if full {
		size = len(buf)
	}
	if size == 0 {
		return GPUStats{}
	}

	samples := make([]int, size)
	if full {
		copy(samples, buf[n:])
		copy(samples[len(buf)-n:], buf[:n])
	} else {
		copy(samples, buf[:n])
	}

	mn, mx, total := samples[0], samples[0], 0
	for _, v := range samples {
		total += v
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}

	sorted := make([]int, size)
	copy(sorted, samples)
	sort.Ints(sorted)
	cutoff := max(size*85/100, 1)
	baseTotal := 0
	for _, v := range sorted[:cutoff] {
		baseTotal += v
	}

	return GPUStats{
		Mean:         total / size,
		BaselineMean: baseTotal / cutoff,
		Min:          mn,
		Max:          mx,
		SampleCount:  size,
	}
}
