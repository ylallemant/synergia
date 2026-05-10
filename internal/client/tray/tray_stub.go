//go:build nosystray

package tray

import (
	"github.com/ylallemant/synergia/internal/client/gpu"
)

// StatusProvider gives the tray access to current worker state.
type StatusProvider interface {
	IsConnected() bool
	GPUState() gpu.State
	GPUSupported() (bool, string)
	LLMReachable() (bool, string)
	Fingerprint() string
	Model() string
}

// Tray is a no-op stub when built without systray support.
type Tray struct {
	pauseCh  chan struct{}
	resumeCh chan struct{}
	quitCh   chan struct{}
	doneCh   chan struct{}
}

func New(_ StatusProvider, _, _ string) *Tray {
	return &Tray{
		pauseCh:  make(chan struct{}, 1),
		resumeCh: make(chan struct{}, 1),
		quitCh:   make(chan struct{}, 1),
		doneCh:   make(chan struct{}),
	}
}

func (t *Tray) PauseCh() <-chan struct{}  { return t.pauseCh }
func (t *Tray) ResumeCh() <-chan struct{} { return t.resumeCh }
func (t *Tray) QuitCh() <-chan struct{}   { return t.quitCh }

// Run blocks until Quit is called.
func (t *Tray) Run() { <-t.doneCh }

// Quit unblocks Run, allowing the process to exit on SIGINT/SIGTERM.
func (t *Tray) Quit() { close(t.doneCh) }

func (t *Tray) UpdateStatus(_ bool, _ gpu.State, _ bool, _ bool) {}
