package main

import (
	"math"
	"testing"
)

func TestULawKnownValues(t *testing.T) {
	// Reference points of G.711 µ-law: silence is 0xFF, full scale is
	// ±32124, and the sign lives in bit 7.
	cases := []struct {
		linear int16
		ulaw   byte
	}{
		{0, 0xFF},
		{32124, 0x80},
		{-32124, 0x00},
		{math.MaxInt16, 0x80}, // clipped
		{math.MinInt16, 0x00}, // clipped
	}
	for _, c := range cases {
		if got := linearToULaw(c.linear); got != c.ulaw {
			t.Errorf("linearToULaw(%d) = %#02x, want %#02x", c.linear, got, c.ulaw)
		}
	}
	if got := ulawToLinear(0xFF); got != 0 {
		t.Errorf("ulawToLinear(0xFF) = %d, want 0", got)
	}
	if got := ulawToLinear(0x80); got != 32124 {
		t.Errorf("ulawToLinear(0x80) = %d, want 32124", got)
	}
}

func TestULawRoundTrip(t *testing.T) {
	// Every code decodes to a value that encodes back to the same code
	// (except negative zero, 0x7F, which encodes as positive zero).
	for c := range 256 {
		u := byte(c)
		if u == 0x7F {
			continue
		}
		if got := linearToULaw(ulawToLinear(u)); got != u {
			t.Errorf("code %#02x -> %d -> %#02x", u, ulawToLinear(u), got)
		}
	}
	// Quantisation error stays within µ-law's step size (about 3% of the
	// value, at least a few units near zero).
	for s := -32000; s <= 32000; s += 37 {
		got := float64(ulawToLinear(linearToULaw(int16(s))))
		if err := math.Abs(got - float64(s)); err > math.Max(16, 0.04*math.Abs(float64(s))) {
			t.Fatalf("round trip of %d gave %v (error %v)", s, got, err)
		}
	}
}

// sine is one frame of a tone at freq.
func sine(freq float64) []float64 {
	out := make([]float64, samplesPerFrame)
	for i := range out {
		out[i] = 10000 * math.Sin(2*math.Pi*freq*float64(i)/sampleRate)
	}
	return out
}

func TestAnalyse(t *testing.T) {
	cases := []struct {
		name    string
		samples []float64
		detect  bool
	}{
		{"1 kHz", sine(1000), true},
		{"400 Hz", sine(400), false},
		{"1.5 kHz", sine(1500), false},
		{"silence", make([]float64, samplesPerFrame), false},
		{"quiet 1 kHz (comfort-noise level)", func() []float64 {
			s := sine(1000)
			for i := range s {
				s[i] /= 100
			}
			return s
		}(), false},
	}
	for _, c := range cases {
		a := analyse(c.samples, 1000)
		if got := a.toneDetected(); got != c.detect {
			t.Errorf("%s: detected=%v (ratio %.3f, rms %.0f), want %v", c.name, got, a.Ratio, a.RMS, c.detect)
		}
	}
}

func TestToneSurvivesULaw(t *testing.T) {
	g := &toneGenerator{freq: 1000, amplitude: 0.5}
	for i := range 10 {
		frame := g.nextFrame()
		if len(frame) != samplesPerFrame {
			t.Fatalf("frame %d has %d samples, want %d", i, len(frame), samplesPerFrame)
		}
		a := analyseULaw(frame, 1000)
		if !a.toneDetected() || a.Ratio < 0.95 {
			t.Fatalf("frame %d: ratio %.3f rms %.0f, want a clean 1 kHz tone", i, a.Ratio, a.RMS)
		}
		if other := analyseULaw(frame, 400); other.toneDetected() {
			t.Fatalf("frame %d: a 1 kHz tone was detected at 400 Hz (ratio %.3f)", i, other.Ratio)
		}
	}
}
