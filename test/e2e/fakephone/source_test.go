package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// sourceHarness is a running `fakephone source` on loopback.
type sourceHarness struct {
	tuner, radio string // http://127.0.0.1:port
	done         chan error
	stop         context.CancelFunc
}

func startSource(t *testing.T, o sourceOptions) *sourceHarness {
	t.Helper()
	if o.TunerListen == "" && o.RadioListen == "" {
		o.TunerListen, o.RadioListen = "127.0.0.1:0", "127.0.0.1:0"
	}
	if o.Tuners == 0 {
		o.Tuners = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &sourceHarness{done: make(chan error, 1), stop: cancel}
	started := make(chan struct{})
	o.Started = func(tuner, radio net.Addr) {
		if tuner != nil {
			h.tuner = "http://" + tuner.String()
		}
		if radio != nil {
			h.radio = "http://" + radio.String()
		}
		close(started)
	}
	go func() { h.done <- runSource(ctx, o) }()
	select {
	case <-started:
	case err := <-h.done:
		cancel()
		t.Fatalf("source did not start: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("source did not start within 5 s")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("source ended with %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("source did not stop within 10 s")
		}
	})
	return h
}

// stream is an answered GET of a stream path.
type stream struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       io.ReadCloser
}

// dialStream GETs a stream from any goroutine; the caller closes Body.
func dialStream(url string) (*stream, error) {
	res, err := http.Get(url) //nolint:bodyclose // the Body is returned, for the caller to close (openStream: at cleanup).
	if err != nil {
		return nil, err
	}
	return &stream{StatusCode: res.StatusCode, Status: res.Status, Header: res.Header, Body: res.Body}, nil
}

// openStream GETs a stream; its body is closed when the test ends (or
// earlier, by the test).
func openStream(t *testing.T, url string) *stream {
	t.Helper()
	s, err := dialStream(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Body.Close() })
	return s
}

// sourceStatus GETs a listener's status.
func sourceStatus(t *testing.T, base string) script.SourceStatus {
	t.Helper()
	st, err := tryStatus(base)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// tryStatus is sourceStatus for any goroutine.
func tryStatus(base string) (script.SourceStatus, error) {
	var st script.SourceStatus
	res, err := http.Get(base + script.SourcePathStatus)
	if err != nil {
		return st, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return st, fmt.Errorf("status: %s", res.Status)
	}
	return st, json.NewDecoder(res.Body).Decode(&st)
}

// occupy POSTs SourcePathOccupy and returns the answer's code and status.
func occupy(t *testing.T, base string, n string) (int, script.SourceStatus) {
	t.Helper()
	res, err := http.Post(base+script.SourcePathOccupy+"?"+script.SourceParamTuners+"="+n, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var st script.SourceStatus
	body, _ := io.ReadAll(res.Body)
	if res.Header.Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatalf("occupy: %v\n%s", err, body)
		}
	}
	return res.StatusCode, st
}

type wavFormat struct {
	riff, wave, fmtID, dataID   string
	riffSize, fmtSize, dataSize uint32
	format, channels, bits      uint16
	rate, byteRate              uint32
	blockAlign                  uint16
}

func readWAVHeader(t *testing.T, r io.Reader) wavFormat {
	t.Helper()
	var h [44]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		t.Fatalf("WAV header: %v", err)
	}
	le := binary.LittleEndian
	return wavFormat{
		riff: string(h[0:4]), riffSize: le.Uint32(h[4:8]), wave: string(h[8:12]),
		fmtID: string(h[12:16]), fmtSize: le.Uint32(h[16:20]), format: le.Uint16(h[20:22]),
		channels: le.Uint16(h[22:24]), rate: le.Uint32(h[24:28]), byteRate: le.Uint32(h[28:32]),
		blockAlign: le.Uint16(h[32:34]), bits: le.Uint16(h[34:36]),
		dataID: string(h[36:40]), dataSize: le.Uint32(h[40:44]),
	}
}

