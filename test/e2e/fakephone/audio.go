package main

import "math"

// Audio is G.711 µ-law (PCMU, RTP payload type 0) at 8 kHz, 20 ms per packet:
// what a Cisco 79x5 sends when G.722 is not negotiated.
const (
	sampleRate      = 8000
	frameDuration   = 20 // ms
	samplesPerFrame = sampleRate * frameDuration / 1000
	payloadTypePCMU = 0
)

// G.711 µ-law constants (ITU-T G.711, the classic Sun reference algorithm).
const (
	ulawBias = 0x84
	ulawClip = 32635
)

// linearToULaw encodes one 16-bit linear sample as µ-law.
func linearToULaw(sample int16) byte {
	s := int(sample)
	sign := 0
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > ulawClip {
		s = ulawClip
	}
	s += ulawBias
	exponent := 7
	for mask := 0x4000; s&mask == 0 && exponent > 0; mask >>= 1 {
		exponent--
	}
	mantissa := (s >> (exponent + 3)) & 0x0F
	return ^byte(sign | exponent<<4 | mantissa)
}

// ulawToLinear decodes one µ-law byte to a 16-bit linear sample.
func ulawToLinear(u byte) int16 {
	u = ^u
	exponent := int(u>>4) & 0x07
	mantissa := int(u & 0x0F)
	s := ((mantissa << 3) + ulawBias) << exponent
	s -= ulawBias
	if u&0x80 != 0 {
		return int16(-s)
	}
	return int16(s)
}

// toneGenerator produces a continuous sine wave, frame by frame, so the
// phase never jumps between packets.
type toneGenerator struct {
	freq      float64
	amplitude float64 // 0..1 of full scale
	n         int     // samples produced so far
}

// nextFrame returns the next frame of µ-law samples.
func (g *toneGenerator) nextFrame() []byte {
	out := make([]byte, samplesPerFrame)
	for i := range out {
		t := float64(g.n) / sampleRate
		v := g.amplitude * math.MaxInt16 * math.Sin(2*math.Pi*g.freq*t)
		out[i] = linearToULaw(int16(v))
		g.n++
	}
	return out
}

// toneAnalysis is how much of a frame's energy sits at one frequency.
type toneAnalysis struct {
	// Ratio is the energy at the frequency over the frame's total energy:
	// about 1 for a pure tone at that frequency, about 0 for silence or a
	// different tone.
	Ratio float64
	// RMS is the frame's level in linear 16-bit units.
	RMS float64
}

// analyseULaw decodes a µ-law payload and measures freq in it with the
// Goertzel algorithm (one DFT bin, no FFT).
func analyseULaw(payload []byte, freq float64) toneAnalysis {
	samples := make([]float64, len(payload))
	for i, b := range payload {
		samples[i] = float64(ulawToLinear(b))
	}
	return analyse(samples, freq)
}

func analyse(samples []float64, freq float64) toneAnalysis {
	n := len(samples)
	if n == 0 {
		return toneAnalysis{}
	}
	k := math.Round(float64(n) * freq / sampleRate)
	coeff := 2 * math.Cos(2*math.Pi*k/float64(n))
	var s1, s2, energy float64
	for _, x := range samples {
		s0 := x + coeff*s1 - s2
		s2, s1 = s1, s0
		energy += x * x
	}
	if energy == 0 {
		return toneAnalysis{}
	}
	// |X(k)|² for a pure sinusoid of amplitude A on bin k is (A·N/2)², and
	// its energy is A²·N/2, so this ratio is 1 for a pure tone.
	power := s1*s1 + s2*s2 - coeff*s1*s2
	return toneAnalysis{
		Ratio: power / (energy * float64(n) / 2),
		RMS:   math.Sqrt(energy / float64(n)),
	}
}

// toneDetected is the per-frame verdict: mostly this frequency, and loud
// enough not to be comfort noise.
func (a toneAnalysis) toneDetected() bool {
	return a.Ratio >= 0.6 && a.RMS >= 200
}

// loud reports whether a frame carries sound at all: comfort noise and
// Asterisk's silence stay below this level.
func (a toneAnalysis) loud() bool { return a.RMS >= 200 }

// sitFrequencies are the special information tone's three tones (950, 1400
// and 1800 Hz), which Asterisk plays before a prompt.
var sitFrequencies = []float64{950, 1400, 1800}

// classifyULaw sorts a µ-law frame into loud or not, and, if loud, into the
// SIT tone or anything else (a spoken prompt).
func classifyULaw(payload []byte) (loud, sit bool) {
	for _, f := range sitFrequencies {
		a := analyseULaw(payload, f)
		if !a.loud() {
			return false, false
		}
		if a.toneDetected() {
			return true, true
		}
	}
	return true, false
}
