package cli

import (
	"fmt"
	"io"
	"strings"

	"rsc.io/qr"
)

// qrQuietZone is the light border around the code, in modules: the four the
// QR specification asks for, so a phone camera finds the code on any
// terminal background.
const qrQuietZone = 4

// ANSI colours of the half-block renderer: the top module is the
// foreground of "▀", the bottom module its background. Explicit black on
// bright white, never the terminal's defaults, so the code reads the same
// on dark and light terminals.
const (
	qrDarkFG  = "30"
	qrLightFG = "97"
	qrDarkBG  = "40"
	qrLightBG = "107"
	qrReset   = "\x1b[0m"
)

// RenderQR draws text as a QR code (error correction level M) for a
// terminal: one column per module and two module rows per line, each cell
// an upper half block ("▀") whose foreground is the top module and whose
// background is the bottom one, in explicit ANSI black and bright white,
// with a four-module quiet zone. A ~60-byte provisioning URL is a
// version-4 code: 33 modules plus the zone, 41 columns by 21 lines. When
// the module count is odd, the last line's bottom half is light. Nothing is
// written when text cannot be encoded.
func RenderQR(w io.Writer, text string) error {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return fmt.Errorf("QR code: %w", err)
	}
	size := code.Size + 2*qrQuietZone
	// Black is false outside the code: the quiet zone, and the padding row
	// under an odd last row.
	dark := func(x, y int) bool { return code.Black(x-qrQuietZone, y-qrQuietZone) }
	var b strings.Builder
	for y := 0; y < size; y += 2 {
		for x := range size {
			fg, bg := qrLightFG, qrLightBG
			if dark(x, y) {
				fg = qrDarkFG
			}
			if dark(x, y+1) {
				bg = qrDarkBG
			}
			b.WriteString("\x1b[" + fg + ";" + bg + "m▀")
		}
		b.WriteString(qrReset + "\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}
