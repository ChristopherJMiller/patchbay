package compile_test

import (
	"bytes"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// bridge is the stream bridge's address in these tests, as the chart
// renders it for release patchbay in namespace telephony.
const bridge = "patchbay-streams.telephony.svc:9092"

// streamHouse is the fake house with the stream bridge on and the given
// Streams.
func streamHouse(list ...v1alpha1.Stream) (compile.Inputs, compile.Settings) {
	in, s := fakeHouse()
	s.StreamBridge = bridge
	in.Streams = list
	return in, s
}

// sourced is a Stream with its own source.
func sourced(name string, minutes int, number, source string) v1alpha1.Stream {
	st := stream(name, minutes, number)
	st.Spec.Source = source
	return st
}

func extensions(b compile.Bundle) string { return string(b.Files["asterisk/extensions.conf"]) }

// TestStreamDialplan pins the three forms a Stream holding its number
// takes in the dialplan (docs/plans/release-b.md RT.1): routed to the
// bridge, with the time limit and "not available" after; "not available"
// with streams disabled; "not available" with its source refused. The
// display name never reaches the dialplan.
func TestStreamDialplan(t *testing.T) {
	reg := fakeRegistry()
	king := sourced("king5", 3, "705", "http://192.168.0.58:5004/auto/v5.1")
	king.Spec.Name = "KING 5 ;-- @@secret:patchbay-ami/password@@"

	t.Run("routed", func(t *testing.T) {
		in, s := streamHouse(king)
		s.Streams.CallLimit = metav1.Duration{Duration: 90 * time.Minute}
		want := "; Stream king5\n" +
			"exten => 705,1,Answer()\n" +
			" same => n,Set(TIMEOUT(absolute)=5400)\n" +
			" same => n,Dial(AudioSocket/" + bridge + "/89fb6719-3e3d-5ef3-881a-b9f423a8a432/c(slin16),,gS(5400))\n" +
			" same => n,Goto(patchbay-unavailable,s,1)\n"
		ext := extensions(compileOK(t, reg, in, s))
		if !strings.Contains(ext, want) {
			t.Errorf("extensions.conf lacks\n%s\nin\n%s", want, ext)
		}
		if streams.UUID("king5") != "89fb6719-3e3d-5ef3-881a-b9f423a8a432" {
			t.Errorf("king5's UUID is %s", streams.UUID("king5"))
		}
		if strings.Contains(ext, "KING") || strings.Contains(ext, "@@") {
			t.Errorf("the display name reached the dialplan:\n%s", ext)
		}
		if _, err := parseConf([]byte(ext)); err != nil {
			t.Errorf("extensions.conf does not parse: %v", err)
		}
	})

	t.Run("the default limit", func(t *testing.T) {
		in, s := streamHouse(king)
		if ext := extensions(compileOK(t, reg, in, s)); !strings.Contains(ext, " same => n,Set(TIMEOUT(absolute)=7200)\n") || !strings.Contains(ext, "/c(slin16),,gS(7200))\n") {
			t.Errorf("the default call limit is not 2 h:\n%s", ext)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		in, s := streamHouse(king)
		s.StreamBridge = ""
		b := compileOK(t, reg, in, s)
		want := "; Stream king5: streams are disabled (no stream bridge)\nexten => 705,1,Goto(patchbay-unavailable,s,1)\n"
		if ext := extensions(b); !strings.Contains(ext, want) || strings.Contains(ext, "AudioSocket") || strings.Contains(ext, "TIMEOUT") {
			t.Errorf("extensions.conf lacks\n%s\nor routes a stream:\n%s", want, ext)
		}
		if _, ok := b.Streams.Streams[streams.UUID("king5")]; !ok {
			t.Errorf("a stream of a disabled install is not in the document: %+v", b.Streams)
		}
	})

	t.Run("refused", func(t *testing.T) {
		in, s := streamHouse(sourced("king5", 3, "705", "rtsp://192.168.0.58/auto/v5.1"))
		b := compileOK(t, reg, in, s)
		want := "; Stream king5: its source is refused (streams: the source's scheme is \"rtsp\"; only http and https are read)\n" +
			"exten => 705,1,Goto(patchbay-unavailable,s,1)\n"
		if ext := extensions(b); !strings.Contains(ext, want) || strings.Contains(ext, "AudioSocket") {
			t.Errorf("extensions.conf lacks\n%s\nor routes the stream:\n%s", want, ext)
		}
		if len(b.Streams.Streams) != 0 {
			t.Errorf("a refused stream is in the document: %+v", b.Streams.Streams)
		}
	})
}

// TestStreamDocument: Bundle.Streams holds every Stream that holds its
// number with an acceptable source, keyed by UUID, with its pool, and
// every tuner pool; it is sealed, and is neither one of the bundle's
// files nor part of its version, so a source change leaves pbx-config as
// it is.
func TestStreamDocument(t *testing.T) {
	reg := fakeRegistry()
	in, s := streamHouse(
		sourced("king5", 3, "705", "http://192.168.0.58:5004/auto/v5.1"),
		sourced("kiro7", 3, "707", "http://192.168.0.58:5004/auto/v7.1"),
		sourced("groovesalad", 3, "750", "https://ice2.somafm.com/groovesalad-128-mp3"),
		sourced("late", 4, "705", "http://192.168.0.58:5004/auto/v5.1"),         // 705 is king5's
		sourced("person-range", 4, "305", "http://192.168.0.58:5004/auto/v9.1"), // not a stream number
		sourced("playlist", 4, "751", "https://somafm.com/groovesalad130.pls"),  // refused
		sourced("attic", 4, "760", "http://attic.lan:5004/auto/v13.1"),          // the unused pool's host
		sourced("king5-again", 4, "706", "http://192.168.0.58:5004/auto/v5.1"),  // shares king5's source
	)
	s.Streams.TunerPools = []compile.TunerPool{
		{Name: "unused", Hosts: []string{"192.0.2.99"}, Max: 2},
		{Name: "hdhomerun", Hosts: []string{"192.168.0.58"}, Max: 1},
		{Name: "attic", Hosts: []string{"attic.lan"}, Max: 1},
	}
	b := compileOK(t, reg, in, s)
	d := b.Streams
	if err := d.Validate(); err != nil {
		t.Fatalf("the document does not validate: %v", err)
	}
	if want := []streams.Pool{{Name: "attic", Max: 1}, {Name: "hdhomerun", Max: 1}, {Name: "unused", Max: 2}}; !slices.Equal(d.Pools, want) {
		t.Errorf("pools %+v, want %+v", d.Pools, want)
	}
	want := map[string]streams.Stream{}
	for _, st := range []streams.Stream{
		{Name: "king5", Number: "705", Source: "http://192.168.0.58:5004/auto/v5.1", Pool: "hdhomerun"},
		{Name: "kiro7", Number: "707", Source: "http://192.168.0.58:5004/auto/v7.1", Pool: "hdhomerun"},
		{Name: "groovesalad", Number: "750", Source: "https://ice2.somafm.com/groovesalad-128-mp3"},
		{Name: "attic", Number: "760", Source: "http://attic.lan:5004/auto/v13.1", Pool: "attic"},
		{Name: "king5-again", Number: "706", Source: "http://192.168.0.58:5004/auto/v5.1", Pool: "hdhomerun"},
	} {
		want[streams.UUID(st.Name)] = st
	}
	if !maps.Equal(d.Streams, want) {
		t.Errorf("streams %+v,\nwant %+v", d.Streams, want)
	}
	raw, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if back, err := streams.Decode(raw); err != nil || !reflect.DeepEqual(back, d) {
		t.Errorf("the document does not round-trip: %v", err)
	}
	for p := range b.Files {
		if strings.Contains(p, streams.FileName) {
			t.Errorf("streams.json is among the bundle's files: %s", p)
		}
	}

	// A new source, or a pool's new max, changes only the document.
	in.Streams[0].Spec.Source = "http://192.168.0.58:5004/auto/v5.2"
	s.Streams.TunerPools[1].Max = 2
	after := compileOK(t, reg, in, s)
	if after.Version != b.Version || !maps.EqualFunc(after.Files, b.Files, bytes.Equal) {
		t.Error("a source or pool change changed pbx-config")
	}
	if after.Streams.Version == b.Streams.Version || after.Streams.Streams[streams.UUID("king5")].Source != in.Streams[0].Spec.Source {
		t.Errorf("the document did not follow the source: %+v", after.Streams.Streams[streams.UUID("king5")])
	}

	// No streams, no pools: an empty sealed document.
	in, s = fakeHouse()
	empty := compileOK(t, reg, in, s).Streams
	if empty.Version == "" || empty.Pools == nil || empty.Streams == nil || len(empty.Pools)+len(empty.Streams) != 0 {
		t.Errorf("the empty document %+v is not sealed and empty", empty)
	}
	if err := empty.Validate(); err != nil {
		t.Error(err)
	}
}

// TestPoolMatching: a source is in a pool when its URL's host, lower-cased,
// is one of the pool's hosts, lower-cased, whatever the port; a host
// written another way (a name for the IP) is not.
func TestPoolMatching(t *testing.T) {
	reg := fakeRegistry()
	pools := []compile.TunerPool{
		{Name: "hdhomerun", Hosts: []string{"192.168.0.58", "HDHomeRun.lan"}, Max: 1},
		{Name: "v6", Hosts: []string{"fd00::58"}, Max: 1},
	}
	for source, want := range map[string]string{
		"http://192.168.0.58:5004/auto/v5.1":      "hdhomerun",
		"http://192.168.0.58/auto/v5.1":           "hdhomerun",
		"https://192.168.0.58:443/x":              "hdhomerun",
		"http://hdhomerun.LAN:5004/auto/v5.1":     "hdhomerun",
		"HTTP://HDHOMERUN.LAN/auto/v5.1":          "hdhomerun",
		"http://[fd00::58]:5004/auto/v5.1":        "v6",
		"http://[FD00::58]/auto/v5.1":             "v6",
		"http://192.168.0.580:5004/auto/v5.1":     "",
		"http://hdhomerun.lan.example:5004/x":     "",
		"http://192.168.0.5:5004/auto/v5.1":       "",
		"https://ice2.somafm.com/groovesalad-128": "",
	} {
		in, s := streamHouse(sourced("tv", 0, "705", source))
		s.Streams.TunerPools = pools
		b := compileOK(t, reg, in, s)
		got, ok := b.Streams.Streams[streams.UUID("tv")]
		if !ok || got.Pool != want {
			t.Errorf("%s: pool %q (in the document: %t), want %q", source, got.Pool, ok, want)
		}
	}
}

// TestStreamRefusedSource: a Stream whose source breaks streams.CheckSource
// (written before the CRD's rules) is left out of the document and plays
// "not available", saying why in a comment; the compile does not fail,
// and nothing a source holds can form a placeholder in Asterisk's config.
func TestStreamRefusedSource(t *testing.T) {
	reg := fakeRegistry()
	for source, why := range map[string]string{
		"rtsp://192.168.0.58/auto/v5.1":                    `the source's scheme is "rtsp"`,
		"https://somafm.com/groovesalad130.pls":            "the source is a playlist (.pls or .m3u)",
		"http://somafm.com/x/LIST.M3U":                     "the source is a playlist",
		"http://user:pass@192.168.0.58/auto/v5.1":          "the source carries credentials",
		"file:///etc/passwd":                               `the source's scheme is "file"`,
		"http:///no-host":                                  "the source has no host",
		"http://[@@secret:patchbay-ami/password@@":         "the source is not a URL",
		"http://192.168.0.58/" + strings.Repeat("a", 2100): "more than 2048",
	} {
		in, s := streamHouse(sourced("tv", 0, "705", source), sourced("radio", 0, "750", "https://ice2.somafm.com/dronezone-128-mp3"))
		b := compileOK(t, reg, in, s)
		ext := extensions(b)
		line := ""
		for l := range strings.SplitSeq(ext, "\n") {
			if strings.HasPrefix(l, "; Stream tv:") {
				line = l
			}
		}
		if !strings.HasPrefix(line, "; Stream tv: its source is refused (streams: ") || !strings.Contains(line, why) || !strings.HasSuffix(line, ")") {
			t.Errorf("%.60s: comment %q, want it to say %q", source, line, why)
		}
		if !strings.Contains(ext, line+"\nexten => 705,1,Goto(patchbay-unavailable,s,1)\n") {
			t.Errorf("%.60s: 705 does not play \"not available\":\n%s", source, ext)
		}
		if strings.Contains(ext, "@@") || len(line) > 300 {
			t.Errorf("%.60s: the comment %q is unbounded or holds a placeholder", source, line)
		}
		if _, ok := b.Streams.Streams[streams.UUID("tv")]; ok || len(b.Streams.Streams) != 1 {
			t.Errorf("%.60s: document %+v, want only radio", source, b.Streams.Streams)
		}
	}
}
