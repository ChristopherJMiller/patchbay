package streambridge_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// TestFFmpegArgs pins the decoder's command line (plan §2(m)): an http
// source gets no TLS options, an https one has its certificate verified.
func TestFFmpegArgs(t *testing.T) {
	head := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "http,https,tcp,tls,crypto",
		"-format_whitelist", "mpegts,mp3,aac,ogg,flac,wav,hls,mov",
		"-rw_timeout", "10000000", "-probesize", "1000000", "-analyzeduration", "1000000",
	}
	tail := func(src string) []string {
		return []string{"-i", src, "-vn", "-sn", "-dn", "-ac", "1", "-ar", "16000", "-acodec", "pcm_s16le", "-f", "s16le", "pipe:1"}
	}
	tv := "http://192.168.0.58:5004/auto/v5.1"
	if got, want := streambridge.FFmpegArgs(tv, "/ca.pem"), append(slices.Clone(head), tail(tv)...); !slices.Equal(got, want) {
		t.Errorf("http:\n got %q\nwant %q", got, want)
	}
	radio := "https://ice2.somafm.com/groovesalad-128-mp3"
	want := append(append(slices.Clone(head), "-tls_verify", "1"), tail(radio)...)
	if got := streambridge.FFmpegArgs(radio, ""); !slices.Equal(got, want) {
		t.Errorf("https, system trust:\n got %q\nwant %q", got, want)
	}
	want = append(append(slices.Clone(head), "-tls_verify", "1", "-ca_file", "/ca.pem"), tail("HTTPS://x/y")...)
	if got := streambridge.FFmpegArgs("HTTPS://x/y", "/ca.pem"); !slices.Equal(got, want) {
		t.Errorf("https, a CA file:\n got %q\nwant %q", got, want)
	}
}

// TestTimings: the grace and queue the design names, and the bounds
// between the timings that the session manager relies on.
func TestTimings(t *testing.T) {
	if streambridge.Grace != 5*time.Second {
		t.Errorf("Grace %s, design §9 says 5 s", streambridge.Grace)
	}
	if time.Duration(streambridge.QueueFrames)*streams.FrameDuration != time.Second {
		t.Errorf("QueueFrames %d is not 1 s of audio", streambridge.QueueFrames)
	}
	if streambridge.PreemptSettle >= streambridge.Grace || streambridge.StallTimeout >= streambridge.StartTimeout {
		t.Error("PreemptSettle < Grace and StallTimeout < StartTimeout")
	}
	if streambridge.MaxSessions > streambridge.MaxCalls {
		t.Error("MaxSessions > MaxCalls")
	}
	if streambridge.DefaultListen != ":9092" || streambridge.DefaultControlListen != ":9093" ||
		!strings.HasSuffix(streambridge.DefaultStreamsFile, "/"+streams.FileName) {
		t.Errorf("defaults: %s %s %s", streambridge.DefaultListen, streambridge.DefaultControlListen, streambridge.DefaultStreamsFile)
	}
}
