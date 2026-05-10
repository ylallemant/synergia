//go:build !nosystray

package tray

import (
	"fmt"
	"os/exec"
	"runtime"

	"fyne.io/systray"
	"github.com/rs/zerolog/log"
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

// Tray manages the system tray icon and menu.
type Tray struct {
	status   StatusProvider
	dashURL  string
	adminURL string // non-empty only when manager is on localhost
	pauseCh  chan struct{}
	resumeCh chan struct{}
	quitCh   chan struct{}
}

func New(status StatusProvider, dashURL, adminURL string) *Tray {
	return &Tray{
		status:   status,
		dashURL:  dashURL,
		adminURL: adminURL,
		pauseCh:  make(chan struct{}, 1),
		resumeCh: make(chan struct{}, 1),
		quitCh:   make(chan struct{}, 1),
	}
}

// PauseCh returns a channel that signals when the user clicks "Pause".
func (t *Tray) PauseCh() <-chan struct{} { return t.pauseCh }

// ResumeCh returns a channel that signals when the user clicks "Resume".
func (t *Tray) ResumeCh() <-chan struct{} { return t.resumeCh }

// QuitCh returns a channel that signals when the user clicks "Quit".
func (t *Tray) QuitCh() <-chan struct{} { return t.quitCh }

// Run starts the system tray. Must be called from the main goroutine on macOS.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

// Quit stops the system tray event loop, unblocking Run.
// Called when the process receives SIGINT/SIGTERM so Ctrl-C exits cleanly.
func (t *Tray) Quit() {
	systray.Quit()
}

func (t *Tray) onReady() {
	systray.SetTitle("DT")
	systray.SetTooltip("DeepThink Worker")
	t.setIcon(iconConnectedIdle)

	mDash := systray.AddMenuItem("Open Dashboard", "Open the worker dashboard in your browser")

	// Show "Open Backend" only when manager is on localhost
	var mBackend *systray.MenuItem
	if t.adminURL != "" {
		mBackend = systray.AddMenuItem("Open Backend", "Open the cluster manager admin page")
	}

	systray.AddSeparator()
	mStatus := systray.AddMenuItem("Status: starting...", "")
	mStatus.Disable()
	systray.AddSeparator()
	mPause := systray.AddMenuItem("Pause", "Stop accepting work units")
	mResume := systray.AddMenuItem("Resume", "Resume accepting work units")
	mResume.Hide()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Stop the worker daemon")

	go func() {
		// Use a nil channel for mBackend clicks when not available
		var backendCh <-chan struct{}
		if mBackend != nil {
			backendCh = mBackend.ClickedCh
		}

		for {
			select {
			case <-mDash.ClickedCh:
				t.openBrowser(t.dashURL)
			case <-backendCh:
				t.openBrowser(t.adminURL)
			case <-mPause.ClickedCh:
				mPause.Hide()
				mResume.Show()
				t.pauseCh <- struct{}{}
			case <-mResume.ClickedCh:
				mResume.Hide()
				mPause.Show()
				t.resumeCh <- struct{}{}
			case <-mQuit.ClickedCh:
				t.quitCh <- struct{}{}
				systray.Quit()
			}
		}
	}()

	log.Info().Msg("system tray ready")
}

func (t *Tray) onExit() {
	log.Info().Msg("system tray exiting")
}

// UpdateStatus refreshes the tray icon based on current state.
func (t *Tray) UpdateStatus(connected bool, gpuState gpu.State, processing bool, paused bool) {
	gpuSupported, gpuReason := t.status.GPUSupported()
	llmReachable, llmError := t.status.LLMReachable()

	switch {
	case !connected:
		t.setIcon(iconDisconnected)
		systray.SetTooltip("DeepThink Worker — Disconnected")
	case !llmReachable:
		t.setIcon(iconReconnecting) // yellow — warning state
		systray.SetTooltip("DeepThink Worker — LLM Unreachable: " + llmError)
	case !gpuSupported:
		t.setIcon(iconReconnecting) // yellow — warning state
		systray.SetTooltip("DeepThink Worker — GPU Unsupported: " + gpuReason)
	case paused:
		t.setIcon(iconPaused)
		systray.SetTooltip("DeepThink Worker — Paused")
	case gpuState == gpu.StateIdle:
		t.setIcon(iconReconnecting) // yellow — GPU busy
		systray.SetTooltip("DeepThink Worker — GPU Busy (Idle)")
	case processing:
		t.setIcon(iconProcessing)
		systray.SetTooltip("DeepThink Worker — Processing")
	default:
		t.setIcon(iconConnectedIdle)
		systray.SetTooltip("DeepThink Worker — Ready")
	}
}

func (t *Tray) setIcon(icon []byte) {
	systray.SetIcon(icon)
}

func (t *Tray) openBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "linux":
		cmd = "xdg-open"
		args = []string{url}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		log.Warn().Str("os", runtime.GOOS).Msg("unsupported platform for browser open")
		fmt.Println("Open in browser:", url)
		return
	}

	if err := exec.Command(cmd, args...).Start(); err != nil {
		log.Warn().Err(err).Msg("failed to open browser")
	}
}
