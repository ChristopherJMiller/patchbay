// Package metrics is the controller's Prometheus metrics (docs/plans/
// release-a.md W3.8, §6.2.1 "Metrics"):
//
//	patchbay_devices{kind,state}          Phones and Softphones by status.state (from the cache at scrape; "" is Pending)
//	patchbay_registered_endpoints         devices with a SIP registration in the last good observations
//	patchbay_config_version_info{version} 1 for the bundle version in pbx-config (one series)
//	patchbay_apply_failures_total         each distinct failed apply the agent reported, once
//	patchbay_compile_failures_total       reconciles whose compile or bundle encoding failed
//	patchbay_observation_age_seconds      how old the last good observations are
//	patchbay_agent_reachable              1 when the latest poll of the agent succeeded, else 0
//	patchbay_asterisk_up                  1 when the agent's AMI session to Asterisk was up at the latest poll, else 0
//
// patchbay_registered_endpoints and patchbay_observation_age_seconds are
// absent until the first successful poll, patchbay_agent_reachable until
// the first poll attempt has ended, patchbay_asterisk_up while the latest poll
// failed (patchbay_agent_reachable says so), and patchbay_config_version_info until
// the bundle reconciler has a version in place. Labels carry kinds, states
// and bundle versions, never a secret.
//
// Every method is a no-op on a nil *Metrics, so components run without
// metrics.
package metrics

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Metric names.
const (
	NameDevices             = "patchbay_devices"
	NameRegisteredEndpoints = "patchbay_registered_endpoints"
	NameConfigVersionInfo   = "patchbay_config_version_info"
	NameApplyFailures       = "patchbay_apply_failures_total"
	NameCompileFailures     = "patchbay_compile_failures_total"
	NameObservationAge      = "patchbay_observation_age_seconds"
	NameAgentReachable      = "patchbay_agent_reachable"
	NameAsteriskUp          = "patchbay_asterisk_up"
)

// StatePending is patchbay_devices' state label for a device whose status
// has no state yet (not decided, or not compiled).
const StatePending = "Pending"

// listTimeout bounds patchbay_devices' read of the cache at scrape.
const listTimeout = 5 * time.Second

// Config configures the metrics.
type Config struct {
	// Registerer is where the collectors are registered:
	// controller-runtime's metrics.Registry in production. Nil registers
	// them in a registry of their own, which nothing serves. Registering
	// twice in one registry fails (prometheus.AlreadyRegisteredError).
	Registerer prometheus.Registerer

	// Reader lists Phones and Softphones for patchbay_devices at scrape
	// (the manager's cached client). Nil: no patchbay_devices.
	Reader client.Reader

	// Namespace is the PBX's namespace.
	Namespace string

	// Observer gives the poller's latest snapshot for the registered
	// endpoints, the observation age and agent reachability at scrape. Nil:
	// none of the three.
	Observer agentview.Observer

	// Now is the clock for the observation age. Nil: time.Now.
	Now func() time.Time
}

// Metrics holds the controller's collectors.
type Metrics struct {
	configVersion   *prometheus.GaugeVec
	applyFailures   prometheus.Counter
	compileFailures prometheus.Counter

	mu          sync.Mutex
	version     string     // the config_version_info series set last
	lastFailure failureKey // the failed apply counted last
}

// failureKey is one apply attempt the agent reported.
type failureKey struct {
	version string
	at      time.Time
}

