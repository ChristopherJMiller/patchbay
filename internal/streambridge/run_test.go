package streambridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// running is a bridge under test.
type running struct {
	audio, control string
	stop           func() // cancels Run and checks it returned nil
}

// startRun runs the bridge with o (both listeners on loopback port 0 unless
// set) until the test ends.
func startRun(t *testing.T, o Options) *running {
	t.Helper()
	if o.Listen == "" {
		o.Listen = "127.0.0.1:0"
	}
	if o.ControlListen == "" {
		o.ControlListen = "127.0.0.1:0"
	}
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [2]string, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, o, func(a, c net.Addr) { addrs <- [2]string{a.String(), c.String()} })
	}()
	r := &running{}
	select {
	case a := <-addrs:
		r.audio, r.control = a[0], a[1]
	case err := <-errc:
		cancel()
		t.Fatalf("Run: %v", err)
	}
	var stopped bool
	r.stop = func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("Run returned %v after its context ended", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return within 10 s of its context ending")
		}
	}
	t.Cleanup(r.stop)
	return r
}

// observe is GET observations from the running bridge.
func (r *running) observe(t *testing.T) api.Observations {
	t.Helper()
	resp, err := http.Get("http://" + r.control + api.PathObservations)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var o api.Observations
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		t.Fatal(err)
	}
	return o
}

// ready is GET readyz's status and text.
func (r *running) ready(t *testing.T) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + r.control + api.PathReadyz)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// streamsJSON writes d as a streams.json in a new directory.
func streamsJSON(t *testing.T, d streams.Document) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), streams.FileName)
	if err := os.WriteFile(path, encode(t, d), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunEndToEnd(t *testing.T) {
	src := newFakeSource()
	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})
	r := startRun(t, Options{StreamsFile: streamsJSON(t, doc(t, []streams.Pool{hdhomerun}, king5)), Source: src, Log: log})
	if code, why := r.ready(t); code != http.StatusOK {
		t.Fatalf("readyz %d %q", code, why)
	}
	a := call(t, r.audio, "king5")
	start := time.Now()
	frames := a.audio(50)
	if took := time.Since(start); took < 900*time.Millisecond || took > 1100*time.Millisecond {
		t.Errorf("50 frames took %s, want 1 s (paced)", took)
	}
	var heard []int16
	for _, f := range frames[5:] {
		heard = append(heard, samples(f)...)
	}
	if ratio := toneRatio(heard, 1000); ratio < 0.8 {
		t.Errorf("the caller hears %.2f of the tone", ratio)
	}
	o := r.observe(t)
	if o.Bridge.Calls != 1 || len(o.Streams) != 1 || o.Streams[0].Listeners != 1 || o.Streams[0].Session == nil ||
		o.Streams[0].Session.State != api.SessionPlaying {
		t.Fatalf("during the call: %+v %+v", o.Bridge, o.Streams)
	}
	ctx := src.decoderCtx(t, srcKing)
	_ = a.conn.Close()
	left := time.Now()
	eventuallyWithin(t, 2*Grace, "the decoder to stop", func() bool { return done(ctx) })
	if after := time.Since(left); after < Grace-500*time.Millisecond || after > Grace+time.Second {
		t.Errorf("the decoder stopped %s after the call, want Grace (%s)", after, Grace)
	}
	r.stop()
	mu.Lock()
	defer mu.Unlock()
	var sawCall bool
	for _, l := range lines {
		if strings.Contains(l, `"call ended"`) && strings.Contains(l, `"stream"="king5"`) && strings.Contains(l, `"number"="705"`) &&
			strings.Contains(l, `"ended"="the caller hung up"`) {
			sawCall = true
		}
	}
	if !sawCall {
		t.Errorf("no call line in the log:\n%s", strings.Join(lines, "\n"))
	}
}

func TestRunStopsCalls(t *testing.T) {
	src := newFakeSource()
	r := startRun(t, Options{StreamsFile: streamsJSON(t, doc(t, nil, groovesalad)), Source: src, Log: logr.Discard()})
	a := call(t, r.audio, "groovesalad")
	a.audio(2)
	ctx := src.decoderCtx(t, srcRadio)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.stop()
	}()
	a.hungUp()
	<-stopped
	if !done(ctx) {
		t.Error("a decoder runs after Run returned")
	}
}

