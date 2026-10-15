// Package metrics is the agent's Prometheus metrics (W2.11), served on the
// control port at api.PathMetrics without a token: counts and version
// digests, never a secret.
//
//	patchbay_agent_applied_version_info{version}    1 for the live bundle version (one series)
//	patchbay_agent_apply_failures_total             versions refused or rolled back
//	patchbay_agent_fetches_total{transport,result}  every valid fetch event, attributed or not
//	patchbay_agent_registered_endpoints             devices with a SIP contact at the last AMI poll
//
// plus the Go runtime and process collectors. Each Metrics has its own
// registry, so nothing is registered globally and tests can run in
// parallel. Every method is a no-op on a nil *Metrics, so components may
// run without metrics.
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// Metrics holds the agent's collectors.
type Metrics struct {
	reg *prometheus.Registry

	mu         sync.Mutex // SetAppliedVersion's reset and set are one step
	applied    *prometheus.GaugeVec
	failures   prometheus.Counter
	fetches    *prometheus.CounterVec
	registered prometheus.Gauge
}

// transports and results are every label value fetches_total starts with
// at 0, so a dashboard sees each series before its first event.
var (
	transports = []api.Transport{api.TransportTFTP, api.TransportHTTP6970, api.TransportHTTPDevice}
	results    = []api.FetchResult{api.FetchServed, api.FetchNotFound, api.FetchError}
)

// New returns the metrics on a registry of their own, with the Go and
// process collectors.
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		applied: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patchbay_agent_applied_version_info",
			Help: "1 for the bundle version that is live in the PBX pod.",
		}, []string{"version"}),
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "patchbay_agent_apply_failures_total",
			Help: "Bundle versions the agent refused or rolled back.",
		}),
		fetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patchbay_agent_fetches_total",
			Help: "File fetches by transport and result, attributed to a device or not.",
		}, []string{"transport", "result"}),
		registered: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "patchbay_agent_registered_endpoints",
			Help: "Devices with a registered SIP contact at the last AMI poll.",
		}),
	}
	for _, t := range transports {
		for _, r := range results {
			m.fetches.WithLabelValues(string(t), string(r))
		}
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.applied, m.failures, m.fetches, m.registered,
	)
	return m
}

// Handler serves the registry in the Prometheus text format
// (agent.Options.Metrics). Nil for a nil *Metrics, which leaves /metrics
// unrouted.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return nil
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry is the metrics' own registry (nil for a nil *Metrics).
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// SetAppliedVersion makes version the one series of
// patchbay_agent_applied_version_info; "" leaves none.
func (m *Metrics) SetAppliedVersion(version string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applied.Reset()
	if version != "" {
		m.applied.WithLabelValues(version).Set(1)
	}
}

// ApplyFailed counts a version that was refused or rolled back.
func (m *Metrics) ApplyFailed() {
	if m == nil {
		return
	}
	m.failures.Inc()
}

// Fetch counts one fetch event.
func (m *Metrics) Fetch(t api.Transport, r api.FetchResult) {
	if m == nil {
		return
	}
	m.fetches.WithLabelValues(string(t), string(r)).Inc()
}

// SetRegisteredEndpoints sets how many devices have a registered contact.
func (m *Metrics) SetRegisteredEndpoints(n int) {
	if m == nil {
		return
	}
	m.registered.Set(float64(n))
}
