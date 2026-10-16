package streambridge

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// The stderr tails below are ffmpeg 9.0.1's (the dev shell's), as it wrote
// them on 2026-10-16 with FFmpegArgs against local test servers.
func TestFFmpegReason(t *testing.T) {
	for _, c := range []struct{ name, stderr, want string }{
		{"nothing", "", ""},
		{"503", "[in#0 @ 0x55555560bdc0] Error opening input: Server returned 5XX Server Error reply\n" +
			"Error opening input file http://127.0.0.1:33283/503.\n" +
			"Error opening input files: Server returned 5XX Server Error reply\n",
			"Server returned 5XX Server Error reply"},
		{"404", "[in#0 @ 0x55555560bdc0] Error opening input: Server returned 404 Not Found\n" +
			"Error opening input file http://127.0.0.1:33283/404.\nError opening input files: Server returned 404 Not Found\n",
			"Server returned 404 Not Found"},
		{"refused", "[tcp @ 0x55555560f640] Connection to tcp://127.0.0.1:1 failed: Connection refused\n" +
			"[in#0 @ 0x55555560bdc0] Error opening input: Connection refused\n" +
			"Error opening input file http://127.0.0.1:1/x.\nError opening input files: Connection refused\n",
			"Connection to tcp://127.0.0.1:1 failed: Connection refused"},
		{"dns", "[tcp @ 0x55555560f640] Failed to resolve hostname nonexistent.invalid: Name or service not known\n" +
			"[in#0 @ 0x55555560bdc0] Error opening input: Input/output error\n" +
			"Error opening input file http://nonexistent.invalid/x.\nError opening input files: Input/output error\n",
			"Failed to resolve hostname nonexistent.invalid: Name or service not known"},
		{"certificate", "[tls @ 0x55555560f700] Peer certificate failed verification\n" +
			"[in#0 @ 0x55555560bdc0] Error opening input: Input/output error\n" +
			"Error opening input file https://127.0.0.1:43135/mp3.\nError opening input files: Input/output error\n",
			"Peer certificate failed verification"},
		{"file redirect", "[http @ 0x55555560cac0] Protocol 'file' not on whitelist 'http,https,tcp,tls,crypto'!\n" +
			"[in#0 @ 0x55555560bdc0] Error opening input: Invalid argument\n" +
			"Error opening input file http://127.0.0.1:33283/redir.\nError opening input files: Invalid argument\n",
			"Protocol 'file' not on whitelist 'http,https,tcp,tls,crypto'!"},
		{"unlisted format", "[in#0 @ 0x55555560c0c0] Format not on whitelist 'mpegts,mp3,aac,ogg,flac,wav,hls,mov'\n" +
			"[in#0 @ 0x55555560bdc0] Error opening input: Invalid argument\n" +
			"Error opening input file http://127.0.0.1:34335/avi.\nError opening input files: Invalid argument\n",
			"the source is not an audio stream the bridge reads (Format not on whitelist 'mpegts,mp3,aac,ogg,flac,wav,hls,mov')"},
		{"a long web page", "[in#0 @ 0x55555560bdc0] Error opening input: Invalid data found when processing input\n" +
			"Error opening input file http://127.0.0.1:37215/html2.\nError opening input files: Invalid data found when processing input\n",
			"the source is not an audio stream the bridge reads (Invalid data found when processing input)"},
		{"a short web page", "[in#0 @ 0x55555560bdc0] Error opening input: Input/output error\n" +
			"Error opening input file http://127.0.0.1:37215/html.\nError opening input files: Input/output error\n",
			"the source is not an audio stream the bridge reads (Input/output error)"},
		{"an empty mp3", "[in#0 @ 0x55555560c0c0] Failed to find two consecutive MPEG audio frames.\n" +
			"[in#0 @ 0x55555560bdc0] Error opening input: Invalid data found when processing input\n" +
			"Error opening input file http://127.0.0.1:33283/empty.\nError opening input files: Invalid data found when processing input\n",
			"the source is not an audio stream the bridge reads (Failed to find two consecutive MPEG audio frames.)"},
		{"a stall while playing", "[in#0/mpegts @ 0x55555560bdc0] Error during demuxing: Connection timed out\n",
			"Error during demuxing: Connection timed out"},
		{"the last line wins while playing", "[ac3 @ 0x5555] incomplete frame\n[mp3 @ 0x5555] Header missing\r\n\n",
			"Header missing"},
		{"control characters", "[http @ 0x1] HTTP \x1b[31merror\x07 \t here\n", "HTTP [31merror here"},
		{"a cut first line", "ne cut off\n[tcp @ 0x2] Connection reset by peer\n", "Connection reset by peer"},
	} {
		if got := ffmpegReason(c.stderr); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	long := strings.Repeat("x", 3000)
	if got := ffmpegReason("[tcp @ 0x1] " + long + "\n"); len(got) > api.MaxMessageBytes || !strings.HasSuffix(got, ellipsis) {
		t.Errorf("a long line: %d bytes", len(got))
	}
}

func TestExitMessage(t *testing.T) {
	for _, c := range []struct {
		code   int
		signal string
		reason string
		want   string
	}{
		{8, "", "Server returned 5XX Server Error reply", "ffmpeg exited with status 8: Server returned 5XX Server Error reply"},
		{0, "", "", "ffmpeg exited with status 0: the source ended"},
		{0, "", "Error during demuxing: Connection timed out", "ffmpeg exited with status 0: Error during demuxing: Connection timed out"},
		{-1, "killed", "", "ffmpeg was killed by killed"},
		{-1, "segmentation fault", "x", "ffmpeg was killed by segmentation fault: x"},
	} {
		if got := exitMessage(c.code, c.signal, c.reason); got != c.want {
			t.Errorf("exitMessage(%d, %q, %q) = %q, want %q", c.code, c.signal, c.reason, got, c.want)
		}
	}
	if got := exitMessage(1, "", strings.Repeat("y", 2000)); len(got) > api.MaxMessageBytes {
		t.Errorf("%d bytes", len(got))
	}
}

func TestTailBuffer(t *testing.T) {
	var b tailBuffer
	_, _ = b.Write([]byte(strings.Repeat("a", stderrTail)))
	_, _ = b.Write([]byte("bcd"))
	got := b.String()
	if len(got) != stderrTail || !strings.HasSuffix(got, "abcd") || strings.Count(got, "a") != stderrTail-3 {
		t.Errorf("%d bytes, tail %q", len(got), got[len(got)-8:])
	}
	_, _ = b.Write([]byte(strings.Repeat("z", 2*stderrTail)))
	if got := b.String(); got != strings.Repeat("z", stderrTail) {
		t.Errorf("after a large write: %d bytes", len(got))
	}
}

// startFFmpeg starts the dev shell's ffmpeg on src.
func startFFmpeg(t *testing.T, ctx context.Context, src string) *ffmpegDecoder {
	t.Helper()
	dec, err := NewFFmpegSource(needFFmpeg(t), "", logr.Discard()).Start(ctx, src)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return dec.(*ffmpegDecoder)
}

// waitDecoder is the decoder's Wait, within d.
func waitDecoder(t *testing.T, dec Decoder, d time.Duration) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { _, _ = io.Copy(io.Discard, dec); errc <- dec.Wait() }()
	select {
	case err := <-errc:
		return err
	case <-time.After(d):
		t.Fatalf("Wait did not return within %s", d)
		return nil
	}
}

