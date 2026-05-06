//go:build !nosystray

package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Minimal 16x16 PNG icons for the system tray.
// Each is a solid circle in the appropriate color.

// iconConnectedIdle — green circle: connected and ready
var iconConnectedIdle = generateIcon(0x4C, 0xAF, 0x50)

// iconProcessing — blue circle: connected and processing work
var iconProcessing = generateIcon(0x21, 0x96, 0xF3)

// iconReconnecting — yellow circle: reconnecting or GPU busy
var iconReconnecting = generateIcon(0xFF, 0xC1, 0x07)

// iconPaused — grey circle: paused by user
var iconPaused = generateIcon(0x9E, 0x9E, 0x9E)

// iconDisconnected — red circle: disconnected or error
var iconDisconnected = generateIcon(0xF4, 0x43, 0x36)

func generateIcon(r, g, b uint8) []byte {
	const size = 16
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	center := float64(size) / 2.0
	radius := center - 1.5

	fillColor := color.RGBA{R: r, G: g, B: b, A: 255}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) - center + 0.5
			dy := float64(y) - center + 0.5
			if math.Sqrt(dx*dx+dy*dy) <= radius {
				img.SetRGBA(x, y, fillColor)
			}
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
