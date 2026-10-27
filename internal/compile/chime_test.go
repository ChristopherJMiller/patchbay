package compile

import (
	"bytes"
	"math"
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
	// mu-law magnitude: the byte's low 7 bits, inverted, so 0xFF is silence and 0x00 the loudest.
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

// TestMulaw pins the encoder: the byte is inverted, so 0 is 0xFF, positive
// full scale 0x80 and negative full scale 0x00; a sample and its negation
// differ in the sign bit alone.
func TestMulaw(t *testing.T) {
	for _, tc := range []struct {
		pcm  int
		want byte
	}{{0, 0xFF}, {32767, 0x80}, {-32767, 0x00}, {-1, 0x7F}} {
		if got := mulaw(tc.pcm); got != tc.want {
			t.Errorf("mulaw(%d) = %#02x, want %#02x", tc.pcm, got, tc.want)
		}
	}
	for _, pcm := range []int{1, 100, 5000, 32767} {
		if p, n := mulaw(pcm), mulaw(-pcm); p^n != 0x80 {
			t.Errorf("mulaw(%d) = %#02x and mulaw(%d) = %#02x differ by more than the sign bit", pcm, p, -pcm, n)
		}
	}
}

// TestChimeSecondTone: the 1320 Hz tone starts at sample 960 (120 ms), at
// phase zero, and runs on 1320 Hz and not on the first tone's 880 Hz.
func TestChimeSecondTone(t *testing.T) {
	c := chime()
	if c[960] != 0xFF {
		t.Errorf("sample 960 = %#02x, want 0xFF (phase zero)", c[960])
	}
	for _, i := range []int{961, 970, 1000, 1100} {
		amp := 1.0
		if i >= 2400-chimeFade {
			amp = float64(2400-1-i) / chimeFade
		}
		want := mulaw(int(math.Round(0.5 * 32767 * amp * math.Sin(2*math.Pi*1320*float64(i-960)/8000))))
		if c[i] != want {
			t.Errorf("sample %d = %#02x, want %#02x (1320 Hz)", i, c[i], want)
		}
	}
}