// TestFFmpegDecodesWAV: the e2e fake source's format (48 kHz stereo WAV,
// at real time) comes out 16 kHz mono at 32 000 bytes a second. ffmpeg
// first reads the WAV for its probe (FFmpegArgs' 1 MB: about 2.8 s of this
// WAV with ffmpeg 9.0.1), then writes what it holds at once and follows
// the source at real time, so the rate is measured from 1 s after the
// first output, while the source still sends.
func TestFFmpegDecodesWAV(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	const seconds = 9
	srv := httptest.NewServer(realTime(wav(seconds, 1000, 48000, 2), 48000*2*2, "audio/wav"))
	defer srv.Close()
	start := time.Now()
	dec, err := NewFFmpegSource(ffmpeg, "", logr.Discard()).Start(t.Context(), srv.URL+"/radio")
	if err != nil {
		t.Fatal(err)
	}
	var pcm []byte
	var first time.Time
	window := 0
	buf := make([]byte, streams.FrameBytes)
	for {
		n, err := io.ReadFull(dec, buf)
		if n > 0 && first.IsZero() {
			first = time.Now()
			t.Logf("the first PCM came %s after Start", first.Sub(start).Round(time.Millisecond))
		}
		if since := time.Since(first); since >= time.Second && since < 4*time.Second {
			window += n
		}
		pcm = append(pcm, buf[:n]...)
		if err != nil {
			break
		}
	}
	if err := dec.Wait(); err == nil || !strings.Contains(err.Error(), "status 0") {
		t.Errorf("Wait after the source ended: %v, want an exit with status 0", err)
	}
	if rate := window / 3; rate < 28800 || rate > 35200 {
		t.Errorf("%d bytes a second from 1 s to 4 s after the first, want 32000 ± 10%% (16 kHz mono)", rate)
	}
	if total := len(pcm); total < seconds*32000*9/10 || total > seconds*32000*11/10 {
		t.Errorf("%d bytes in all, want about %d", total, seconds*32000)
	}
	if r := toneRatio(samples(pcm[3200:]), 1000); r < 0.8 {
		t.Errorf("1 kHz carries %.2f of the power, want >= 0.8", r)
	}
}

