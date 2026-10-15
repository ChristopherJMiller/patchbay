package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/metrics"
)

const (
	v1 = "1111111111111111111111111111111111111111111111111111111111111111"
	v2 = "2222222222222222222222222222222222222222222222222222222222222222"
)

// series is one sample: its labels ("k=v,k=v", sorted) and value.
type series map[string]float64

// gather returns the named family's samples, keyed by their labels.
func gather(t *testing.T, m *metrics.Metrics, name string) series {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		out := series{}
		for _, s := range f.GetMetric() {
			var labels []string
			for _, l := range s.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			slices.Sort(labels)
			out[strings.Join(labels, ",")] = s.GetCounter().GetValue() + s.GetGauge().GetValue()
		}
		return out
	}
	return nil
}

func TestAppliedVersionIsOneSeries(t *testing.T) {
	m := metrics.New()
	if got := gather(t, m, "patchbay_agent_applied_version_info"); len(got) != 0 {
		t.Errorf("before any apply: %v, want no series", got)
	}
	m.SetAppliedVersion(v1)
	m.SetAppliedVersion(v2)
	want := series{"version=" + v2: 1}
	if got := gather(t, m, "patchbay_agent_applied_version_info"); !equal(got, want) {
		t.Errorf("applied version = %v, want %v", got, want)
	}
	m.SetAppliedVersion("")
	if got := gather(t, m, "patchbay_agent_applied_version_info"); len(got) != 0 {
		t.Errorf("after an empty version: %v, want no series", got)
	}
}

func TestCounters(t *testing.T) {
	m := metrics.New()
	m.ApplyFailed()
	m.ApplyFailed()
	if got := gather(t, m, "patchbay_agent_apply_failures_total"); !equal(got, series{"": 2}) {
		t.Errorf("apply failures = %v, want 2", got)
	}

	m.Fetch(api.TransportTFTP, api.FetchServed)
	m.Fetch(api.TransportTFTP, api.FetchServed)
	m.Fetch(api.TransportHTTPDevice, api.FetchNotFound)
	got := gather(t, m, "patchbay_agent_fetches_total")
	// Every transport and result is there from the start, at 0.
	if len(got) != 9 {
		t.Errorf("fetch series = %v, want all 9 transport/result pairs", got)
	}
	for labels, want := range map[string]float64{
		"result=served,transport=tftp":       2,
		"result=notFound,transport=http8088": 1,
		"result=error,transport=http6970":    0,
	} {
		if got[labels] != want {
			t.Errorf("fetches{%s} = %v, want %v", labels, got[labels], want)
		}
	}

	m.SetRegisteredEndpoints(3)
	m.SetRegisteredEndpoints(2)
	if got := gather(t, m, "patchbay_agent_registered_endpoints"); !equal(got, series{"": 2}) {
		t.Errorf("registered endpoints = %v, want 2", got)
	}
}

func TestHandler(t *testing.T) {
	m := metrics.New()
	m.SetAppliedVersion(v1)
	m.Fetch(api.TransportHTTP6970, api.FetchError)
	ts := httptest.NewServer(m.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	for _, want := range []string{
		`patchbay_agent_applied_version_info{version="` + v1 + `"} 1`,
		`patchbay_agent_fetches_total{result="error",transport="http6970"} 1`,
		`patchbay_agent_apply_failures_total 0`,
		`patchbay_agent_registered_endpoints 0`,
		"go_goroutines ",
		"process_",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

// TestNilIsANoOp: every method of a nil *Metrics does nothing, so a
// component may run without metrics.
func TestNilIsANoOp(t *testing.T) {
	var m *metrics.Metrics
	m.SetAppliedVersion(v1)
	m.ApplyFailed()
	m.Fetch(api.TransportTFTP, api.FetchServed)
	m.SetRegisteredEndpoints(1)
	if m.Handler() != nil {
		t.Error("a nil *Metrics has a handler; want nil, so /metrics stays unrouted")
	}
	if m.Registry() != nil {
		t.Error("a nil *Metrics has a registry")
	}
}

// TestOwnRegistry: two instances do not share series (nothing is
// registered globally).
func TestOwnRegistry(t *testing.T) {
	a, b := metrics.New(), metrics.New()
	a.ApplyFailed()
	if got := gather(t, b, "patchbay_agent_apply_failures_total"); !equal(got, series{"": 0}) {
		t.Errorf("b saw a's failure: %v", got)
	}
}

func equal(a, b series) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
