package audiosocket_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/audiosocket"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// allKinds is every kind Asterisk 22.11 knows, and one it does not.
var allKinds = []audiosocket.Kind{
	audiosocket.KindHangup, audiosocket.KindUUID, audiosocket.KindDTMF,
	audiosocket.KindSlin, audiosocket.KindSlin12, audiosocket.KindSlin16, audiosocket.KindSlin24,
	audiosocket.KindSlin32, audiosocket.KindSlin44, audiosocket.KindSlin48, audiosocket.KindSlin96,
	audiosocket.KindSlin192, audiosocket.KindError, 0x42,
}

// pattern is n bytes that differ from one position to the next.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

func TestFrameRoundTrip(t *testing.T) {
	for _, k := range allKinds {
		for _, n := range []int{0, 1, streams.FrameBytes, audiosocket.MaxPayload} {
			payload := pattern(n)
			var buf bytes.Buffer
			if err := audiosocket.WriteFrame(&buf, audiosocket.Frame{Kind: k, Payload: payload}); err != nil {
				t.Fatalf("%s/%d: WriteFrame: %v", k, n, err)
			}
			if buf.Len() != audiosocket.HeaderSize+n {
				t.Fatalf("%s/%d: wrote %d bytes, want %d", k, n, buf.Len(), audiosocket.HeaderSize+n)
			}
			if got := buf.Bytes()[:audiosocket.HeaderSize]; got[0] != byte(k) || int(got[1])<<8|int(got[2]) != n {
				t.Fatalf("%s/%d: header % x", k, n, got)
			}
			if appended := audiosocket.AppendFrame([]byte("x"), k, payload); !bytes.Equal(appended[1:], buf.Bytes()) || appended[0] != 'x' {
				t.Fatalf("%s/%d: AppendFrame differs from WriteFrame", k, n)
			}
			f, err := audiosocket.ReadFrame(&buf)
			if err != nil {
				t.Fatalf("%s/%d: ReadFrame: %v", k, n, err)
			}
			if f.Kind != k || !bytes.Equal(f.Payload, payload) {
				t.Fatalf("%s/%d: read back %s with %d bytes", k, n, f.Kind, len(f.Payload))
			}
			if _, err := audiosocket.ReadFrame(&buf); !errors.Is(err, io.EOF) {
				t.Fatalf("%s/%d: after the frame: %v, want io.EOF", k, n, err)
			}
		}
	}
}

func TestReadFrameSequence(t *testing.T) {
	var buf bytes.Buffer
	for _, f := range []audiosocket.Frame{
		{Kind: audiosocket.KindUUID, Payload: pattern(audiosocket.UUIDSize)},
		{Kind: audiosocket.KindSlin16, Payload: pattern(streams.FrameBytes)},
		{Kind: audiosocket.KindDTMF, Payload: []byte("5")},
		{Kind: audiosocket.KindHangup},
	} {
		buf.Write(audiosocket.AppendFrame(nil, f.Kind, f.Payload))
	}
	var kinds []audiosocket.Kind
	for {
		f, err := audiosocket.ReadFrame(&buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, f.Kind)
	}
	if want := []audiosocket.Kind{audiosocket.KindUUID, audiosocket.KindSlin16, audiosocket.KindDTMF, audiosocket.KindHangup}; !equalKinds(kinds, want) {
		t.Errorf("kinds %v, want %v", kinds, want)
	}
}