func TestRunFFmpeg(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	srv := httptest.NewServer(realTime(wav(30, 1000, 48000, 2), 48000*2*2, "audio/wav"))
	defer srv.Close()
	radio := streams.Stream{Name: "fake-radio", Number: "751", Source: srv.URL + "/radio-a"}
	r := startRun(t, Options{StreamsFile: streamsJSON(t, doc(t, nil, radio)), FFmpeg: ffmpeg, Log: logr.Discard()})
	if code, why := r.ready(t); code != http.StatusOK {
		t.Fatalf("readyz %d %q", code, why)
	}
	if v := r.observe(t).Bridge.FFmpeg; !strings.HasPrefix(v, "ffmpeg version") {
		t.Errorf("BridgeStatus.FFmpeg %q", v)
	}
	a := call(t, r.audio, "fake-radio")
	for range 300 { // 6 s: ffmpeg's probe of this WAV takes about 2.8 s
		f := a.audio(1)[0]
		if toneRatio(samples(f), 1000) > 0.8 {
			return
		}
	}
	t.Error("no tone within 6 s")
}

// versionedSource is a fake source that says which decoder it runs.
type versionedSource struct {
	*fakeSource
	errs []error // Version's results in turn; then success
}

func (v *versionedSource) Version(context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.errs) > 0 {
		err := v.errs[0]
		v.errs = v.errs[1:]
		return "", err
	}
	return "ffmpeg version 8.1.2 Copyright (c) 2000-2026 the FFmpeg developers", nil
}

func TestRunReadiness(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	dir := t.TempDir()
	path := filepath.Join(dir, streams.FileName)
	src := &versionedSource{fakeSource: newFakeSource(), errs: []error{errors.New("ffmpeg -version: exec: \"ffmpeg\": not found")}}
	r := startRun(t, Options{StreamsFile: path, Source: src, Clock: clk, Log: logr.Discard()})
	if code, why := r.ready(t); code != http.StatusServiceUnavailable || !strings.Contains(why, "does not exist") {
		t.Errorf("no streams.json: %d %q", code, why)
	}
	if err := os.WriteFile(path, encode(t, doc(t, nil, groovesalad)), 0o644); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the poll timer", clk.HasWaiters)
	clk.Step(StreamsPollInterval)
	eventually(t, "the file to load", func() bool { return r.observe(t).Bridge.StreamsVersion != "" })
	eventually(t, "ffmpeg's version", func() bool { return r.observe(t).Bridge.FFmpeg != "" })
	if code, why := r.ready(t); code != http.StatusOK {
		t.Errorf("loaded, ffmpeg found: %d %q", code, why)
	}
}

func TestRunOptions(t *testing.T) {
	o, err := Options{Source: newFakeSource()}.complete()
	if err != nil {
		t.Fatal(err)
	}
	if o.Listen != DefaultListen || o.ControlListen != DefaultControlListen || o.StreamsFile != DefaultStreamsFile ||
		o.FFmpeg != DefaultFFmpeg || o.Clock == nil || o.Registerer == nil || o.Gatherer == nil {
		t.Errorf("defaults: %+v", o)
	}
	o, err = Options{FFmpeg: "/opt/ffmpeg", CAFile: "/ca.pem"}.complete()
	if f, ok := o.Source.(*FFmpegSource); err != nil || !ok || f.path != "/opt/ffmpeg" || f.caFile != "/ca.pem" {
		t.Errorf("a nil Source: %#v, %v", o.Source, err)
	}
	reg := prometheus.NewRegistry()
	if o, err := (Options{Registerer: reg}).complete(); err != nil || o.Gatherer != reg {
		t.Errorf("a Registry as the Registerer: %v %v", o.Gatherer, err)
	}
	for name, bad := range map[string]Options{
		"no port":                         {Listen: "127.0.0.1"},
		"a port too large":                {ControlListen: ":99999"},
		"a named port":                    {Listen: ":audiosocket"},
		"the same address":                {Listen: ":9000", ControlListen: ":9000"},
		"a Gatherer alone":                {Gatherer: reg},
		"a Registerer that cannot gather": {Registerer: prometheus.WrapRegistererWith(prometheus.Labels{"a": "b"}, reg)},
	} {
		if err := Run(context.Background(), bad); err == nil {
			t.Errorf("%s: Run started", name)
		}
	}
}

func TestRunListenFails(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	for _, o := range []Options{
		{Listen: taken.Addr().String(), ControlListen: "127.0.0.1:0"},
		{Listen: "127.0.0.1:0", ControlListen: taken.Addr().String()},
	} {
		o.Source, o.Log = newFakeSource(), logr.Discard()
		err := Run(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), taken.Addr().String()) {
			t.Errorf("%v, want an error naming %s", err, taken.Addr())
		}
	}
	reg := prometheus.NewRegistry()
	startRun(t, Options{Source: newFakeSource(), Registerer: reg, Log: logr.Discard()})
	err = Run(context.Background(), Options{Listen: "127.0.0.1:0", ControlListen: "127.0.0.1:0", Source: newFakeSource(), Registerer: reg})
	if err == nil || !strings.Contains(err.Error(), "metrics") {
		t.Errorf("a second bridge on one registry: %v", err)
	}
}