// readFrames reads n stereo frames and returns the left channel.
func readFrames(t *testing.T, r io.Reader, n int) []float64 {
	t.Helper()
	buf := make([]byte, n*sourceBytesPerFrame)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("reading %d frames: %v", n, err)
	}
	out := make([]float64, n)
	for i := range out {
		l := int16(binary.LittleEndian.Uint16(buf[i*sourceBytesPerFrame:]))
		rt := int16(binary.LittleEndian.Uint16(buf[i*sourceBytesPerFrame+2:]))
		if l != rt {
			t.Fatalf("frame %d: left %d, right %d: want the same tone on both", i, l, rt)
		}
		out[i] = float64(l)
	}
	return out
}

// TestSourceWAV: a stream is a WAV header (PCM, 48 kHz, stereo, 16-bit, the
// largest data size), then the tone at ?hz= on both channels, its phase
// continuous across the 20 ms writes.
func TestSourceWAV(t *testing.T) {
	h := startSource(t, sourceOptions{})
	res := openStream(t, h.radio+"/radio-a?"+script.SourceParamHz+"=510")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("GET: %s, %s", res.Status, res.Header.Get("Content-Type"))
	}
	r := bufio.NewReader(res.Body)
	got := readWAVHeader(t, r)
	want := wavFormat{
		riff: "RIFF", wave: "WAVE", fmtID: "fmt ", dataID: "data",
		riffSize: 36 + wavDataSize, fmtSize: 16, dataSize: wavDataSize,
		format: 1, channels: script.SourceChannels, bits: 16,
		rate: script.SourceSampleRate, byteRate: script.SourceSampleRate * script.SourceChannels * 2,
		blockAlign: script.SourceChannels * 2,
	}
	if got != want {
		t.Errorf("WAV header\n  %+v\nwant\n  %+v", got, want)
	}
	if got.dataSize < math.MaxUint32-64 || got.dataSize%uint32(got.blockAlign) != 0 {
		t.Errorf("data size %d: want the largest whole number of frames", got.dataSize)
	}

	// 0.5 s: 25 writes, so 24 boundaries between them. 510 Hz is not a
	// whole number of cycles in 20 ms (the e2e tones, multiples of 50 Hz,
	// are), so a write that restarted the phase would show.
	samples := readFrames(t, r, script.SourceSampleRate/2)
	if a := analyseAt(samples, 510, script.SourceSampleRate); a.Ratio < 0.95 {
		t.Errorf("510 Hz carries %.3f of the energy", a.Ratio)
	}
	if a := analyseAt(samples, 1000, script.SourceSampleRate); a.Ratio > 0.05 {
		t.Errorf("1000 Hz carries %.3f of a 510 Hz stream's energy", a.Ratio)
	}
	// No step between two samples is larger than the sine's own steepest,
	// so no write boundary jumps in phase.
	peak := sourceAmplitude * math.MaxInt16
	maxStep := 2 * math.Pi * 510 / script.SourceSampleRate * peak * 1.05
	for i := 1; i < len(samples); i++ {
		if d := math.Abs(samples[i] - samples[i-1]); d > maxStep+1 {
			t.Fatalf("frame %d steps by %.0f (at most %.0f for 510 Hz): the phase jumped", i, d, maxStep)
		}
	}
	var rms float64
	for _, s := range samples {
		rms += s * s
	}
	if rms = math.Sqrt(rms / float64(len(samples))); math.Abs(rms-peak/math.Sqrt2) > peak*0.02 {
		t.Errorf("RMS %.0f, want a sine of peak %.0f", rms, peak)
	}

	// Without ?hz= the tone is DefaultSourceHz.
	def := bufio.NewReader(openStream(t, h.radio+"/radio-b").Body)
	readWAVHeader(t, def)
	if a := analyseAt(readFrames(t, def, 4800), script.DefaultSourceHz, script.SourceSampleRate); a.Ratio < 0.95 {
		t.Errorf("the default tone: %.0f Hz carries %.3f", float64(script.DefaultSourceHz), a.Ratio)
	}
}

