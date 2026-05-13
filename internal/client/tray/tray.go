//go:build !nosystray

package tray

import (
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"

	"fyne.io/systray"
	"github.com/rs/zerolog/log"
	"github.com/ylallemant/synergia/internal/client/proc"
	"github.com/ylallemant/synergia/internal/client/status"
)

// StatusProvider gives the tray access to error details for tooltip text.
// The tray no longer drives its own status computation — it reacts to
// status.ChangeHandler calls fired by status.Provider.Run.
type StatusProvider interface {
	LLMReachable() (bool, string)
	GPUSupported() (bool, string)
}

// Tray manages the system tray icon and menu.
type Tray struct {
	status   StatusProvider
	dashURL  string
	adminURL string // non-empty only when manager is on localhost
	pauseCh  chan struct{}
	resumeCh chan struct{}
	quitCh   chan struct{}

	// ready toggles to true once systray.Run has invoked onReady. Status
	// updates that arrive before that race-condition window closes are
	// buffered in pendingStatus (latest wins) and replayed by onReady.
	// Without this, fyne.io/systray's SetIcon/SetTooltip return the
	// noisy "tray not ready yet" warning twice on every Windows startup.
	ready         atomic.Bool
	pendingMu     sync.Mutex
	pendingStatus string
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
	systray.SetTooltip("Synergia Worker")
	t.setIcon(iconConnectedIdle)
	t.ready.Store(true)

	// Replay any status that arrived during the pre-ready race window.
	t.pendingMu.Lock()
	pending := t.pendingStatus
	t.pendingStatus = ""
	t.pendingMu.Unlock()
	if pending != "" {
		t.applyStatus(pending)
	}

	mDash := systray.AddMenuItem("Open Dashboard", "Open the worker dashboard in your browser")

	// Show "Open Backend" only when manager is on localhost
	var mBackend *systray.MenuItem
	if t.adminURL != "" {
		mBackend = systray.AddMenuItem("Open Backend", "Open the cluster manager admin page")
	}

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

// UpdateStatus is a status.ChangeHandler — called by status.Provider.Run
// whenever the computed status changes. It maps each status const to the
// appropriate tray icon and tooltip. If the systray hasn't finished
// initialising yet, the latest status is buffered and replayed from onReady.
func (t *Tray) UpdateStatus(_, current string) {
	if !t.ready.Load() {
		t.pendingMu.Lock()
		t.pendingStatus = current
		t.pendingMu.Unlock()
		return
	}
	t.applyStatus(current)
}

func (t *Tray) applyStatus(current string) {
	_, llmError := t.status.LLMReachable()
	_, gpuReason := t.status.GPUSupported()

	switch current {
	case status.StatusDisconnected:
		t.setIcon(iconDisconnected)
		systray.SetTooltip("Synergia Worker — Disconnected")
	case status.StatusLLMUnreachable:
		t.setIcon(iconReconnecting)
		systray.SetTooltip("Synergia Worker — LLM Unreachable: " + llmError)
	case status.StatusGPUUnsupported:
		t.setIcon(iconReconnecting)
		systray.SetTooltip("Synergia Worker — GPU Unsupported: " + gpuReason)
	case status.StatusPaused:
		t.setIcon(iconPaused)
		systray.SetTooltip("Synergia Worker — Paused")
	case status.StatusGPUBusy:
		t.setIcon(iconReconnecting)
		systray.SetTooltip("Synergia Worker — GPU Busy")
	case status.StatusProcessing:
		t.setIcon(iconProcessing)
		systray.SetTooltip("Synergia Worker — Processing")
	default: // StatusReady
		t.setIcon(iconConnectedIdle)
		systray.SetTooltip("Synergia Worker — Ready")
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

	c := exec.Command(cmd, args...)
	proc.HideWindow(c)
	if err := c.Start(); err != nil {
		log.Warn().Err(err).Msg("failed to open browser")
	}
}
