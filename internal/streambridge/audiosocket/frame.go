// Package audiosocket is Asterisk's AudioSocket protocol, as Asterisk 22.11
// speaks it: a TCP stream of frames, each a kind byte, a big-endian 16-bit
// payload length, then the payload.
//
// This file is the contract (types and constants only): the stream bridge
// stage adds the codec beside it. What Asterisk 22.11 does, checked on
// 2026-10-16 against the chart's pinned andrius/asterisk:22.11.0_debian-trixie
// with a probe server (docs/plans/release-b.md §2(a)):
//
//   - The connection opens with one KindUUID frame: the 16 bytes of the
//     UUID the dialplan named (streams.FormatUUID writes the same bytes as
//     text).
//   - Dial(AudioSocket/<host:port>/<uuid>/c(slin16)) sends the caller's
//     audio as KindSlin16 frames of 640 bytes, one per 20 ms, and plays
//     what the server sends. The AudioSocket() application sends KindSlin
//     (8 kHz, 320 bytes) whatever the channel's codec. Asterisk takes any
//     audio kind from the server and translates it.
//   - A key press on the caller's phone arrives as a KindDTMF frame with a
//     one-byte payload (the ASCII digit).
//   - Asterisk ends a call by closing the TCP connection: no KindHangup
//     frame first.
//   - A KindHangup frame from the server, or the server closing the
//     connection, ends Dial with DIALSTATUS=ANSWER, and the dialplan goes on
//     (Dial's g option): patchbay's plays "not available". A KindError frame
//     from the server is logged as an error by Asterisk and ends the call the
//     same way; the bridge never sends one. A server that cannot be
//     reached ends Dial with DIALSTATUS=CHANUNAVAIL, and the dialplan goes on
//     too.
//   - The host may be a name: Asterisk resolves it and tries each address.
//
// The bridge sends KindAudio frames of streams.FrameBytes, and refuses a
// call by sending KindHangup and closing.
package audiosocket

import "fmt"

// Kind is a frame's type, its first byte.
type Kind byte

// The frame kinds of Asterisk 22.11's res_audiosocket.
const (
	// KindHangup ends the call; no payload.
	KindHangup Kind = 0x00
	// KindUUID is the first frame Asterisk sends: a 16-byte UUID.
	KindUUID Kind = 0x01
	// KindDTMF is a key press: one ASCII byte.
	KindDTMF Kind = 0x03
	// KindSlin is 8 kHz signed linear audio (16-bit little-endian mono).
	KindSlin Kind = 0x10
	// KindSlin12 is 12 kHz signed linear audio.
	KindSlin12 Kind = 0x11
	// KindSlin16 is 16 kHz signed linear audio: what Dial's c(slin16)
	// carries, and the bridge's KindAudio.
	KindSlin16 Kind = 0x12
	// KindSlin24 is 24 kHz signed linear audio.
	KindSlin24 Kind = 0x13
	// KindSlin32 is 32 kHz signed linear audio.
	KindSlin32 Kind = 0x14
	// KindSlin44 is 44.1 kHz signed linear audio.
	KindSlin44 Kind = 0x15
	// KindSlin48 is 48 kHz signed linear audio.
	KindSlin48 Kind = 0x16
	// KindSlin96 is 96 kHz signed linear audio.
	KindSlin96 Kind = 0x17
	// KindSlin192 is 192 kHz signed linear audio.
	KindSlin192 Kind = 0x18
	// KindError is an error report: a one-byte code, if any.
	KindError Kind = 0xff
)

// KindAudio is the audio kind the bridge sends, at streams.SampleRate
// (TestKinds ties the two).
const KindAudio = KindSlin16

// Frame layout and bounds.
const (
	// HeaderSize is the kind byte plus the 16-bit payload length.
	HeaderSize = 3
	// MaxPayload is the largest payload the length field can carry.
	MaxPayload = 0xffff
	// UUIDSize is a KindUUID payload's length.
	UUIDSize = 16
)

// Frame is one AudioSocket frame.
type Frame struct {
	Kind    Kind
	Payload []byte
}

// audioKinds are the audio kinds, in rate order.
var audioKinds = []Kind{KindSlin, KindSlin12, KindSlin16, KindSlin24, KindSlin32, KindSlin44, KindSlin48, KindSlin96, KindSlin192}

// IsAudio reports whether frames of this kind carry signed linear audio.
func (k Kind) IsAudio() bool { return k.SampleRate() > 0 }

// SampleRate is an audio kind's sample rate in Hz; 0 for any other kind.
func (k Kind) SampleRate() int {
	switch k {
	case KindSlin:
		return 8000
	case KindSlin12:
		return 12000
	case KindSlin16:
		return 16000
	case KindSlin24:
		return 24000
	case KindSlin32:
		return 32000
	case KindSlin44:
		return 44100
	case KindSlin48:
		return 48000
	case KindSlin96:
		return 96000
	case KindSlin192:
		return 192000
	case KindHangup, KindUUID, KindDTMF, KindError:
		return 0
	}
	return 0
}

// AudioKind is the audio kind of a sample rate, and whether there is one.
func AudioKind(rate int) (Kind, bool) {
	for _, k := range audioKinds {
		if k.SampleRate() == rate {
			return k, true
		}
	}
	return 0, false
}

func (k Kind) String() string {
	switch k {
	case KindHangup:
		return "hangup"
	case KindUUID:
		return "uuid"
	case KindDTMF:
		return "dtmf"
	case KindError:
		return "error"
	case KindSlin, KindSlin12, KindSlin16, KindSlin24, KindSlin32, KindSlin44, KindSlin48, KindSlin96, KindSlin192:
		return fmt.Sprintf("slin%d", k.SampleRate())
	}
	return fmt.Sprintf("kind(0x%02x)", byte(k))
}
