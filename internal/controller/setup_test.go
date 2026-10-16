package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// fakeAgent is the agent's control API as the controller sees it: it
// answers GET /v1/observations only with the token the controller minted,
// reporting Asterisk up, the version in pbx-config applied, and one unknown
// phone.
type fakeAgent struct {
	t         *testing.T
	namespace string
	names     install.Names

	mu       sync.Mutex
	authOK   int // requests with the minted token
	refused  int // requests with any other
	unknown  agentapi.UnknownDevice
	started  time.Time
	lastSeen string // the version last reported applied
}

func (a *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var tok corev1.Secret
	err := testClient.Get(ctx, client.ObjectKey{Namespace: a.namespace, Name: a.names.AgentTokenSecret}, &tok)
	want := ""
	if err == nil {
		want = strings.TrimSpace(string(tok.Data[install.AgentTokenKey]))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if want == "" || r.Header.Get("Authorization") != "Bearer "+want {
		a.refused++
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(agentapi.Error{Error: "missing or wrong agent token"})
		return
	}
	a.authOK++
	if r.Method != http.MethodGet || r.URL.Path != agentapi.PathObservations {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	applied := ""
	var cm corev1.ConfigMap
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: a.namespace, Name: a.names.ConfigMap}, &cm); err == nil {
		if idx, err := install.IndexOf(&cm); err == nil {
			applied = idx.Version
		}
	}
	a.lastSeen = applied
	now := time.Now().UTC()
	doc := agentapi.Empty(now, agentapi.AgentStatus{
		Version: "test", StartedAt: a.started, AsteriskUp: true, AsteriskVersion: "22.11.0",
		AppliedVersion: applied, LastApplyVersion: applied,
	})
	doc.UnknownDevices = []agentapi.UnknownDevice{a.unknown}
	_ = json.NewEncoder(w).Encode(doc)
}

func (a *fakeAgent) counts() (ok, refused int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authOK, a.refused
}

// fakeBridge is the stream bridge's control API as the controller sees it:
// GET /v1/observations reports every stream of the compiled
// <fullname>-streams as loaded and idle, as the bridge does once the
// kubelet has synced the mounted ConfigMap.
type fakeBridge struct {
	namespace string
	names     install.Names
	started   time.Time

	mu    sync.Mutex
	polls int
}

