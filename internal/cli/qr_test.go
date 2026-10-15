package cli_test

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"rsc.io/qr"

	"github.com/ChristopherJMiller/patchbay/internal/cli"
)

// cellRE is one rendered cell: the top module's foreground, the bottom
// module's background, then the upper half block.
var cellRE = regexp.MustCompile("^\x1b\\[(30|97);(40|107)m▀")

// decodeHalfBlocks turns RenderQR's output back into a module grid (true is
// dark), two module rows per line. It fails the test on anything that is not
// exactly the documented encoding.
func decodeHalfBlocks(t *testing.T, out string) [][]bool {
	t.Helper()
	if !strings.HasSuffix(out, "\x1b[0m\n") {
		t.Fatalf("output does not end with a reset and a newline: %q", out[max(0, len(out)-20):])
	}
	var grid [][]bool
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		rest, ok := strings.CutSuffix(line, "\x1b[0m")
		if !ok {
			t.Fatalf("line %d does not end with a reset: %q", i, line)
		}
		var top, bottom []bool
		for rest != "" {
			m := cellRE.FindStringSubmatch(rest)
			if m == nil {
				t.Fatalf("line %d: not a cell at %q", i, rest[:min(len(rest), 16)])
			}
			top = append(top, m[1] == "30")
			bottom = append(bottom, m[2] == "40")
			rest = rest[len(m[0]):]
		}
		grid = append(grid, top, bottom)
	}
	return grid
}

func TestRenderQR(t *testing.T) {
	// A provisioning URL is about 60 bytes: http://<pbx>:8088/p/<32 characters>.
	const url = "http://192.168.0.240:8088/p/not-a-secret-provisioning-token1"
	if len(url) != 60 {
		t.Fatalf("test URL is %d bytes, want 60", len(url))
	}
	var buf bytes.Buffer
	if err := cli.RenderQR(&buf, url); err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(url, qr.M)
	if err != nil {
		t.Fatal(err)
	}
	const quiet = 4
	size := code.Size + 2*quiet
	grid := decodeHalfBlocks(t, buf.String())

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if want := (size + 1) / 2; len(lines) != want {
		t.Errorf("%d lines, want %d (two module rows per line)", len(lines), want)
	}
	for i, line := range lines {
		visible := utf8.RuneCountInString(regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(line, ""))
		if visible != size {
			t.Errorf("line %d is %d columns, want %d", i, visible, size)
		}
		if visible > 80 {
			t.Errorf("line %d is %d columns, over 80", i, visible)
		}
	}
	for y := range grid {
		for x := range grid[y] {
			want := code.Black(x-quiet, y-quiet) // outside the code (quiet zone, the odd last row) is light
			if y >= size {
				want = false
			}
			if grid[y][x] != want {
				t.Fatalf("module (%d,%d) dark=%v, want %v", x, y, grid[y][x], want)
			}
		}
	}
	// The quiet zone really is four light modules on every side.
	for y := range size {
		for x := range size {
			inCode := x >= quiet && x < size-quiet && y >= quiet && y < size-quiet
			if !inCode && grid[y][x] {
				t.Fatalf("quiet zone module (%d,%d) is dark", x, y)
			}
		}
	}
}

// A version-4 code (33 modules) plus the quiet zone is odd, so the last line's
// bottom half is the light padding row.
func TestRenderQROddLastRow(t *testing.T) {
	var buf bytes.Buffer
	if err := cli.RenderQR(&buf, "http://192.168.0.240:8088/p/not-a-secret-provisioning-token1"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	last := lines[len(lines)-1]
	if strings.Contains(last, ";40m") {
		t.Errorf("the last line has a dark bottom module: %q", last)
	}
}

func TestRenderQRTooLong(t *testing.T) {
	var buf bytes.Buffer
	if err := cli.RenderQR(&buf, strings.Repeat("x", 4000)); err == nil {
		t.Error("RenderQR of 4000 bytes: no error")
	}
	if buf.Len() != 0 {
		t.Errorf("RenderQR wrote %d bytes before failing", buf.Len())
	}
}