// TestSourceRealTime: a stream arrives at real time, 1 s of audio per
// second (± 10%), not as fast as the client reads.
func TestSourceRealTime(t *testing.T) {
	h := startSource(t, sourceOptions{})
	for _, base := range []string{h.radio, h.tuner} {
		r := bufio.NewReader(openStream(t, base+"/x").Body)
		readWAVHeader(t, r)
		start := time.Now()
		var n int
		buf := make([]byte, 4096)
		for time.Since(start) < 2*time.Second {
			k, err := r.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			n += k
		}
		elapsed := time.Since(start)
		perSecond := float64(n) / elapsed.Seconds()
		want := float64(script.SourceSampleRate * sourceBytesPerFrame)
		if math.Abs(perSecond-want) > want/10 {
			t.Errorf("%s: %.0f bytes a second over %s, want %.0f ± 10%%", base, perSecond, elapsed.Round(time.Millisecond), want)
		}
	}
}

// TestSourceTuners: the tuner listener serves --tuners streams at once; the
// next request is 503 with the HDHomeRun's header and counts as Refused;
// a client that goes frees its tuner.
func TestSourceTuners(t *testing.T) {
	h := startSource(t, sourceOptions{Tuners: 2})
	a := openStream(t, h.tuner+"/auto/v5.1?hz=1100")
	b := openStream(t, h.tuner+"/auto/v7.1")
	for _, res := range []*stream{a, b} {
		if res.StatusCode != http.StatusOK {
			t.Fatalf("one of two tuners: %s", res.Status)
		}
	}
	third, err := http.Get(h.tuner + "/auto/v13.1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(third.Body)
	_ = third.Body.Close()
	if third.StatusCode != http.StatusServiceUnavailable || third.Header.Get(script.HDHomeRunErrorHeader) != script.HDHomeRunAllTunersInUse {
		t.Errorf("a third stream: %s, %s: %q, want 503 and %s: %s", third.Status, script.HDHomeRunErrorHeader,
			third.Header.Get(script.HDHomeRunErrorHeader), script.HDHomeRunErrorHeader, script.HDHomeRunAllTunersInUse)
	}
	if !strings.Contains(string(body), "All Tuners In Use") {
		t.Errorf("the 503's body: %q", body)
	}
	st := sourceStatus(t, h.tuner)
	if st.Tuners != 2 || st.OpenTotal() != 2 || st.Open["/auto/v5.1"] != 1 || st.Open["/auto/v7.1"] != 1 || st.Refused != 1 {
		t.Errorf("status %+v", st)
	}

	_ = a.Body.Close()
	waitFor(t, 3*time.Second, "the closed stream's tuner to be free", func() bool {
		return sourceStatus(t, h.tuner).OpenTotal() == 1
	})
	again := openStream(t, h.tuner+"/auto/v13.1")
	if again.StatusCode != http.StatusOK {
		t.Errorf("after a close: %s", again.Status)
	}
	st = sourceStatus(t, h.tuner)
	if _, ok := st.Open["/auto/v5.1"]; ok || st.Opened["/auto/v5.1"] != 1 || st.Opened["/auto/v13.1"] != 1 {
		t.Errorf("after a close: %+v", st)
	}
}

// TestSourceOccupy: POST /fake/occupy holds tuners as a DVR does (0 frees
// them), and refuses more than are free; only POST, only numbers from 0 to
// --tuners, only on the tuner listener.
func TestSourceOccupy(t *testing.T) {
	h := startSource(t, sourceOptions{Tuners: 2})
	code, st := occupy(t, h.tuner, "2")
	if code != http.StatusOK || st.Occupied != 2 || st.Tuners != 2 {
		t.Fatalf("occupy 2: %d %+v", code, st)
	}
	refused, err := http.Get(h.tuner + "/auto/v5.1")
	if err != nil {
		t.Fatal(err)
	}
	_ = refused.Body.Close()
	if refused.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("with both tuners occupied: %s", refused.Status)
	}
	if code, st = occupy(t, h.tuner, "0"); code != http.StatusOK || st.Occupied != 0 || st.Refused != 1 {
		t.Errorf("occupy 0: %d %+v", code, st)
	}
	if res := openStream(t, h.tuner+"/auto/v5.1"); res.StatusCode != http.StatusOK {
		t.Errorf("after freeing: %s", res.Status)
	}
	// One tuner is the stream's: a DVR can take only the other.
	if code, st = occupy(t, h.tuner, "2"); code != http.StatusConflict || st.Occupied != 0 || st.OpenTotal() != 1 {
		t.Errorf("occupy 2 with a stream open: %d %+v, want 409", code, st)
	}
	if code, st = occupy(t, h.tuner, "1"); code != http.StatusOK || st.Occupied != 1 {
		t.Errorf("occupy 1 with a stream open: %d %+v", code, st)
	}
	for _, n := range []string{"3", "-1", "two", ""} {
		if code, _ := occupy(t, h.tuner, n); code != http.StatusBadRequest {
			t.Errorf("occupy %q: %d, want 400", n, code)
		}
	}
	res, err := http.Get(h.tuner + script.SourcePathOccupy + "?tuners=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET occupy: %s", res.Status)
	}
	if code, _ := occupy(t, h.radio, "1"); code != http.StatusNotFound {
		t.Errorf("occupy on the radio: %d, want 404", code)
	}
}