// New builds the collectors and registers them.
func New(cfg Config) (*Metrics, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	reg := cfg.Registerer
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &Metrics{
		configVersion: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameConfigVersionInfo,
			Help: "1 for the bundle version the controller has in pbx-config.",
		}, []string{"version"}),
		applyFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameApplyFailures,
			Help: "Bundle versions the agent reported it refused or rolled back, each attempt once.",
		}),
		compileFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameCompileFailures,
			Help: "Reconciles whose compile or bundle encoding failed; the previous bundle stayed.",
		}),
	}
	collectors := []prometheus.Collector{m.configVersion, m.applyFailures, m.compileFailures}
	if cfg.Reader != nil {
		collectors = append(collectors, &devicesCollector{reader: cfg.Reader, namespace: cfg.Namespace})
	}
	if cfg.Observer != nil {
		collectors = append(collectors, &observerCollector{observer: cfg.Observer, now: cfg.Now})
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// CompileFailed counts a failed compile.
func (m *Metrics) CompileFailed() {
	if m == nil {
		return
	}
	m.compileFailures.Inc()
}

// ConfigWritten records the bundle version now in pbx-config: its
// config_version_info series is 1 and the previous version's series is
// removed. The bundle reconciler calls it after every reconcile that leaves
// pbx-config at a version, written or already current, so a restarted
// controller reports it too.
func (m *Metrics) ConfigWritten(version string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if version == m.version {
		return
	}
	m.configVersion.Reset()
	m.configVersion.WithLabelValues(version).Set(1)
	m.version = version
}

// ObserveSnapshot counts the agent's failed apply in s, if it has one that
// was not counted yet: each distinct (LastApplyVersion, LastApplyAt) with a
// LastApplyError counts once, however many polls report it.
func (m *Metrics) ObserveSnapshot(s agentview.Snapshot) {
	if m == nil {
		return
	}
	a := s.Document.Agent
	if a.LastApplyError == "" {
		return
	}
	k := failureKey{version: a.LastApplyVersion}
	if a.LastApplyAt != nil {
		k.at = *a.LastApplyAt
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if k.version == m.lastFailure.version && k.at.Equal(m.lastFailure.at) {
		return
	}
	m.lastFailure = k
	m.applyFailures.Inc()
}

// devicesCollector is patchbay_devices, counted from the cache at scrape.
type devicesCollector struct {
	reader    client.Reader
	namespace string
}

var devicesDesc = prometheus.NewDesc(NameDevices,
	"Phones and Softphones by status.state (Pending: no state yet).", []string{"kind", "state"}, nil)

func (c *devicesCollector) Describe(ch chan<- *prometheus.Desc) { ch <- devicesDesc }

func (c *devicesCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	counts := map[[2]string]int{}
	var phones v1alpha1.PhoneList
	var softphones v1alpha1.SoftphoneList
	err := errors.Join(
		c.reader.List(ctx, &phones, client.InNamespace(c.namespace)),
		c.reader.List(ctx, &softphones, client.InNamespace(c.namespace)),
	)
	if err != nil {
		// A scrape is no place to fail the whole registry; the series
		// vanish until the cache answers again.
		return
	}
	for i := range phones.Items {
		counts[[2]string{string(drivers.KindPhone), stateLabel(phones.Items[i].Status.State)}]++
	}
	for i := range softphones.Items {
		counts[[2]string{string(drivers.KindSoftphone), stateLabel(softphones.Items[i].Status.State)}]++
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(devicesDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
	}
}

func stateLabel(state string) string {
	if state == "" {
		return StatePending
	}
	return state
}

// observerCollector is everything read from the poller's snapshot at
// scrape.
type observerCollector struct {
	observer agentview.Observer
	now      func() time.Time
}

var (
	registeredDesc = prometheus.NewDesc(NameRegisteredEndpoints,
		"Devices with a SIP registration in the last good observations of the agent.", nil, nil)
	ageDesc = prometheus.NewDesc(NameObservationAge,
		"Seconds since the last good observations of the agent were fetched.", nil, nil)
	reachableDesc = prometheus.NewDesc(NameAgentReachable,
		"1 when the latest poll of the agent's observations succeeded, else 0.", nil, nil)
	asteriskUpDesc = prometheus.NewDesc(NameAsteriskUp,
		"1 when the agent's AMI session to Asterisk was up at the latest poll, else 0 (absent while the agent cannot be reached).", nil, nil)
)

func (c *observerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- registeredDesc
	ch <- ageDesc
	ch <- reachableDesc
	ch <- asteriskUpDesc
}

func (c *observerCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.observer.Snapshot()
	if s.Started() {
		ch <- prometheus.MustNewConstMetric(reachableDesc, prometheus.GaugeValue, boolValue(s.AgentReachable))
	}
	if s.AgentReachable {
		ch <- prometheus.MustNewConstMetric(asteriskUpDesc, prometheus.GaugeValue, boolValue(s.Document.Agent.AsteriskUp))
	}
	if s.PolledAt.IsZero() {
		return
	}
	registered := 0
	for i := range s.Document.Devices {
		if s.Document.Devices[i].Registration != nil {
			registered++
		}
	}
	ch <- prometheus.MustNewConstMetric(registeredDesc, prometheus.GaugeValue, float64(registered))
	ch <- prometheus.MustNewConstMetric(ageDesc, prometheus.GaugeValue, max(0, c.now().Sub(s.PolledAt).Seconds()))
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
