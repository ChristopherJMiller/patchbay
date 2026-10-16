package streambridge

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// A producer writes a fake decoder's audio to w until ctx ends (it then
// returns ctx.Err()) or it ends by itself (returning why).
type producer func(ctx context.Context, w io.Writer) error

// fakeDecoder is a Decoder over a producer.
type fakeDecoder struct {
	ctx  context.Context
	r    *io.PipeReader
	done chan struct{}
	err  error
}

func newFakeDecoder(ctx context.Context, p producer) *fakeDecoder {
	r, w := io.Pipe()
	d := &fakeDecoder{ctx: ctx, r: r, done: make(chan struct{})}
	go func() {
		err := p(ctx, w)
		if ctx.Err() != nil {
			err = nil // Decoder.Wait: nil once its ctx ended
		}
		d.err = err
		_ = w.Close()
		close(d.done)
	}()
	return d
}

func (d *fakeDecoder) Read(p []byte) (int, error) { return d.r.Read(p) }

func (d *fakeDecoder) Wait() error {
	<-d.done
	return d.err
}

// fakeSource is a Source whose decoders run the producer set for their
// source (without one, a 1 kHz tone at real time), or fail to start.
type fakeSource struct {
	mu        sync.Mutex
	producers map[string]producer
	startErr  map[string]error
	starts    []string
	ctxs      map[string][]context.Context
}

func newFakeSource() *fakeSource {
	return &fakeSource{producers: map[string]producer{}, startErr: map[string]error{}, ctxs: map[string][]context.Context{}}
}

func (f *fakeSource) set(source string, p producer) *fakeSource {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.producers[source] = p
	return f
}

func (f *fakeSource) failStart(source string, err error) *fakeSource {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErr[source] = err
	return f
}

func (f *fakeSource) Start(ctx context.Context, source string) (Decoder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, source)
	f.ctxs[source] = append(f.ctxs[source], ctx)
	if err := f.startErr[source]; err != nil {
		return nil, err
	}
	p := f.producers[source]
	if p == nil {
		p = pacedTone(1000, 0)
	}
	return newFakeDecoder(ctx, p), nil
}

// started is how many decoders source has had.
func (f *fakeSource) started(source string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ctxs[source])
}

// total is how many decoders have started.
func (f *fakeSource) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

// decoderCtx is the context of source's first decoder, once it started.
func (f *fakeSource) decoderCtx(t *testing.T, source string) context.Context {
	t.Helper()
	eventually(t, "decoder "+source, func() bool { return f.started(source) > 0 })
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxs[source][0]
}

// toneFrame is frame n of a hz tone at streams.SampleRate.
func toneFrame(n, hz int) []byte {
	all := tone((n+1)*streams.FrameSamples, hz, streams.SampleRate)
	out := make([]byte, 0, streams.FrameBytes)
	for _, s := range all[n*streams.FrameSamples:] {
		out = binary.LittleEndian.AppendUint16(out, uint16(s))
	}
	return out
}

// numberedFrame is a frame whose first four bytes are n (big-endian), the
// rest a pattern that is never silence.
func numberedFrame(n int) []byte {
	f := make([]byte, streams.FrameBytes)
	for i := range f {
		f[i] = 0x55
	}
	binary.BigEndian.PutUint32(f, uint32(n))
	return f
}

// frameNumber is numberedFrame's n, or -1 for anything else.
func frameNumber(f []byte) int {
	if len(f) != streams.FrameBytes || f[4] != 0x55 {
		return -1
	}
	return int(binary.BigEndian.Uint32(f))
}

func writeFrame(ctx context.Context, w io.Writer, f []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err := w.Write(f)
	return err
}

// toneForever writes a hz tone as fast as it is read.
func toneForever(hz int) producer {
	frames := make([][]byte, 50) // one second, repeated (50 Hz multiples are whole)
	for i := range frames {
		frames[i] = toneFrame(i, hz)
	}
	return func(ctx context.Context, w io.Writer) error {
		for i := 0; ; i++ {
			if err := writeFrame(ctx, w, frames[i%len(frames)]); err != nil {
				return err
			}
		}
	}
}

