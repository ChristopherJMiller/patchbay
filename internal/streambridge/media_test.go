package streambridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// Test media is generated, never committed (hard rule 4): WAVs here, MPEG-TS
// by the dev shell's ffmpeg into t.TempDir().

// needFFmpeg is the dev shell's ffmpeg. Tests that run it are skipped under
// -short; under plain go test (make test, CI) a missing ffmpeg fails them.
func needFFmpeg(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the real ffmpeg (not under -short)")
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("no ffmpeg on PATH (run the tests in nix develop): %v", err)
	}
	return path
}

// tone is n samples of a sine at hz, sampled at rate, at half of full
// scale.
func tone(n int, hz, rate int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(0.5 * math.MaxInt16 * math.Sin(2*math.Pi*float64(hz)*float64(i)/float64(rate)))
	}
	return out
}

// wav is a 16-bit PCM WAV of seconds of a hz tone at rate, in channels
// channels (the same samples in each).
func wav(seconds float64, hz, rate, channels int) []byte {
	samples := tone(int(seconds*float64(rate)), hz, rate)
	data := make([]byte, 0, len(samples)*2*channels)
	for _, s := range samples {
		for range channels {
			data = binary.LittleEndian.AppendUint16(data, uint16(s))
		}
	}
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	le(uint32(36 + len(data)))
	b.WriteString("WAVEfmt ")
	le(uint32(16))
	le(uint16(1)) // PCM
	le(uint16(channels))
	le(uint32(rate))
	le(uint32(rate * channels * 2))
	le(uint16(channels * 2))
	le(uint16(16))
	b.WriteString("data")
	le(uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// realTime serves body at bytesPerSecond, in a chunk every 20 ms, like a
// live source; it stops when the client goes away.
func realTime(body []byte, bytesPerSecond int, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := body
		w.Header().Set("Content-Type", contentType)
		chunk := max(bytesPerSecond/50, 1)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for len(body) > 0 {
			n := min(chunk, len(body))
			if _, err := w.Write(body[:n]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			body = body[n:]
			select {
			case <-tick.C:
			case <-r.Context().Done():
				return
			}
		}
	}
}

// mpegTS has the dev shell's ffmpeg make seconds of an MPEG-TS like an
// HDHomeRun's: MPEG-2 video and AC-3 stereo audio, the audio a hz tone.
func mpegTS(t *testing.T, ffmpeg string, seconds, hz int) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "tuner.ts")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=frequency="+strconv.Itoa(hz)+":sample_rate=48000",
		"-t", strconv.Itoa(seconds), "-c:v", "mpeg2video", "-c:a", "ac3", "-ac", "2", "-f", "mpegts", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("making the MPEG-TS: %v: %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// samples decodes signed 16-bit little-endian PCM.
func samples(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	return out
}

// toneRatio is the share of the signal's power at hz (Goertzel, over
// windows of streams.SampleRate/50 samples, on which 50 Hz multiples are
// whole bins): about 1 for a pure tone at hz, about 0 for silence or
// another tone.
func toneRatio(x []int16, hz int) float64 {
	const n = streams.SampleRate / 50
	var at, total float64
	w := 2 * math.Pi * float64(hz) / streams.SampleRate
	for start := 0; start+n <= len(x); start += n {
		var s1, s2, energy float64
		for _, v := range x[start : start+n] {
			f := float64(v)
			s := f + 2*math.Cos(w)*s1 - s2
			s2, s1 = s1, s
			energy += f * f
		}
		power := s1*s1 + s2*s2 - 2*math.Cos(w)*s1*s2
		at += 2 * power / n
		total += energy
	}
	if total == 0 {
		return 0
	}
	return at / total
}

func TestToneRatio(t *testing.T) {
	if r := toneRatio(tone(16000, 1000, streams.SampleRate), 1000); r < 0.99 || r > 1.01 {
		t.Errorf("a 1 kHz tone at 1 kHz: %.3f", r)
	}
	if r := toneRatio(tone(16000, 1300, streams.SampleRate), 1000); r > 0.01 {
		t.Errorf("a 1.3 kHz tone at 1 kHz: %.3f", r)
	}
	if r := toneRatio(tone(16000, 1300, streams.SampleRate), 1300); r < 0.99 {
		t.Errorf("a 1.3 kHz tone at 1.3 kHz: %.3f", r)
	}
	if r := toneRatio(make([]int16, 16000), 1000); r != 0 {
		t.Errorf("silence: %.3f", r)
	}
}
