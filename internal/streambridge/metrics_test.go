package streambridge

import (
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// gauge is the value of the sample of family name whose labels are labels
// ("k=v,k=v", sorted; "" for none), or -1 if there is none.
func gauge(t *testing.T, g prometheus.Gatherer, name, labels string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, s := range f.GetMetric() {
			var ls []string
			for _, l := range s.GetLabel() {
				ls = append(ls, l.GetName()+"="+l.GetValue())
			}
			slices.Sort(ls)
			if strings.Join(ls, ",") == labels {
				return s.GetCounter().GetValue() + s.GetGauge().GetValue()
			}
		}
	}
	return -1
}

func TestEveryMetric(t *testing.T) {
	src := newFakeSource().failStart(srcRadio, errors.New("starting ffmpeg: no such file"))
	d := doc(t, []streams.Pool{hdhomerun}, king5, kiro7, groovesalad)
	m, reg := testManager(t, src, nil, d)
	slow := join(t, m, king5) // never read: its queue overflows
	eventually(t, "a dropped frame", func() bool { return gauge(t, reg, "patchbay_stream_bridge_frames_dropped_total", "") > 0 })
	if _, err := m.join(streams.UUID("kiro7")); err == nil {
		t.Fatal("kiro7 joined a full pool")
	}
	_, _ = m.join(streams.UUID("not-a-stream"))
	failed := join(t, m, groovesalad)
	eventually(t, "radio to fail", func() bool { return ended(failed) })
	for range MaxCalls {
		if err := m.openCall(); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.openCall(); !errors.Is(err, errBridgeFull) {
		t.Fatalf("call %d: %v", MaxCalls+1, err)
	}
	(&streamsFile{path: filepath.Join(t.TempDir(), "missing.json"), m: m, log: logr.Discard()}).poll()

	code, text := get(t, newControl(m, reg, false).handler(), http.MethodGet, api.PathMetrics)
	if code != http.StatusOK {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{
		`patchbay_stream_bridge_calls{stream="king5"} 1`,
		`patchbay_stream_bridge_calls{stream="kiro7"} 0`,
		`patchbay_stream_bridge_calls{stream="groovesalad"} 0`,
		`patchbay_stream_bridge_sessions{pool="hdhomerun"} 1`,
		`patchbay_stream_bridge_sessions{pool="none"} 0`,
		`patchbay_stream_bridge_session_starts_total{result="played"} 1`,
		`patchbay_stream_bridge_session_starts_total{result="failed"} 1`,
		`patchbay_stream_bridge_refused_calls_total{reason="bridge_full"} 1`,
		`patchbay_stream_bridge_refused_calls_total{reason="tuner_busy"} 1`,
		`patchbay_stream_bridge_refused_calls_total{reason="unknown_stream"} 1`,
		`patchbay_stream_bridge_frames_dropped_total `,
		`patchbay_stream_bridge_underrun_frames_total 0`,
		`patchbay_stream_bridge_streams_version_info{version="` + d.Version + `"} 1`,
		`patchbay_stream_bridge_streams_load_failures_total 1`,
	} {
		if !strings.Contains(text, want+"\n") && !strings.Contains(text, "\n"+want) {
			t.Errorf("no %s", want)
		}
	}
	if strings.Contains(text, "tuner.test") || strings.Contains(text, "radio.test") {
		t.Error("a source URL in a metric")
	}

	m.setDocument(doc(t, []streams.Pool{hdhomerun}, kiro7))
	_, text = get(t, newControl(m, reg, false).handler(), http.MethodGet, api.PathMetrics)
	if strings.Contains(text, `stream="king5"`) || strings.Contains(text, d.Version) {
		t.Error("a removed stream or an old version still has a series")
	}
	if !strings.Contains(text, `patchbay_stream_bridge_sessions{pool="hdhomerun"} 1`) {
		t.Error("king5's session, still playing, no longer counts")
	}
	m.leave(slow)
}