// pacedTone writes a hz tone at real time, one frame each 20 ms, after
// waiting delay (a tuner locking).
func pacedTone(hz int, delay time.Duration) producer {
	return func(ctx context.Context, w io.Writer) error {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		tick := time.NewTicker(streams.FrameDuration)
		defer tick.Stop()
		for i := 0; ; i++ {
			if err := writeFrame(ctx, w, toneFrame(i%50, hz)); err != nil {
				return err
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// numbered writes numbered frames 0 to n-1 at once, then waits for ctx.
func numbered(n int) producer {
	return func(ctx context.Context, w io.Writer) error {
		for i := range n {
			if err := writeFrame(ctx, w, numberedFrame(i)); err != nil {
				return err
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

// silent never writes.
func silent() producer {
	return func(ctx context.Context, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
}

// failAfter writes n numbered frames, then ends with err.
func failAfter(n int, err error) producer {
	return func(ctx context.Context, w io.Writer) error {
		for i := range n {
			if werr := writeFrame(ctx, w, numberedFrame(i)); werr != nil {
				return werr
			}
		}
		return err
	}
}

// fed writes the frames sent on frames until ctx ends or frames is closed
// (then it ends with "the feed ended").
func fed(frames <-chan []byte) producer {
	return func(ctx context.Context, w io.Writer) error {
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					return errors.New("the feed ended")
				}
				if err := writeFrame(ctx, w, f); err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// eventually polls cond for up to 5 s of real time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	eventuallyWithin(t, 5*time.Second, what, cond)
}

// eventuallyWithin polls cond for up to d of real time.
func eventuallyWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// never checks that cond stays false for a while (100 ms of real time).
func never(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// done reports whether ctx has ended.
func done(ctx context.Context) bool { return ctx.Err() != nil }

// epoch is the fake clocks' start.
var epoch = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// Test sources and streams (as the house's, on test hosts).
const (
	srcKing  = "http://tuner.test:5004/auto/v5.1"
	srcKiro  = "http://tuner.test:5004/auto/v7.1"
	srcKomo  = "http://tuner.test:5004/auto/v4.1"
	srcRadio = "https://radio.test/groovesalad-128-mp3"
)

var (
	king5       = streams.Stream{Name: "king5", Number: "705", Source: srcKing, Pool: "hdhomerun"}
	king5Spare  = streams.Stream{Name: "king5-spare", Number: "715", Source: srcKing, Pool: "hdhomerun"}
	kiro7       = streams.Stream{Name: "kiro7", Number: "707", Source: srcKiro, Pool: "hdhomerun"}
	komo4       = streams.Stream{Name: "komo4", Number: "704", Source: srcKomo, Pool: "hdhomerun"}
	groovesalad = streams.Stream{Name: "groovesalad", Number: "750", Source: srcRadio}
	hdhomerun   = streams.Pool{Name: "hdhomerun", Max: 1}
)

// doc is a sealed streams.Document.
func doc(t *testing.T, pools []streams.Pool, list ...streams.Stream) streams.Document {
	t.Helper()
	d := streams.Document{Pools: pools, Streams: map[string]streams.Stream{}}
	for _, s := range list {
		d.Streams[streams.UUID(s.Name)] = s
	}
	sealed, err := d.Sealed()
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// testManager is a manager on src and clk (a fake clock at epoch if nil)
// with its own registry, the document loaded (unless it is the zero
// Document), stopped when the test ends.
func testManager(t *testing.T, src Source, clk clock.WithTicker, d streams.Document) (*manager, *prometheus.Registry) {
	t.Helper()
	if clk == nil {
		clk = clocktesting.NewFakeClock(epoch)
	}
	reg := prometheus.NewRegistry()
	m := newManager(src, clk, logr.Discard(), clk.Now())
	if err := m.register(reg); err != nil {
		t.Fatal(err)
	}
	if d.Version != "" {
		m.setDocument(d)
	}
	t.Cleanup(m.stopAll)
	return m, reg
}

// join joins a call on stream s, failing the test on a refusal.
func join(t *testing.T, m *manager, s streams.Stream) *caller {
	t.Helper()
	c, err := m.join(streams.UUID(s.Name))
	if err != nil {
		t.Fatalf("joining %s: %v", s.Name, err)
	}
	return c
}

// popN pops n frames from c's queue, waiting for each.
func popN(t *testing.T, c *caller, n int) [][]byte {
	t.Helper()
	var out [][]byte
	eventually(t, "frames", func() bool {
		for len(out) < n {
			f, ok := c.queue.pop()
			if !ok {
				return false
			}
			out = append(out, f)
		}
		return true
	})
	return out
}

// ended reports whether the bridge ended c's call.
func ended(c *caller) bool {
	select {
	case <-c.ended:
		return true
	default:
		return false
	}
}

// streamObs is the observations of the stream named name.
func streamObs(t *testing.T, m *manager, name string) api.StreamObservations {
	t.Helper()
	for _, s := range m.observations().Streams {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no observations of %s", name)
	return api.StreamObservations{}
}