// TestFFmpegDecodesMPEGTS: a tuner's MPEG-TS (MPEG-2 video, AC-3 stereo)
// at real time. ffmpeg 9.0.1 writes its first PCM about 1.1-1.2 s in
// (FFmpegArgs' 1 s analyzeduration bounds the probe, at this file's
// 84 KB/s and at a broadcast 1 MB/s alike), so the bound here is 1.5 s.
func TestFFmpegDecodesMPEGTS(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	const seconds = 5
	ts := mpegTS(t, ffmpeg, seconds, 1000)
	srv := httptest.NewServer(realTime(ts, len(ts)/seconds, "video/mp2t"))
	defer srv.Close()
	start := time.Now()
	dec, err := NewFFmpegSource(ffmpeg, "", logr.Discard()).Start(t.Context(), srv.URL+"/auto/v5.1")
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, streams.FrameBytes)
	if _, err := io.ReadFull(dec, frame); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("the first PCM frame came %s after Start", took.Round(time.Millisecond))
	if took > 1500*time.Millisecond {
		t.Errorf("the first PCM frame came after %s, want under 1.5 s", took)
	}
	rest, _ := io.ReadAll(dec)
	_ = dec.Wait()
	if r := toneRatio(samples(rest), 1000); r < 0.8 {
		t.Errorf("1 kHz carries %.2f of the AC-3 track's power, want >= 0.8", r)
	}
}

func TestFFmpeg503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-HDHomeRun-Error", "805 All Tuners In Use")
		http.Error(w, "805 All Tuners In Use", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	dec := startFFmpeg(t, t.Context(), srv.URL+"/auto/v5.1")
	err := waitDecoder(t, dec, 3*time.Second)
	// ffmpeg at -loglevel error names the class (5XX), not the code: it
	// logs "HTTP error 503" only at warning (FFmpegArgs is a contract).
	if err == nil || !strings.Contains(err.Error(), "Server returned 5XX Server Error reply") || !strings.HasPrefix(err.Error(), "ffmpeg exited with status ") {
		t.Errorf("Wait = %v, want ffmpeg's 5XX line", err)
	}
}