// TestSourceLockDelay: the tuner's first audio comes --lock-delay after
// the request (the header at once, the tuner held meanwhile); the radio's
// at once.
func TestSourceLockDelay(t *testing.T) {
	const delay = 400 * time.Millisecond
	h := startSource(t, sourceOptions{Tuners: 1, LockDelay: delay})
	firstByte := func(url string) (header, audio time.Duration) {
		t.Helper()
		start := time.Now()
		res := openStream(t, url)
		header = time.Since(start)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", url, res.Status)
		}
		var b [1]byte
		if _, err := io.ReadFull(res.Body, b[:]); err != nil {
			t.Fatal(err)
		}
		return header, time.Since(start)
	}
	done := make(chan struct{})
	var held bool
	go func() {
		defer close(done)
		time.Sleep(delay / 2)
		st, err := tryStatus(h.tuner)
		held = err == nil && st.OpenTotal() == 1
	}()
	header, audio := firstByte(h.tuner + "/auto/v5.1")
	<-done
	if header > delay/2 || audio < delay || audio > delay+500*time.Millisecond {
		t.Errorf("tuner: header after %s, audio after %s; want the header at once and audio after %s", header, audio, delay)
	}
	if !held {
		t.Error("the tuner was not held while it locked")
	}
	if _, audio := firstByte(h.radio + "/radio-a"); audio > 200*time.Millisecond {
		t.Errorf("radio: audio after %s, want at once", audio)
	}
}

// TestSourceStatus: each listener's status, as the e2e harness reads it:
// the tuner's --tuners, the radio's 0, and open and opened streams by path
// without the query, never null.
func TestSourceStatus(t *testing.T) {
	h := startSource(t, sourceOptions{Tuners: 2})
	for base, tuners := range map[string]int{h.tuner: 2, h.radio: 0} {
		res, err := http.Get(base + script.SourcePathStatus)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: %s %s", base, res.Status, res.Header.Get("Content-Type"))
		}
		if !bytes.Contains(raw, []byte(`"open":{}`)) || !bytes.Contains(raw, []byte(`"opened":{}`)) ||
			!bytes.Contains(raw, []byte(fmt.Sprintf(`"tuners":%d`, tuners))) {
			t.Errorf("%s: a fresh status is %s", base, raw)
		}
	}
	r := openStream(t, h.radio+"/radio-a?hz=700")
	if r.StatusCode != http.StatusOK {
		t.Fatal(r.Status)
	}
	st := sourceStatus(t, h.radio)
	if st.Open["/radio-a"] != 1 || st.Opened["/radio-a"] != 1 || len(st.Open) != 1 {
		t.Errorf("radio status %+v", st)
	}
	if res, err := http.Post(h.radio+script.SourcePathStatus, "", nil); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status: %v %v", res, err)
	} else {
		_ = res.Body.Close()
	}
	if res, err := http.Post(h.radio+"/radio-a", "", nil); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST a stream: %v %v", res, err)
	} else {
		_ = res.Body.Close()
	}
}

