package streams

import "time"

// The audio a stream call carries over AudioSocket, both ways: 16 kHz
// signed linear, mono, 16-bit little-endian, in 20 ms frames.
//
// Why 16 kHz (docs/plans/release-b.md §2(a), checked 2026-10-16 against the
// chart's pinned andrius/asterisk:22.11.0_debian-trixie): Asterisk 22.11's
// AudioSocket() application is fixed at 8 kHz slin towards the server
// (kind 0x10, 320-byte frames), while its AudioSocket channel driver,
// Dial(AudioSocket/<host:port>/<uuid>/c(slin16)), carries kind 0x12,
// 640-byte frames both ways. Over a G.722-only SIP call a 1 kHz + 6 kHz
// test signal from the server reached the far end with both tones through
// the channel driver: wideband to the 79x5s (G.722), and Asterisk
// resamples it for G.711 callers (codec_resample).
const (
	// Codec is Asterisk's name for the format, as the dialplan's
	// Dial(AudioSocket/…/c(Codec)) asks for it.
	Codec = "slin16"

	// SampleRate is the sample rate in Hz: ffmpeg's -ar, and the rate of
	// the AudioSocket audio kind the bridge sends.
	SampleRate = 16000

	// FrameDuration is the length of one audio frame; the bridge paces
	// each caller at one frame per FrameDuration.
	FrameDuration = 20 * time.Millisecond

	// FrameSamples is how many samples one frame holds (320).
	FrameSamples = SampleRate * int(FrameDuration/time.Millisecond) / 1000

	// FrameBytes is one frame's AudioSocket payload: FrameSamples 16-bit
	// samples (640 bytes).
	FrameBytes = FrameSamples * 2
)
