package audiosocket_test

import (
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/audiosocket"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// TestKinds pins the kind bytes Asterisk 22.11 uses (plan §2(a)) and the
// bridge's audio kind.
func TestKinds(t *testing.T) {
	for _, c := range []struct {
		kind audiosocket.Kind
		want byte
		rate int
	}{
		{audiosocket.KindHangup, 0x00, 0}, {audiosocket.KindUUID, 0x01, 0}, {audiosocket.KindDTMF, 0x03, 0},
		{audiosocket.KindSlin, 0x10, 8000}, {audiosocket.KindSlin12, 0x11, 12000}, {audiosocket.KindSlin16, 0x12, 16000},
		{audiosocket.KindSlin24, 0x13, 24000}, {audiosocket.KindSlin32, 0x14, 32000}, {audiosocket.KindSlin44, 0x15, 44100},
		{audiosocket.KindSlin48, 0x16, 48000}, {audiosocket.KindSlin96, 0x17, 96000}, {audiosocket.KindSlin192, 0x18, 192000},
		{audiosocket.KindError, 0xff, 0},
	} {
		if byte(c.kind) != c.want || c.kind.SampleRate() != c.rate || c.kind.IsAudio() != (c.rate > 0) {
			t.Errorf("%s = 0x%02x at %d Hz, want 0x%02x at %d Hz", c.kind, byte(c.kind), c.kind.SampleRate(), c.want, c.rate)
		}
		if k, ok := audiosocket.AudioKind(c.rate); c.rate > 0 && (!ok || k != c.kind) {
			t.Errorf("AudioKind(%d) = %s %v", c.rate, k, ok)
		}
	}
	if k, ok := audiosocket.AudioKind(streams.SampleRate); !ok || k != audiosocket.KindAudio || audiosocket.KindAudio.SampleRate() != streams.SampleRate {
		t.Errorf("AudioKind(%d) = %s %v; KindAudio %s", streams.SampleRate, k, ok, audiosocket.KindAudio)
	}
	for _, k := range []audiosocket.Kind{audiosocket.KindHangup, audiosocket.KindUUID, audiosocket.KindDTMF, audiosocket.KindError, 0x42} {
		if k.IsAudio() || k.SampleRate() != 0 {
			t.Errorf("%s is audio", k)
		}
	}
	if audiosocket.KindSlin16.String() != "slin16000" || audiosocket.Kind(0x42).String() != "kind(0x42)" {
		t.Errorf("String: %s %s", audiosocket.KindSlin16, audiosocket.Kind(0x42))
	}
	if _, ok := audiosocket.AudioKind(11025); ok {
		t.Error("AudioKind(11025) exists")
	}
	if audiosocket.HeaderSize != 3 || audiosocket.UUIDSize != 16 || audiosocket.MaxPayload != 65535 || streams.FrameBytes > audiosocket.MaxPayload {
		t.Error("frame bounds")
	}
}