func TestFFmpegHTMLPage(t *testing.T) {
	for name, size := range map[string]int{"short": 20 << 10, "long": 2 << 20} {
		t.Run(name, func(t *testing.T) {
			page := "<!doctype html><html><head><title>Groove Salad</title></head><body>" + strings.Repeat("<p>listen</p>", size/13) + "</body></html>"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, page)
			}))
			defer srv.Close()
			err := waitDecoder(t, startFFmpeg(t, t.Context(), srv.URL+"/groovesalad"), 10*time.Second)
			if err == nil || !strings.Contains(err.Error(), "the source is not an audio stream the bridge reads") {
				t.Errorf("Wait = %v", err)
			}
		})
	}
}

func TestFFmpegRefusesRedirectToFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer srv.Close()
	err := waitDecoder(t, startFFmpeg(t, t.Context(), srv.URL+"/x"), 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "Protocol 'file' not on whitelist") {
		t.Errorf("Wait = %v", err)
	}
}

func TestFFmpegHTTPS(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	srv := httptest.NewTLSServer(realTime(wav(30, 1000, 16000, 1), 32000, "audio/wav"))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("trusted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		dec, err := NewFFmpegSource(ffmpeg, ca, logr.Discard()).Start(ctx, srv.URL+"/groovesalad-128-mp3")
		if err != nil {
			t.Fatal(err)
		}
		pcm := make([]byte, 10*streams.FrameBytes)
		if _, err := io.ReadFull(dec, pcm); err != nil {
			t.Fatalf("reading: %v (%v)", err, dec.Wait())
		}
		if r := toneRatio(samples(pcm), 1000); r < 0.8 {
			t.Errorf("tone %.2f", r)
		}
		cancel()
		if err := waitDecoder(t, dec, 2*StopTimeout); err != nil {
			t.Errorf("Wait after cancel: %v", err)
		}
	})
	t.Run("untrusted", func(t *testing.T) {
		err := waitDecoder(t, startFFmpeg(t, t.Context(), srv.URL+"/groovesalad-128-mp3"), 10*time.Second)
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("Wait = %v, want a certificate failure", err)
		}
	})
}

func TestFFmpegStop(t *testing.T) {
	srv := httptest.NewServer(realTime(wav(60, 1000, 48000, 2), 48000*2*2, "audio/wav"))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	dec := startFFmpeg(t, ctx, srv.URL+"/radio")
	if _, err := io.ReadFull(dec, make([]byte, 10*streams.FrameBytes)); err != nil {
		t.Fatal(err)
	}
	stopped := time.Now()
	cancel()
	if err := waitDecoder(t, dec, StopTimeout+time.Second); err != nil {
		t.Errorf("Wait after the context ended: %v, want nil", err)
	}
	if took := time.Since(stopped); took > StopTimeout {
		t.Errorf("stopping took %s, more than StopTimeout", took)
	}
	if dec.cmd.ProcessState == nil {
		t.Error("no process state after Wait")
	}
	if err := dec.cmd.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Error("the process is still there")
	}
}

func TestFFmpegVersion(t *testing.T) {
	v, err := NewFFmpegSource(needFFmpeg(t), "", logr.Discard()).Version(t.Context())
	if err != nil || !strings.HasPrefix(v, "ffmpeg version ") || strings.ContainsAny(v, "\n\r") || len(v) > api.MaxMessageBytes {
		t.Errorf("Version = %q, %v", v, err)
	}
}

func TestFFmpegMissing(t *testing.T) {
	s := NewFFmpegSource(filepath.Join(t.TempDir(), "ffmpeg"), "", logr.Discard())
	if _, err := s.Start(t.Context(), "http://127.0.0.1:1/x"); err == nil {
		t.Error("Start with no binary succeeded")
	}
	if _, err := s.Version(t.Context()); err == nil {
		t.Error("Version with no binary succeeded")
	}
	if _, err := s.Start(t.Context(), "file:///etc/passwd"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("a source CheckSource refuses: %v", err)
	}
}