// TestRadioUnlimited: the radio listener has no limit and no refusal.
func TestRadioUnlimited(t *testing.T) {
	h := startSource(t, sourceOptions{Tuners: 1})
	const n = 10
	var wg sync.WaitGroup
	results := make(chan *stream, n)
	for i := range n {
		wg.Go(func() {
			res, err := dialStream(h.radio + "/radio-" + strconv.Itoa(i%3))
			if err != nil {
				t.Errorf("a radio stream: %v", err)
				return
			}
			results <- res
		})
	}
	wg.Wait()
	close(results)
	for res := range results {
		t.Cleanup(func() { _ = res.Body.Close() })
		if res.StatusCode != http.StatusOK {
			t.Errorf("a radio stream: %s", res.Status)
		}
	}
	st := sourceStatus(t, h.radio)
	if st.OpenTotal() != n || st.Refused != 0 || st.Tuners != 0 {
		t.Errorf("radio status %+v, want %d open and none refused", st, n)
	}
}

// TestSourceBadHz: a tone the WAV cannot carry is refused (400), and no
// tuner is taken for it.
func TestSourceBadHz(t *testing.T) {
	h := startSource(t, sourceOptions{Tuners: 1})
	for _, hz := range []string{"abc", "0", "-5", "24000", "1e9", "NaN"} {
		res, err := http.Get(h.tuner + "/auto/v5.1?hz=" + hz)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("hz=%s: %s, want 400", hz, res.Status)
		}
	}
	if st := sourceStatus(t, h.tuner); st.OpenTotal() != 0 || st.Refused != 0 || len(st.Opened) != 0 {
		t.Errorf("after bad requests: %+v", st)
	}
}

// TestSourceStops: when the source stops, its open streams end and
// runSource returns nil.
func TestSourceStops(t *testing.T) {
	h := startSource(t, sourceOptions{})
	res := openStream(t, h.radio+"/radio-a")
	h.stop()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("runSource: %v", err)
		}
		h.done <- nil // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("runSource did not return")
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Logf("the stream ended with %v", err)
	}
}

// TestSourceCommand: `fakephone source` with its flags serves both
// listeners until its context ends; bad flags fail at once.
func TestSourceCommand(t *testing.T) {
	tunerPort, radioPort := freePort(t), freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	root := newRootCmd()
	root.SetArgs([]string{"source", "--tuner-listen", "127.0.0.1:" + strconv.Itoa(tunerPort), "--tuners", "3",
		"--lock-delay", "0s", "--radio-listen", "127.0.0.1:" + strconv.Itoa(radioPort)})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	tuner := "http://127.0.0.1:" + strconv.Itoa(tunerPort)
	waitFor(t, 5*time.Second, "the source to listen", func() bool {
		res, err := http.Get(tuner + script.SourcePathStatus)
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return true
	})
	if st := sourceStatus(t, tuner); st.Tuners != 3 {
		t.Errorf("--tuners 3: status %+v", st)
	}
	if st := sourceStatus(t, "http://127.0.0.1:"+strconv.Itoa(radioPort)); st.Tuners != 0 {
		t.Errorf("radio status %+v", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("source: %v", err)
	}

	for _, args := range [][]string{
		{"--tuners", "-1"},
		{"--lock-delay", "-1s"},
		{"--tuner-listen", "", "--radio-listen", ""},
		{"--tuner-listen", "127.0.0.1:" + strconv.Itoa(radioPort), "--radio-listen", "127.0.0.1:" + strconv.Itoa(radioPort)},
	} {
		root := newRootCmd()
		root.SetArgs(append([]string{"source"}, args...))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := root.ExecuteContext(ctx)
		cancel()
		if err == nil {
			t.Errorf("source %v ran", args)
		}
	}
}
