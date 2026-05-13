//go:build !nosystray && !windows

package tray

import _ "embed"

// Tray icons — phi symbol rendered at 32×32 px in each status colour.
// Regenerate with: ./internal/client/tray/generate_icons.sh
//
// On macOS and Linux, fyne.io/systray accepts PNG bytes directly.
// Windows requires ICO format — see icons_windows.go for the wrapper.

//go:embed phi_connected.png
var iconConnectedIdle []byte

//go:embed phi_processing.png
var iconProcessing []byte

//go:embed phi_reconnecting.png
var iconReconnecting []byte

//go:embed phi_paused.png
var iconPaused []byte

//go:embed phi_disconnected.png
var iconDisconnected []byte
