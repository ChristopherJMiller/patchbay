package streambridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streambridge/audiosocket"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// startServer serves AudioSocket for m on a loopback port until the test
// ends, and returns its address.
func startServer(t *testing.T, m *manager, clk clock.WithTicker) (string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &server{m: m, clock: clk, log: logr.Discard()}
	errc := make(chan error, 1)
	go func() { errc <- srv.serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return ln.Addr().String(), cancel
}

// asterisk is a test's AudioSocket client, as Asterisk would connect.
type asterisk struct {
	t    *testing.T
	conn net.Conn
}

func dialBridge(t *testing.T, addr string) *asterisk {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &asterisk{t: t, conn: conn}
}

// call dials and sends the UUID of the stream named name.
func call(t *testing.T, addr, name string) *asterisk {
	t.Helper()
	a := dialBridge(t, addr)
	u, err := streams.ParseUUID(streams.UUID(name))
	if err != nil {
		t.Fatal(err)
	}
	a.send(audiosocket.KindUUID, u[:])
	return a
}

func (a *asterisk) send(k audiosocket.Kind, payload []byte) {
	a.t.Helper()
	if err := audiosocket.WriteFrame(a.conn, audiosocket.Frame{Kind: k, Payload: payload}); err != nil {
		a.t.Fatal(err)
	}
}

// next is the next frame from the bridge, within d.
func (a *asterisk) next(d time.Duration) (audiosocket.Frame, error) {
	_ = a.conn.SetReadDeadline(time.Now().Add(d))
	return audiosocket.ReadFrame(a.conn)
}

// audio reads n frames, each of which must be KindAudio of FrameBytes.
func (a *asterisk) audio(n int) [][]byte {
	a.t.Helper()
	out := make([][]byte, 0, n)
	for range n {
		f, err := a.next(2 * time.Second)
		if err != nil {
			a.t.Fatalf("after %d frames: %v", len(out), err)
		}
		if f.Kind != audiosocket.KindAudio || len(f.Payload) != streams.FrameBytes {
			a.t.Fatalf("frame %d: %s of %d bytes", len(out), f.Kind, len(f.Payload))
		}
		out = append(out, f.Payload)
	}
	return out
}

// hungUp reads until the bridge's hangup frame (skipping audio), then
// expects the connection to end.
func (a *asterisk) hungUp() {
	a.t.Helper()
	for {
		f, err := a.next(5 * time.Second)
		if err != nil {
			a.t.Fatalf("no hangup frame: %v", err)
		}
		if f.Kind == audiosocket.KindHangup {
			break
		}
		if f.Kind != audiosocket.KindAudio {
			a.t.Fatalf("a %s frame before the hangup", f.Kind)
		}
	}
	a.closed()
}

// closed expects the connection to end with no frame.
func (a *asterisk) closed() {
	a.t.Helper()
	if f, err := a.next(5 * time.Second); !errors.Is(err, io.EOF) {
		a.t.Fatalf("after the end: %s frame, %v; want EOF", f.Kind, err)
	}
}

// refused expects exactly one hangup frame, then the end.
func (a *asterisk) refused() {
	a.t.Helper()
	f, err := a.next(5 * time.Second)
	if err != nil || f.Kind != audiosocket.KindHangup || len(f.Payload) != 0 {
		a.t.Fatalf("first frame %s (%d bytes), %v; want one hangup", f.Kind, len(f.Payload), err)
	}
	a.closed()
}

var silentFrame = make([]byte, streams.FrameBytes)

func TestServeUnknownUUID(t *testing.T) {
	m, _ := testManager(t, newFakeSource(), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	call(t, addr, "not-a-stream").refused()
	if o := m.observations().Bridge; o.UnknownCalls != 1 {
		t.Errorf("unknown calls %d", o.UnknownCalls)
	}
}

func TestServeIDTimeout(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	m, _ := testManager(t, newFakeSource(), clk, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clk)
	a := dialBridge(t, addr)
	eventually(t, "the ID timer", clk.HasWaiters)
	clk.Step(IDTimeout - time.Millisecond)
	if _, err := a.next(50 * time.Millisecond); !os.IsTimeout(err) {
		t.Fatalf("before IDTimeout: %v, want nothing yet", err)
	}
	clk.Step(time.Millisecond)
	a.closed()
	eventually(t, "the call count", func() bool { return m.observations().Bridge.Calls == 0 })
}

func TestServeNotUUIDFirst(t *testing.T) {
	m, _ := testManager(t, newFakeSource(), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	for name, f := range map[string]audiosocket.Frame{
		"audio":      {Kind: audiosocket.KindSlin16, Payload: silentFrame},
		"short uuid": {Kind: audiosocket.KindUUID, Payload: []byte{1, 2, 3, 4}},
		"hangup":     {Kind: audiosocket.KindHangup},
	} {
		t.Run(name, func(t *testing.T) {
			a := dialBridge(t, addr)
			a.send(f.Kind, f.Payload)
			a.closed()
		})
	}
	if o := m.observations(); o.Bridge.UnknownCalls != 0 || o.Streams[0].Calls != 0 {
		t.Errorf("connections without a UUID counted as calls: %+v", o.Bridge)
	}
}

func TestServePacing(t *testing.T) {
	m, _ := testManager(t, newFakeSource().set(srcRadio, toneForever(1000)), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	a := call(t, addr, "groovesalad")
	const n = 250
	arrived := make([]time.Time, n)
	for i := range n {
		f, err := a.next(time.Second)
		if err != nil || f.Kind != audiosocket.KindAudio {
			t.Fatalf("frame %d: %s, %v", i, f.Kind, err)
		}
		arrived[i] = time.Now()
	}
	for i, at := range arrived {
		if early := arrived[0].Add(time.Duration(i-1) * streams.FrameDuration).Sub(at); early > 0 {
			t.Fatalf("frame %d came %s more than one frame early", i, early)
		}
	}
	if took, want := arrived[n-1].Sub(arrived[0]), time.Duration(n-1)*streams.FrameDuration; took < want-60*time.Millisecond || took > want+60*time.Millisecond {
		t.Errorf("%d frames took %s, want %s ± 60 ms", n, took, want)
	}
}

func TestServeSilenceWhileStarting(t *testing.T) {
	m, reg := testManager(t, newFakeSource().set(srcRadio, pacedTone(1000, time.Second)), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	a := call(t, addr, "groovesalad")
	silent := 0
	for {
		f := a.audio(1)[0]
		if !bytes.Equal(f, silentFrame) {
			if r := toneRatio(samples(f), 1000); r < 0.8 {
				t.Errorf("the first sound is not the tone: %.2f", r)
			}
			break
		}
		silent++
		if silent > 100 {
			t.Fatal("no tone after 2 s")
		}
	}
	if silent < 40 || silent > 60 {
		t.Errorf("%d silent frames before the tone, want about 50 (1 s)", silent)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_underrun_frames_total", ""); v != 0 {
		t.Errorf("silence while Starting counted as %v underruns", v)
	}
}

func TestServeBurst(t *testing.T) {
	m, reg := testManager(t, newFakeSource().set(srcRadio, numbered(250)), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	a := call(t, addr, "groovesalad")
	var got []int
	for _, f := range a.audio(80) {
		if n := frameNumber(f); n >= 0 {
			got = append(got, n)
		} else if !bytes.Equal(f, silentFrame) {
			t.Fatal("a frame that is neither the source's nor silence")
		}
	}
	if len(got) > QueueFrames+1 || len(got) < QueueFrames || got[len(got)-1] != 249 {
		t.Fatalf("got %d of the burst's frames, the last %v; want the newest %d", len(got), got[len(got)-1:], QueueFrames)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("frames out of order: %v", got)
		}
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_underrun_frames_total", ""); v < 1 {
		t.Error("the silence after the burst was not counted as underruns")
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_frames_dropped_total", ""); v < 250-QueueFrames-1 {
		t.Errorf("dropped %v", v)
	}
}

func TestServeDiscardsUpstream(t *testing.T) {
	m, _ := testManager(t, newFakeSource(), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	a := call(t, addr, "groovesalad")
	a.audio(5)
	for range 20 {
		a.send(audiosocket.KindSlin16, silentFrame)
		a.send(audiosocket.KindDTMF, []byte("5"))
		a.send(0x42, []byte("an unknown kind"))
		a.send(audiosocket.KindSlin, make([]byte, 320))
	}
	a.audio(25)
	if o := streamObs(t, m, "groovesalad"); o.Listeners != 1 || o.LastFailure != nil {
		t.Errorf("after the caller's frames: %+v", o)
	}
	a.send(audiosocket.KindHangup, nil)
	for {
		if _, err := a.next(2 * time.Second); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("after the caller's hangup: %v, want the bridge to close", err)
			}
			break
		}
	}
	eventually(t, "the listener to leave", func() bool { return streamObs(t, m, "groovesalad").Listeners == 0 })
}

func TestServeCallerHangsUp(t *testing.T) {
	m, _ := testManager(t, newFakeSource(), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	a := call(t, addr, "groovesalad")
	a.audio(3)
	if o := m.observations(); o.Bridge.Calls != 1 || o.Streams[0].Listeners != 1 {
		t.Fatalf("during the call: %+v", o)
	}
	_ = a.conn.Close()
	eventually(t, "the listener to leave", func() bool {
		o := m.observations()
		return o.Bridge.Calls == 0 && o.Streams[0].Listeners == 0 && o.Streams[0].Session != nil &&
			o.Streams[0].Session.State == api.SessionDraining
	})
}

func TestServeCallerError(t *testing.T) {
	m, _ := testManager(t, newFakeSource(), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	a := call(t, addr, "groovesalad")
	a.audio(1)
	a.send(audiosocket.KindError, []byte{1})
	eventually(t, "the listener to leave", func() bool { return streamObs(t, m, "groovesalad").Listeners == 0 })
}

func TestServeMaxCalls(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch) // no ID timeout fires
	m, reg := testManager(t, newFakeSource(), clk, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clk)
	for range MaxCalls {
		dialBridge(t, addr)
	}
	eventually(t, "every connection counted", func() bool { return m.observations().Bridge.Calls == MaxCalls })
	dialBridge(t, addr).refused()
	if o := m.observations().Bridge; o.FullCalls != 1 || o.Calls != MaxCalls {
		t.Errorf("full calls %d, calls %d", o.FullCalls, o.Calls)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_refused_calls_total", "reason=bridge_full"); v != 1 {
		t.Errorf("refused bridge_full %v", v)
	}
}

func TestServeRefusalIsHangup(t *testing.T) {
	m, _ := testManager(t, newFakeSource(), clock.RealClock{}, doc(t, []streams.Pool{hdhomerun}, king5, kiro7))
	addr, _ := startServer(t, m, clock.RealClock{})
	holder := call(t, addr, "king5")
	holder.audio(2)
	call(t, addr, "kiro7").refused()
	if f := streamObs(t, m, "kiro7").LastFailure; f == nil || f.Reason != api.FailureTunerBusy {
		t.Errorf("kiro7's failure: %+v", f)
	}
	holder.audio(2)
}

func TestServeSourceFailureIsHangup(t *testing.T) {
	why := errors.New("ffmpeg exited with status 0: the source ended")
	m, _ := testManager(t, newFakeSource().set(srcRadio, failAfter(5, why)), clock.RealClock{}, doc(t, nil, groovesalad))
	addr, _ := startServer(t, m, clock.RealClock{})
	call(t, addr, "groovesalad").hungUp()
	if f := streamObs(t, m, "groovesalad").LastFailure; f == nil || f.Reason != api.FailureSourceEnded || f.Message != why.Error() {
		t.Errorf("failure %+v", f)
	}
}

func TestServeShutdown(t *testing.T) {
	src := newFakeSource()
	m, _ := testManager(t, src, clock.RealClock{}, doc(t, []streams.Pool{hdhomerun}, king5, groovesalad))
	addr, stop := startServer(t, m, clock.RealClock{})
	a, b := call(t, addr, "king5"), call(t, addr, "groovesalad")
	a.audio(1)
	b.audio(1)
	waiting := dialBridge(t, addr) // no UUID yet
	eventually(t, "three connections", func() bool { return m.observations().Bridge.Calls == 3 })
	stop()
	a.hungUp()
	b.hungUp()
	waiting.closed()
	eventually(t, "the listener to close", func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	})
}