func (b *fakeBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != bridgeapi.PathObservations {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	b.mu.Lock()
	b.polls++
	b.mu.Unlock()
	now := time.Now().UTC()
	doc := bridgeapi.Observations{
		GeneratedAt: now,
		Bridge:      bridgeapi.BridgeStatus{Version: "test", StartedAt: b.started, FFmpeg: "ffmpeg version test"},
		Streams:     []bridgeapi.StreamObservations{},
		Pools:       []bridgeapi.PoolObservations{},
	}
	if compiled, err := install.ReadStreams(r.Context(), testClient, b.namespace, b.names); err == nil {
		doc.Bridge.StreamsVersion = compiled.Version
		doc.Bridge.StreamsLoadedAt = &now
		for id, s := range compiled.Streams {
			doc.Streams = append(doc.Streams, bridgeapi.StreamObservations{
				UUID: id, Name: s.Name, Number: s.Number, Source: s.Source, Pool: s.Pool,
			})
		}
		for _, p := range compiled.Pools {
			doc.Pools = append(doc.Pools, bridgeapi.PoolObservations{Name: p.Name, Max: p.Max, Sessions: []bridgeapi.PoolSession{}})
		}
	} else {
		doc.Bridge.StreamsError = err.Error()
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (b *fakeBridge) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.polls
}

// TestSetupWiresEverything runs the whole controller, as cmd/patchbay
// builds it, against envtest, a fake agent and a fake stream bridge: it
// mints the install's Secrets, compiles a Phone into pbx-secrets and
// pbx-config, polls the agent with the token it minted, decides the Phone
// Unseen from that document, reports the agent's unknown phone, compiles a
// Stream into <fullname>-streams, polls the bridge and decides the Stream
// Idle, and counts the Phone and the Stream in its metrics.
func TestSetupWiresEverything(t *testing.T) {
	needEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)
	const fullname = "patchbay"
	names := install.NewNames(fullname)

	// The chart's default settings (test/chart checks the chart renders
	// exactly these).
	s, err := settings.Load(filepath.Join("settings", "testdata", "chart-defaults.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}

	agent := &fakeAgent{t: t, namespace: ns, names: names, started: time.Now().UTC().Truncate(time.Second),
		unknown: agentapi.UnknownDevice{
			MAC: "0200E2E0ABCD", Source: "10.244.0.99", File: "SEP0200E2E0ABCD.cnf.xml",
			FirstSeen: time.Now().UTC(), LastSeen: time.Now().UTC(),
		}}
	ts := httptest.NewServer(agent)
	t.Cleanup(ts.Close)
	bridge := &fakeBridge{namespace: ns, names: names, started: time.Now().UTC().Truncate(time.Second)}
	bs := httptest.NewServer(bridge)
	t.Cleanup(bs.Close)

	opts := controller.Options{
		Namespace: ns, MetricsAddr: "0", ProbeAddr: "0",
		Fullname: fullname, AgentURL: ts.URL, StreamBridgeURL: bs.URL, Registerer: prometheus.NewRegistry(),
	}
	registry := opts.Registerer.(*prometheus.Registry)
	mgr, err := controller.NewManager(testCfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Setup(mgr, opts, s, reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	mctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager stopped with error: %v", err)
		}
	})

	phone := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "desk", Namespace: ns},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100", Label: "Desk"},
	}
	if err := testClient.Create(ctx, phone); err != nil {
		t.Fatal(err)
	}
	radio := &v1alpha1.Stream{
		ObjectMeta: metav1.ObjectMeta{Name: "radio", Namespace: ns},
		Spec:       v1alpha1.StreamSpec{Number: "750", Name: "Radio", Source: "https://radio.invalid/live-128-mp3"},
	}
	if err := testClient.Create(ctx, radio); err != nil {
		t.Fatal(err)
	}
	id := drivers.DeviceID(drivers.KindPhone, "desk")

	// The bundle: pbx-secrets and pbx-config at one version, the Phone in
	// its index, the controller's Secrets labelled as its own.
	var compiled install.Compiled
	eventually(t, "the Phone compiled into pbx-config and pbx-secrets", func() (bool, string) {
		c, err := install.ReadCompiled(ctx, testClient, ns, names)
		if err != nil {
			return false, err.Error()
		}
		if _, ok := c.Device(id); !ok || c.Secrets == nil {
			return false, fmt.Sprintf("index %v, secrets at this version: %v", c.Index.Devices, c.Secrets != nil)
		}
		compiled = c
		return true, ""
	})
	for _, name := range []string{names.AgentTokenSecret, names.AMISecret, compile.DeviceSecretName(id), names.ProjectedSecret} {
		var sec corev1.Secret
		if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sec); err != nil {
			t.Fatalf("Secret %s: %v", name, err)
		}
		for k, v := range install.ManagedLabels() {
			if sec.Labels[k] != v {
				t.Errorf("Secret %s has labels %v, want %s=%s", name, sec.Labels, k, v)
			}
		}
	}

	// The poller asked the agent with the minted token, and the Phone was
	// decided from what it answered.
	eventually(t, "the Phone decided Unseen", func() (bool, string) {
		var p v1alpha1.Phone
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(phone), &p); err != nil {
			return false, err.Error()
		}
		ready := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady)
		return p.Status.State == string(drivers.StateUnseen) && ready != nil && ready.Reason == string(drivers.StateUnseen),
			fmt.Sprintf("state %q, ready %+v", p.Status.State, ready)
	})
	if ok, refused := agent.counts(); ok == 0 || refused > 1 {
		// One refusal is allowed: a poll may race the token's creation.
		t.Errorf("the agent saw %d requests with the minted token and %d without", ok, refused)
	}
	agent.mu.Lock()
	applied := agent.lastSeen
	agent.mu.Unlock()
	if applied != compiled.Index.Version {
		t.Logf("the agent last reported %s applied; compiled %s (a later poll catches up)", applied, compiled.Index.Version)
	}

	// Discovery: the unknown phone, once, regarding pbx-config.
	eventually(t, "an UnknownPhone Event on pbx-config", func() (bool, string) {
		var l eventsv1.EventList
		if err := testClient.List(ctx, &l, client.InNamespace(ns)); err != nil {
			return false, err.Error()
		}
		var seen []string
		for _, e := range l.Items {
			seen = append(seen, e.Reason+"/"+e.Regarding.Kind+"/"+e.Regarding.Name)
			// The API server takes the related reference naming the MAC.
			if e.Reason == install.EventReasonUnknownPhone && e.Regarding.Name == names.ConfigMap &&
				e.Note == install.UnknownPhoneMessage(agent.unknown.MAC, agent.unknown.Source) &&
				e.Related != nil && *e.Related == *install.UnknownPhoneRelated(ns, agent.unknown.MAC) {
				return true, ""
			}
		}
		return false, fmt.Sprint(seen)
	})

	// The Stream: compiled into <fullname>-streams (before pbx-config, which
	// holds its number), then decided Idle from the bridge's observations.
	eventually(t, "the Stream compiled into "+names.StreamsConfigMap, func() (bool, string) {
		doc, err := install.ReadStreams(ctx, testClient, ns, names)
		if err != nil {
			return false, err.Error()
		}
		s, ok := doc.Streams[streams.UUID(radio.Name)]
		return ok && s.Source == radio.Spec.Source && s.Number == "750", fmt.Sprintf("%+v", doc)
	})
	eventually(t, "the Stream decided Idle", func() (bool, string) {
		var s v1alpha1.Stream
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(radio), &s); err != nil {
			return false, err.Error()
		}
		ready := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionReady)
		return s.Status.State == "Idle" && ready != nil && ready.Status == metav1.ConditionTrue && ready.Reason == v1alpha1.ReasonIdle &&
				ready.ObservedGeneration == s.Generation,
			fmt.Sprintf("state %q, ready %+v", s.Status.State, ready)
	})
	if bridge.count() == 0 {
		t.Error("the bridge was never polled")
	}

	// The metrics: the Phone by state, the config version, the agent.
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	text := rec.Body.String()
	for _, want := range []string{
		`patchbay_devices{kind="Phone",state="Unseen"} 1`,
		`patchbay_config_version_info{version="` + compiled.Index.Version + `"} 1`,
		`patchbay_agent_reachable 1`,
		`patchbay_streams{state="Idle"} 1`,
		`patchbay_stream_bridge_reachable 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %s:\n%s", want, text)
		}
	}
}

// TestSetupRefusesBadInput: Setup checks what cmd/patchbay hands it before
// it wires anything.
func TestSetupRefusesBadInput(t *testing.T) {
	s, err := settings.Load(filepath.Join("settings", "testdata", "chart-defaults.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	other := s
	other.AMISecret = compile.SecretRef{Name: "someone-else", Key: "password"}
	for _, tc := range []struct {
		name string
		opts controller.Options
		s    compile.Settings
		reg  *drivers.Registry
		want string
	}{
		{"no fullname", controller.Options{Namespace: "ns"}, s, reg, "fullname"},
		{"a fullname too long", controller.Options{Namespace: "ns", Fullname: strings.Repeat("p", install.MaxFullnameLength+1)}, s, reg, "fullname"},
		{"settings of another install", controller.Options{Namespace: "ns", Fullname: "patchbay"}, other, reg, "settings"},
		{"no namespace", controller.Options{Fullname: "patchbay"}, s, reg, "namespace"},
		{"no registry", controller.Options{Namespace: "ns", Fullname: "patchbay"}, s, nil, "registry"},
		{"a stream bridge URL that is not one", controller.Options{Namespace: "ns", Fullname: "patchbay", StreamBridgeURL: "patchbay-streams:9093"}, s, reg, "--stream-bridge-url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Setup fails before it touches the manager, so nil is safe.
			err := controller.Setup(nil, tc.opts, tc.s, tc.reg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Setup = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}

// eventually polls cond every 200 ms for up to 60 s.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		ok, why := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 60 s: %s", what, why)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestSetupMetricsRegisteredOnce: a second Setup in one process, on the
// same registry, fails on its metrics rather than serving two sets.
func TestSetupMetricsRegisteredOnce(t *testing.T) {
	needEnvtest(t)
	s, err := settings.Load(filepath.Join("settings", "testdata", "chart-defaults.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	opts := controller.Options{Namespace: newNamespace(t), MetricsAddr: "0", ProbeAddr: "0",
		Fullname: "patchbay", Registerer: prometheus.NewRegistry()}
	if _, err := metrics.New(metrics.Config{Registerer: opts.Registerer}); err != nil {
		t.Fatal(err)
	}
	mgr, err := controller.NewManager(testCfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Setup(mgr, opts, s, reg); err == nil || !strings.Contains(err.Error(), "metrics") {
		t.Fatalf("Setup = %v, want a metrics error", err)
	}
}