func equalKinds(a, b []audiosocket.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReadFrameErrors(t *testing.T) {
	if _, err := audiosocket.ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("empty input: %v, want io.EOF", err)
	}
	for name, raw := range map[string][]byte{
		"one header byte":  {0x12},
		"two header bytes": {0x12, 0x02},
		"no payload":       {0x12, 0x02, 0x80},
		"short payload":    append([]byte{0x12, 0x02, 0x80}, pattern(100)...),
	} {
		if _, err := audiosocket.ReadFrame(bytes.NewReader(raw)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: %v, want io.ErrUnexpectedEOF", name, err)
		}
	}
	boom := errors.New("boom")
	if _, err := audiosocket.ReadFrame(io.MultiReader(bytes.NewReader([]byte{0x12, 0, 4, 1}), errReader{boom})); !errors.Is(err, boom) {
		t.Errorf("a failing reader: %v, want its error", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// countingWriter counts its Write calls.
type countingWriter struct {
	writes int
	bytes.Buffer
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func TestWriteFrameIsOneWrite(t *testing.T) {
	for _, n := range []int{0, streams.FrameBytes, audiosocket.MaxPayload} {
		w := &countingWriter{}
		if err := audiosocket.WriteFrame(w, audiosocket.Frame{Kind: audiosocket.KindAudio, Payload: pattern(n)}); err != nil {
			t.Fatal(err)
		}
		if w.writes != 1 {
			t.Errorf("%d bytes: %d writes, want 1", n, w.writes)
		}
	}
}

// shortWriter accepts one byte less than it is given.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestWriteFrameErrors(t *testing.T) {
	boom := errors.New("boom")
	if err := audiosocket.WriteFrame(failingWriter{boom}, audiosocket.Frame{Kind: audiosocket.KindHangup}); !errors.Is(err, boom) {
		t.Errorf("failing writer: %v", err)
	}
	if err := audiosocket.WriteFrame(shortWriter{}, audiosocket.Frame{Kind: audiosocket.KindHangup}); !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("short write: %v", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteFrameTooLarge(t *testing.T) {
	w := &countingWriter{}
	if err := audiosocket.WriteFrame(w, audiosocket.Frame{Kind: audiosocket.KindAudio, Payload: make([]byte, audiosocket.MaxPayload+1)}); err == nil {
		t.Error("a payload over MaxPayload was written")
	}
	if w.writes != 0 {
		t.Errorf("%d writes for a refused frame", w.writes)
	}
	defer func() {
		if recover() == nil {
			t.Error("AppendFrame took a payload over MaxPayload")
		}
	}()
	audiosocket.AppendFrame(nil, audiosocket.KindAudio, make([]byte, audiosocket.MaxPayload+1))
}

func TestUUIDOf(t *testing.T) {
	want, err := streams.ParseUUID(streams.UUID("king5"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := audiosocket.UUIDOf(audiosocket.Frame{Kind: audiosocket.KindUUID, Payload: want[:]})
	if err != nil || got != want {
		t.Fatalf("UUIDOf = %x, %v; want %x", got, err, want)
	}
	if streams.FormatUUID(got) != streams.UUID("king5") {
		t.Errorf("formatted: %s", streams.FormatUUID(got))
	}
	for name, f := range map[string]audiosocket.Frame{
		"audio":      {Kind: audiosocket.KindSlin16, Payload: want[:]},
		"short":      {Kind: audiosocket.KindUUID, Payload: want[:15]},
		"long":       {Kind: audiosocket.KindUUID, Payload: append(want[:], 0)},
		"no payload": {Kind: audiosocket.KindUUID},
	} {
		if _, err := audiosocket.UUIDOf(f); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func FuzzReadFrame(f *testing.F) {
	u, _ := streams.ParseUUID(streams.UUID("king5"))
	f.Add(audiosocket.AppendFrame(nil, audiosocket.KindUUID, u[:]))
	f.Add(audiosocket.AppendFrame(nil, audiosocket.KindSlin16, pattern(streams.FrameBytes)))
	f.Add(audiosocket.AppendFrame(nil, audiosocket.KindDTMF, []byte("#")))
	f.Add(audiosocket.AppendFrame(nil, audiosocket.KindHangup, nil))
	f.Add([]byte{0xff, 0x00, 0x01, 0x02})
	f.Add([]byte{0x12, 0xff, 0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		r := bytes.NewReader(raw)
		for {
			fr, err := audiosocket.ReadFrame(r)
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("ReadFrame: %v", err)
				}
				return
			}
			if len(fr.Payload) > audiosocket.MaxPayload {
				t.Fatalf("payload of %d bytes", len(fr.Payload))
			}
			var back bytes.Buffer
			if err := audiosocket.WriteFrame(&back, fr); err != nil {
				t.Fatal(err)
			}
			again, err := audiosocket.ReadFrame(&back)
			if err != nil || again.Kind != fr.Kind || !bytes.Equal(again.Payload, fr.Payload) {
				t.Fatalf("round trip of %s: %v", fr.Kind, err)
			}
		}
	})
}
