// Package status interprets the raw worker state into a coherent view that
// implements the server and tray StatusProvider interfaces.
//
// state.State stores raw probe values (facts).
// status.Provider reads from state and exposes a typed, stable API that
// other packages depend on. All status judgements live here — if a badge
// label or availability rule ever changes, this is the only file to edit.
package status

import (
	"context"
	"sync"

	"github.com/rs/zerolog/log"
	"time"

	"github.com/ylallemant/synergia/internal/client/gpu"
	"github.com/ylallemant/synergia/internal/client/state"
)

// Status strings — the canonical values produced by Compute.
// Consumers (tray, dashboard JS) switch on these constants.
const (
	StatusDisconnected   = "disconnected"
	StatusLLMUnreachable = "llm_unreachable"
	StatusGPUUnsupported = "gpu_unsupported"
	StatusPaused         = "paused"
	StatusGPUBusy        = "gpu_busy"
	StatusProcessing     = "processing"
	StatusReady          = "ready"
)

// ChangeHandler is called whenever the computed status changes.
// old is the previous status string ("" on the first fire), new is current.
type ChangeHandler func(old, new string)

// Provider interprets worker state for the dashboard and system tray.
// It implements both server.StatusProvider and tray.StatusProvider.
type Provider struct {
	st *state.State

	mu       sync.RWMutex
	handlers []ChangeHandler
}

func New(st *state.State) *Provider {
	return &Provider{st: st}
}

// AddHandler registers a function that is called whenever the status changes.
// Safe to call at any time, including before Run.
func (p *Provider) AddHandler(h ChangeHandler) {
	p.mu.Lock()
	p.handlers = append(p.handlers, h)
	p.mu.Unlock()
}

// Compute derives the current status string from raw state.
// Conditions are checked in descending priority — the first match wins.
func (p *Provider) Compute() string {
	// 1. No WebSocket connection — nothing else matters.
	if !p.st.Connected() {
		return StatusDisconnected
	}
	// 2. GPU hardware cannot be monitored — the worker cannot safely participate
	//    because contention detection is blind. Higher weight than LLM: a broken
	//    model backend can be fixed at runtime; unsupported GPU hardware cannot.
	gpuOK, _ := p.st.GPUSupported()
	if !gpuOK {
		return StatusGPUUnsupported
	}
	// 3. Local inference backend (llama-server) is unreachable — would reject
	//    every work unit, so surface this before user-driven states.
	llmOK, _ := p.st.LLMReachable()
	if !llmOK {
		return StatusLLMUnreachable
	}
	// 4. Worker paused by the user — intentional, takes precedence over transient
	//    states like GPU contention.
	if p.st.Paused() {
		return StatusPaused
	}
	// 5. Actively running an inference request — must come before GPUBusy so
	//    that our own inference load is not reported as external contention.
	if p.st.Processing() {
		return StatusProcessing
	}
	// 6. External GPU contention detected — user is running a GPU-heavy task;
	//    the worker will reject new jobs until the GPU returns to baseline.
	if p.st.GPUState() == gpu.StateBusy {
		return StatusGPUBusy
	}
	// 7. All clear.
	return StatusReady
}

// Run evaluates status every second and calls registered handlers whenever
// it changes. Fires all handlers once immediately with ("", current) so
// each handler starts with a known state. Blocks until ctx is cancelled.
func (p *Provider) Run(ctx context.Context) {
	notify := func(old, current string) {
		if old == "" {
			log.Debug().Str("status", current).Msg("initial worker status")
		} else {
			log.Debug().Str("from", old).Str("to", current).Msg("worker status changed")
		}
		p.mu.RLock()
		hs := append([]ChangeHandler(nil), p.handlers...)
		p.mu.RUnlock()
		for _, h := range hs {
			h(old, current)
		}
	}

	current := p.Compute()
	notify("", current)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			next := p.Compute()
			if next == current {
				continue
			}
			old := current
			current = next
			notify(old, current)
		}
	}
}

// ── server.StatusProvider / tray.StatusProvider ────────────────────────────

func (p *Provider) IsConnected() bool               { return p.st.Connected() }
func (p *Provider) IsProcessing() bool              { return p.st.Processing() }
func (p *Provider) IsPaused() bool                  { return p.st.Paused() }
func (p *Provider) GPUState() gpu.State             { return p.st.GPUState() }
func (p *Provider) GPUUtilization() int             { return p.st.GPUUtilization() }
func (p *Provider) GPUSentAvg() int                 { return p.st.GPUSentAvg() }
func (p *Provider) GPUStats() gpu.GPUStats          { return p.st.GPUStats() }
func (p *Provider) GPUSupported() (bool, string)    { return p.st.GPUSupported() }
func (p *Provider) GPUDriverInfo() (string, string) { return p.st.GPUDriverInfo() }
func (p *Provider) LLMReachable() (bool, string)    { return p.st.LLMReachable() }
func (p *Provider) Fingerprint() string             { return p.st.Fingerprint() }
func (p *Provider) Model() string                   { return p.st.Model() }
func (p *Provider) Quantisation() string            { return p.st.Quantisation() }
func (p *Provider) UnitsProcessed() int64           { return p.st.UnitsProcessed() }
func (p *Provider) Uptime() time.Duration           { return p.st.Uptime() }

// ── Derived status (interpretations of raw state) ─────────────────────────
// Add higher-level judgements here as the product evolves.

// IsAvailable reports whether the worker can accept work: connected, LLM
// reachable, GPU not contended, not paused, and not currently processing.
func (p *Provider) IsAvailable() bool {
	llmOK, _ := p.st.LLMReachable()
	return p.st.Connected() &&
		llmOK &&
		p.st.GPUState() == gpu.StateAvailable &&
		!p.st.Paused() &&
		!p.st.Processing()
}
