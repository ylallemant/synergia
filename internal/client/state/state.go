// Package state holds the central, thread-safe snapshot of all worker state.
// Probe goroutines write to it at their own cadence; every other component
// (HTTP handlers, tray, worker) reads from it without doing any I/O.
package state

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ylallemant/synergia/internal/client/gpu"
)

// State aggregates all live worker state written by probe goroutines.
// It satisfies server.StatusProvider, tray.StatusProvider, and the worker's
// ProcessingTracker / UnitCounter / PauseChecker interfaces.
type State struct {
	mu sync.RWMutex

	// Written by connection probe (1 s)
	connected  bool
	gpuSentAvg int

	// Written by LLM probe (3 s)
	llmReachable bool
	llmError     string

	// Written by GPU probe (1 s — reads from gpu.Monitor cache, updated at 5 s)
	gpuState         gpu.State
	gpuUtil          int
	gpuStats         gpu.GPUStats
	gpuSupported     bool
	gpuSupportReason string
	gpuDriver        string
	gpuDriverVersion string

	// Written by worker goroutine (event-driven)
	processing atomic.Bool
	paused     atomic.Bool
	units      atomic.Int64

	// Static — set once at construction
	startedAt    time.Time
	fingerprint  string
	model        string
	quantisation string
}

func New(fingerprint, model, quantisation string) *State {
	return &State{
		startedAt:    time.Now(),
		fingerprint:  fingerprint,
		model:        model,
		quantisation: quantisation,
		llmError:     "not yet probed",
	}
}

// ── Setters called by probe goroutines ───────────────────────────────────────

func (s *State) SetConnected(v bool, gpuSentAvg int) {
	s.mu.Lock()
	s.connected = v
	s.gpuSentAvg = gpuSentAvg
	s.mu.Unlock()
}

func (s *State) SetLLM(reachable bool, errStr string) {
	s.mu.Lock()
	s.llmReachable = reachable
	s.llmError = errStr
	s.mu.Unlock()
}

func (s *State) SetGPU(st gpu.State, util int, stats gpu.GPUStats,
	supported bool, reason, driver, driverVersion string) {
	s.mu.Lock()
	s.gpuState = st
	s.gpuUtil = util
	s.gpuStats = stats
	s.gpuSupported = supported
	s.gpuSupportReason = reason
	s.gpuDriver = driver
	s.gpuDriverVersion = driverVersion
	s.mu.Unlock()
}

// ── Setters called by the worker (event-driven) ───────────────────────────────

func (s *State) SetProcessing(v bool)  { s.processing.Store(v) }
func (s *State) SetPaused(v bool)      { s.paused.Store(v) }
func (s *State) IncrementUnits()       { s.units.Add(1) }

// ── Raw value getters (no interpretation, no I/O) ────────────────────────────

func (s *State) Connected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connected
}
func (s *State) LLMReachable() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.llmReachable, s.llmError
}
func (s *State) GPUState() gpu.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gpuState
}
func (s *State) GPUUtilization() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gpuUtil
}
func (s *State) GPUStats() gpu.GPUStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gpuStats
}
func (s *State) GPUSentAvg() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gpuSentAvg
}
func (s *State) GPUSupported() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gpuSupported, s.gpuSupportReason
}
func (s *State) GPUDriverInfo() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gpuDriver, s.gpuDriverVersion
}
// Processing and Paused are raw boolean fields read by status.Provider.
// The Is-prefixed aliases satisfy the worker.ProcessingTracker /
// worker.PauseChecker interfaces without importing worker.
func (s *State) Processing() bool      { return s.processing.Load() }
func (s *State) IsProcessing() bool    { return s.processing.Load() }
func (s *State) Paused() bool          { return s.paused.Load() }
func (s *State) IsPaused() bool        { return s.paused.Load() }
func (s *State) UnitsProcessed() int64 { return s.units.Load() }
func (s *State) Uptime() time.Duration { return time.Since(s.startedAt) }
func (s *State) Fingerprint() string   { return s.fingerprint }
func (s *State) Model() string         { return s.model }
func (s *State) Quantisation() string  { return s.quantisation }
