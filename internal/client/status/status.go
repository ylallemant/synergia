package status

import (
	"sync/atomic"
	"time"

	"github.com/ylallemant/synergia/internal/client/connection"
	"github.com/ylallemant/synergia/internal/client/gpu"
	"github.com/ylallemant/synergia/internal/client/identity"
	"github.com/ylallemant/synergia/internal/client/llm"
)

// Provider aggregates worker state for the dashboard and system tray.
// Implements both server.StatusProvider and tray.StatusProvider.
type Provider struct {
	conn         *connection.Connection
	monitor      *gpu.Monitor
	llmClient    *llm.Client
	id           *identity.Identity
	model        string
	quantisation string
	startedAt    time.Time
	units        atomic.Int64
	processing   atomic.Bool
	paused       atomic.Bool
}

func New(conn *connection.Connection, monitor *gpu.Monitor, llmClient *llm.Client, id *identity.Identity, model, quantisation string) *Provider {
	return &Provider{
		conn:         conn,
		monitor:      monitor,
		llmClient:    llmClient,
		id:           id,
		model:        model,
		quantisation: quantisation,
		startedAt:    time.Now(),
	}
}

func (p *Provider) IsConnected() bool               { return p.conn.IsConnected() }
func (p *Provider) GPUState() gpu.State             { return p.monitor.GetState() }
func (p *Provider) GPUUtilization() int             { return p.monitor.GetUtilization() }
func (p *Provider) GPUSupported() (bool, string)    { return p.monitor.GPUSupported() }
func (p *Provider) GPUDriverInfo() (string, string) { return p.monitor.GPUDriverInfo() }
func (p *Provider) LLMReachable() (bool, string)    { return p.llmClient.IsReachable() }
func (p *Provider) Fingerprint() string             { return p.id.Fingerprint }
func (p *Provider) Model() string                   { return p.model }
func (p *Provider) Quantisation() string            { return p.quantisation }
func (p *Provider) UnitsProcessed() int64           { return p.units.Load() }
func (p *Provider) Uptime() time.Duration           { return time.Since(p.startedAt) }
func (p *Provider) IncrementUnits()                 { p.units.Add(1) }
func (p *Provider) IsProcessing() bool              { return p.processing.Load() }
func (p *Provider) SetProcessing(v bool)            { p.processing.Store(v) }
func (p *Provider) IsPaused() bool                  { return p.paused.Load() }
func (p *Provider) SetPaused(v bool)                { p.paused.Store(v) }
