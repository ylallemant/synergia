//go:build !nosystray && windows

package tray

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

//go:embed phi_connected.png
var pngConnectedIdle []byte

//go:embed phi_processing.png
var pngProcessing []byte

//go:embed phi_reconnecting.png
var pngReconnecting []byte

//go:embed phi_paused.png
var pngPaused []byte

//go:embed phi_disconnected.png
var pngDisconnected []byte

var (
	iconConnectedIdle []byte
	iconProcessing    []byte
	iconReconnecting  []byte
	iconPaused        []byte
	iconDisconnected  []byte
)

func init() {
	iconConnectedIdle = buildICO(pngConnectedIdle)
	iconProcessing = buildICO(pngProcessing)
	iconReconnecting = buildICO(pngReconnecting)
	iconPaused = buildICO(pngPaused)
	iconDisconnected = buildICO(pngDisconnected)
}

// buildICO assembles a multi-size ICO container around the embedded 32×32
// PNG. A 16×16 version is generated at init time via a 2×2 box filter so
// the notification area at 100% DPI displays a crisp small icon instead of
// the OS-downscaled 32×32. Both sizes are stored as PNG payloads inside
// the ICO (Vista+ supports this — no BMP/DIB re-encoding needed).
//
// Layout:
//
//	[ICONDIR (6 B)]
//	[ICONDIRENTRY × 2 (32 B)]
//	[16×16 PNG]
//	[32×32 PNG]
func buildICO(src32 []byte) []byte {
	src16 := downscale2x(src32)
	if src16 == nil {
		// Box-filter failed; fall back to a single-size ICO with just the
		// 32×32 image so Windows still has something to render.
		return packICO([][]byte{src32})
	}
	return packICO([][]byte{src16, src32})
}

// packICO wraps one or more PNG payloads in an ICO container. Each PNG is
// inspected for its width/height (from the IHDR chunk) so the directory
// entries match the actual image dimensions.
func packICO(pngs [][]byte) []byte {
	var body bytes.Buffer
	// Header offset for the first image data = 6 (ICONDIR) + 16*N entries.
	imageOffset := uint32(6 + 16*len(pngs))

	var entries bytes.Buffer
	for _, p := range pngs {
		var width, height uint32
		if len(p) >= 24 {
			width = binary.BigEndian.Uint32(p[16:20])
			height = binary.BigEndian.Uint32(p[20:24])
		}
		w := byte(width)
		if width >= 256 {
			w = 0
		}
		h := byte(height)
		if height >= 256 {
			h = 0
		}

		entries.WriteByte(w)
		entries.WriteByte(h)
		entries.WriteByte(0) // palette size (0 for true colour)
		entries.WriteByte(0) // reserved
		_ = binary.Write(&entries, binary.LittleEndian, uint16(1))           // colour planes
		_ = binary.Write(&entries, binary.LittleEndian, uint16(32))          // bits per pixel
		_ = binary.Write(&entries, binary.LittleEndian, uint32(len(p)))      // image size
		_ = binary.Write(&entries, binary.LittleEndian, imageOffset)         // offset to image
		imageOffset += uint32(len(p))

		body.Write(p)
	}

	var buf bytes.Buffer
	// ICONDIR (6 bytes): reserved, type=1 (icon), image count.
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(pngs)))
	buf.Write(entries.Bytes())
	buf.Write(body.Bytes())
	return buf.Bytes()
}

// downscale2x returns a half-resolution PNG built from a 2×2 box-average of
// the source. Returns nil if decoding fails or the source isn't a clean
// even-divisor (in which case the caller falls back to a single-size ICO).
// Operates in NRGBA space so alpha is averaged consistently with the
// rendered, un-premultiplied source pixels.
func downscale2x(srcPNG []byte) []byte {
	src, err := png.Decode(bytes.NewReader(srcPNG))
	if err != nil {
		return nil
	}
	b := src.Bounds()
	if b.Dx() < 2 || b.Dy() < 2 || b.Dx()%2 != 0 || b.Dy()%2 != 0 {
		return nil
	}
	nw, nh := b.Dx()/2, b.Dy()/2
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		for x := 0; x < nw; x++ {
			var r, g, bl, a uint32
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					c := color.NRGBAModel.Convert(
						src.At(b.Min.X+x*2+dx, b.Min.Y+y*2+dy),
					).(color.NRGBA)
					r += uint32(c.R)
					g += uint32(c.G)
					bl += uint32(c.B)
					a += uint32(c.A)
				}
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				R: uint8(r / 4),
				G: uint8(g / 4),
				B: uint8(bl / 4),
				A: uint8(a / 4),
			})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil
	}
	return out.Bytes()
}
