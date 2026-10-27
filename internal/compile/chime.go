package compile

import "math"

// The message chime: the push plays it with the Play: URI
// (Play:patchbay-chime.raw) so a pushed message is heard as well as seen.
// The phone fetches it from TFTP like its config, and wants raw G.711 mu-law
// at 8 kHz, 240 to 16080 samples.
const (
	// MessagesPathPrefix is the URL path prefix of a phone's message page
	// on device HTTP: /messages/<device>.xml (the agent serves it, not the
	// bundle).
	MessagesPathPrefix = "/messages/"

	// ChimeFile is the chime in the bundle, in the tree phones fetch from.
	ChimeFile = TreeTFTP + "/patchbay-chime.raw"

	chimeRate = 8000
	chimeFade = chimeRate / 100 // 10 ms
)

// chime is two tones, 880 Hz for 120 ms and then 1320 Hz for 180 ms, at half
// scale with 10 ms linear fades in and out, 2400 samples of mu-law. It is
// computed, not stored: it is deterministic, so the bundle version is stable.
func chime() []byte {
	const (
		first  = chimeRate * 120 / 1000
		total  = first + chimeRate*180/1000
		scale  = 0.5 * 32767
		sample = 1.0 / chimeRate
	)
	out := make([]byte, total)
	for i := range out {
		freq, t := 880.0, float64(i)*sample
		if i >= first {
			freq, t = 1320.0, float64(i-first)*sample
		}
		amp := 1.0
		if i < chimeFade {
			amp = float64(i) / chimeFade
		} else if i >= total-chimeFade {
			amp = float64(total-1-i) / chimeFade
		}
		out[i] = mulaw(int(math.Round(scale * amp * math.Sin(2*math.Pi*freq*t))))
	}
	return out
}

// mulaw is the G.711 mu-law byte of a 16-bit linear sample.
func mulaw(pcm int) byte {
	const bias, clip = 0x84, 32635
	sign := byte(0)
	if pcm < 0 {
		pcm, sign = -pcm, 0x80
	}
	pcm = min(pcm, clip) + bias
	exp := 7
	for mask := 0x4000; pcm&mask == 0 && exp > 0; mask >>= 1 {
		exp--
	}
	mant := (pcm >> (exp + 3)) & 0x0f
	return ^(sign | byte(exp<<4) | byte(mant))
}
