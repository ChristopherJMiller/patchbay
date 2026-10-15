package metrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
)

const ns = "telephony"

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// scrape returns the registry's exposition, without comment lines, as
// "name{labels} value" lines.
func scrape(t *testing.T, reg *prometheus.Registry) map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: %d %s", rec.Code, rec.Body)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		out[line[:i]] = line[i+1:]
	}
	return out
}

// names lists the series whose metric name is name.
func series(m map[string]string, name string) []string {
	var out []string
	for k := range m {
		if k == name || strings.HasPrefix(k, name+"{") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func phone(name, state string) *v1alpha1.Phone {
	p := &v1alpha1.Phone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	p.Status.State = state
	return p
}

func softphone(name, state string) *v1alpha1.Softphone {
	s := &v1alpha1.Softphone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	s.Status.State = state
	return s
}

func TestEveryMetric(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		phone("desk", "Registered"),
		phone("kitchen", "Registered"),
		phone("garage", ""),
		softphone("laptop", "Issued"),
		// Another namespace is not counted.
		&v1alpha1.Phone{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "elsewhere"}},
	).Build()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	applyAt := now.Add(-time.Minute)
	holder := &agentview.Holder{}
	holder.Set(agentview.Snapshot{
		Document: agentapi.Observations{
			Agent: agentapi.AgentStatus{AsteriskUp: true, LastApplyVersion: "v2", LastApplyError: "verify: no", LastApplyAt: &applyAt},
			Devices: []agentapi.DeviceObservations{
				{Device: "phone-desk", Registration: &agentapi.Registration{Contact: "sip:100@x"}},
				{Device: "phone-kitchen", Registration: &agentapi.Registration{Contact: "sip:101@x"}},
				{Device: "phone-garage"},
			},
		},
		PolledAt:       now.Add(-7 * time.Second),
		AgentReachable: true,
		Polls:          3,
	})
	reg := prometheus.NewRegistry()
	m, err := metrics.New(metrics.Config{
		Registerer: reg, Reader: c, Namespace: ns, Observer: holder,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.ConfigWritten("v2")
	m.CompileFailed()
	m.ObserveSnapshot(holder.Snapshot())

	got := scrape(t, reg)
	want := map[string]string{
		`patchbay_devices{kind="Phone",state="Registered"}`: "2",
		`patchbay_devices{kind="Phone",state="Pending"}`:    "1",
		`patchbay_devices{kind="Softphone",state="Issued"}`: "1",
		`patchbay_registered_endpoints`:                     "2",
		`patchbay_config_version_info{version="v2"}`:        "1",
		`patchbay_apply_failures_total`:                     "1",
		`patchbay_compile_failures_total`:                   "1",
		`patchbay_observation_age_seconds`:                  "7",
		`patchbay_agent_reachable`:                          "1",
		`patchbay_asterisk_up`:                              "1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if n := len(series(got, metrics.NameDevices)); n != 3 {
		t.Errorf("patchbay_devices has %d series, want 3: %v", n, series(got, metrics.NameDevices))
	}
	if len(got) != len(want) {
		t.Errorf("%d series, want %d: %v", len(got), len(want), got)
	}

	// Unreachable: the gauge drops to 0, the age keeps growing from the
	// last good poll.
	s := holder.Snapshot()
	s.AgentReachable = false
	s.Polls++
	holder.Set(s)
	now = now.Add(10 * time.Second)
	got = scrape(t, reg)
	if got[metrics.NameAgentReachable] != "0" || got[metrics.NameObservationAge] != "17" {
		t.Errorf("after a failed poll: reachable %q, age %q; want 0 and 17", got[metrics.NameAgentReachable], got[metrics.NameObservationAge])
	}
	// What the agent last said of Asterisk is not repeated while it cannot
	// be asked.
	if s := series(got, metrics.NameAsteriskUp); len(s) != 0 {
		t.Errorf("asterisk_up present while the agent is unreachable: %v", s)
	}

	// Reachable again, Asterisk down (the PBXDown case): 0.
	s.AgentReachable = true
	s.Document.Agent.AsteriskUp = false
	s.Polls++
	holder.Set(s)
	if got = scrape(t, reg); got[metrics.NameAsteriskUp] != "0" {
		t.Errorf("asterisk_up = %q with Asterisk down, want 0", got[metrics.NameAsteriskUp])
	}
}

func TestAbsentBeforeFirstPoll(t *testing.T) {
	holder := &agentview.Holder{}
	reg := prometheus.NewRegistry()
	if _, err := metrics.New(metrics.Config{Registerer: reg, Observer: holder}); err != nil {
		t.Fatal(err)
	}
	got := scrape(t, reg)
	for _, name := range []string{metrics.NameAgentReachable, metrics.NameAsteriskUp, metrics.NameObservationAge, metrics.NameRegisteredEndpoints, metrics.NameConfigVersionInfo, metrics.NameDevices} {
		if s := series(got, name); len(s) != 0 {
			t.Errorf("%s present before any poll: %v", name, s)
		}
	}
	// The counters start at 0.
	if got[metrics.NameApplyFailures] != "0" || got[metrics.NameCompileFailures] != "0" {
		t.Errorf("counters = %q, %q; want 0", got[metrics.NameApplyFailures], got[metrics.NameCompileFailures])
	}

	// The first attempt failed: reachable 0, but no age (nothing good yet).
	holder.Set(agentview.Snapshot{Polls: 1, LastError: "connection refused"})
	got = scrape(t, reg)
	if got[metrics.NameAgentReachable] != "0" {
		t.Errorf("reachable = %q after a failed first poll, want 0", got[metrics.NameAgentReachable])
	}
	if s := series(got, metrics.NameObservationAge); len(s) != 0 {
		t.Errorf("age present before the first success: %v", s)
	}
	if s := series(got, metrics.NameRegisteredEndpoints); len(s) != 0 {
		t.Errorf("registered endpoints present before the first success: %v", s)
	}
}

func TestApplyFailuresCountedOnce(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := metrics.New(metrics.Config{Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	snap := func(version, apiErr string, at *time.Time) agentview.Snapshot {
		return agentview.Snapshot{Polls: 1, Document: agentapi.Observations{Agent: agentapi.AgentStatus{
			LastApplyVersion: version, LastApplyError: apiErr, LastApplyAt: at,
		}}}
	}
	for _, s := range []agentview.Snapshot{
		snap("good", "", &t1),             // a success counts nothing
		snap("bad", "verify failed", &t1), // 1
		snap("bad", "verify failed", &t1), // the same failure, polled again
		snap("bad", "verify failed", &t1), // and again (a failed poll keeps the document)
		snap("bad", "verify failed", &t2), // the same version failing again later: 2
		snap("good", "", &t2),
		snap("bad", "verify failed", &t2), // the failure counted last, reported again
		snap("worse", "refused", nil),     // 3
		snap("worse", "refused", nil),
	} {
		m.ObserveSnapshot(s)
	}
	if got := scrape(t, reg)[metrics.NameApplyFailures]; got != "3" {
		t.Fatalf("apply failures = %s, want 3", got)
	}
}

func TestConfigWrittenDropsPreviousVersion(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := metrics.New(metrics.Config{Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	m.ConfigWritten("aaa")
	m.ConfigWritten("aaa")
	m.ConfigWritten("bbb")
	got := series(scrape(t, reg), metrics.NameConfigVersionInfo)
	if len(got) != 1 || got[0] != `patchbay_config_version_info{version="bbb"}` {
		t.Fatalf("config_version_info series = %v, want only bbb", got)
	}
	m.ConfigWritten("aaa")
	got = series(scrape(t, reg), metrics.NameConfigVersionInfo)
	if len(got) != 1 || got[0] != `patchbay_config_version_info{version="aaa"}` {
		t.Fatalf("config_version_info series = %v, want only aaa", got)
	}
}

func TestNilMetricsDoNothing(t *testing.T) {
	var m *metrics.Metrics
	m.CompileFailed()
	m.ConfigWritten("v")
	m.ObserveSnapshot(agentview.Snapshot{Document: agentapi.Observations{Agent: agentapi.AgentStatus{LastApplyError: "x"}}})
}

func TestRegisteringTwiceFails(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := metrics.New(metrics.Config{Registerer: reg}); err != nil {
		t.Fatal(err)
	}
	_, err := metrics.New(metrics.Config{Registerer: reg})
	var already prometheus.AlreadyRegisteredError
	if !errors.As(err, &already) {
		t.Fatalf("second New on one registry = %v, want AlreadyRegisteredError", err)
	}
	// Without a Registerer each Metrics has a registry of its own.
	for range 2 {
		if _, err := metrics.New(metrics.Config{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDevicesAbsentWhenTheCacheFails(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(phone("desk", "Registered")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("cache not started")
			},
		}).Build()
	reg := prometheus.NewRegistry()
	if _, err := metrics.New(metrics.Config{Registerer: reg, Reader: c, Namespace: ns}); err != nil {
		t.Fatal(err)
	}
	if s := series(scrape(t, reg), metrics.NameDevices); len(s) != 0 {
		t.Fatalf("patchbay_devices with a failing cache: %v, want none", s)
	}
}
