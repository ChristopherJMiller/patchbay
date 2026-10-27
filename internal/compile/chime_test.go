package compile

import (
	"bytes"
	"testing"
)

func TestChime(t *testing.T) {
	c := chime()
	if len(c) != 2400 {
		t.Fatalf("len(chime) = %d, want 2400 (0.3 s at 8 kHz)", len(c))
	}
	if !bytes.Equal(c, chime()) {
		t.Error("chime is not deterministic")
	}
	// mu-law magnitude: the byte's low 7 bits, inverted; 0 is loudest.
	mag := func(b byte) int { return int(^b & 0x7f) }
	loud := func(s []byte) int { // largest decoded magnitude
		m := 0
		for _, b := range s {
			m = max(m, mag(b))
		}
		return m
	}
	mid := loud(c[1000:1080])
	if head := loud(c[:80]); head >= mid {
		t.Errorf("first 80 samples (%d) are not quieter than the middle (%d)", head, mid)
	}
	if tail := loud(c[len(c)-80:]); tail >= mid {
		t.Errorf("last 80 samples (%d) are not quieter than the middle (%d)", tail, mid)
	}
	if ChimeFile != "tftp/patchbay-chime.raw" {
		t.Errorf("ChimeFile = %q", ChimeFile)
	}
}
