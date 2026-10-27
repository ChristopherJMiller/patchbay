package poller_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentclient"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/controller/poller"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

const ns = "telephony"

var (
	names     = install.NewNames("patchbay")
	deskID    = drivers.DeviceID(drivers.KindPhone, "desk")
	kitchenID = drivers.DeviceID(drivers.KindPhone, "kitchen")
	laptopID  = drivers.DeviceID(drivers.KindSoftphone, "laptop")
	// Every compiled device, as Changes delivers them.
	everyone = []string{"Phone/desk", "Phone/kitchen", "Softphone/laptop"}
)

var (
	_ agentview.Observer             = (*poller.Poller)(nil)
	_ manager.LeaderElectionRunnable = (*poller.Poller)(nil)
	_ manager.Runnable               = (*poller.Poller)(nil)
	_ poller.Source                  = (*agentclient.Client)(nil)
)

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

// compiledConfigMap is pbx-config for desk, kitchen and laptop.
func compiledConfigMap(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	phone := func(name, mac, number string) v1alpha1.Phone {
		return v1alpha1.Phone{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: v1alpha1.MAC(mac), Number: v1alpha1.Number(number)},
		}
	}
	in := compile.Inputs{
		Phones: []v1alpha1.Phone{phone("desk", "0200E2E00001", "100"), phone("kitchen", "0200E2E00002", "101")},
		Softphones: []v1alpha1.Softphone{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "laptop"},
			Spec:       v1alpha1.SoftphoneSpec{Number: "150"},
		}},
		Secrets: map[compile.SecretRef][]byte{
			compile.PasswordRef(deskID):            []byte("notAsecret01"),
			compile.PasswordRef(kitchenID):         []byte("notAsecret02"),
			compile.PasswordRef(laptopID):          []byte("notAsecretnotAsecret0003"),
			compile.ProvisioningTokenRef(laptopID): []byte("notAsecretToken"),
			names.AMIRef():                         []byte("notAsecretAMI"),
			names.PushRef():                        []byte("notAsecretPush"),
		},
	}
	b, err := compile.Compile(reg, in, compile.Settings{
		PBXAddress:      "192.168.0.240",
		SIPPort:         5060,
		RTPPorts:        compile.PortRange{First: 10000, Last: 10099},
		DeviceHTTPPort:  8088,
		NTPServer:       "192.168.20.1",
		Timezone:        "UTC",
		Signalling:      drivers.SignallingPJSIP,
		NumberPlan:      compile.DefaultNumberPlan(),
		AllowedNetworks: []string{"192.168.20.0/24"},
		AMISecret:       names.AMIRef(),
		PushSecret:      names.PushRef(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := install.ConfigMap(names, ns, b)
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

// source is a fake agent: it answers with doc, or err.
type source struct {
	mu    sync.Mutex
	doc   agentapi.Observations
	err   error
	block bool // wait for the context to end
	calls int
}

func (s *source) Observations(ctx context.Context) (agentapi.Observations, error) {
	s.mu.Lock()
	s.calls++
	block, doc, err := s.block, s.doc, s.err
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return agentapi.Observations{}, ctx.Err()
	}
	if err != nil {
		return agentapi.Observations{}, err
	}
	// GeneratedAt changes on every answer, as the agent's does.
	doc.GeneratedAt = time.Now()
	return doc, nil
}

func (s *source) set(fn func(doc *agentapi.Observations)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.doc)
	s.err = nil
}

func (s *source) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *source) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type fixture struct {
	src     *source
	holder  *agentview.Holder
	changes agentview.Changes
	p       *poller.Poller
	now     time.Time
	log     *logSink
}

type logSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logSink) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.b.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 10})
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func baseDoc() agentapi.Observations {
	return agentapi.Observations{
		Agent: agentapi.AgentStatus{AppliedVersion: "v1", AsteriskUp: true, StartedAt: time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)},
		Devices: []agentapi.DeviceObservations{
			{Device: deskID, MAC: "0200E2E00001"},
			{Device: laptopID},
		},
		Firmware: agentapi.FirmwareStore{Files: []agentapi.FirmwareFileStatus{{Load: "L", Name: "a.sbn", Present: false}}},
	}
}

func newFixture(t *testing.T, mutate func(*poller.Config), objs ...client.Object) *fixture {
	t.Helper()
	if objs == nil {
		objs = []client.Object{compiledConfigMap(t)}
	}
	f := &fixture{
		src:     &source{doc: baseDoc()},
		holder:  &agentview.Holder{},
		changes: agentview.NewChanges(agentview.DefaultChangesBuffer),
		now:     time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
		log:     &logSink{},
	}
	cfg := poller.Config{
		Source:    f.src,
		Reader:    fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build(),
		Namespace: ns,
		Names:     names,
		Interval:  50 * time.Millisecond,
		Holder:    f.holder,
		Changes:   f.changes,
		Now:       func() time.Time { return f.now },
		Log:       f.log.logger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := poller.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	return f
}

// enqueued drains both channels: "Kind/name", sorted. Every event must
// carry the namespace and nothing but the name.
func (f *fixture) enqueued(t *testing.T) []string {
	t.Helper()
	var out []string
	for {
		select {
		case e := <-f.changes.Phones:
			check(t, e.Object.ObjectMeta)
			out = append(out, "Phone/"+e.Object.Name)
		case e := <-f.changes.Softphones:
			check(t, e.Object.ObjectMeta)
			out = append(out, "Softphone/"+e.Object.Name)
		default:
			slices.Sort(out)
			return out
		}
	}
}

func check(t *testing.T, m metav1.ObjectMeta) {
	t.Helper()
	if m.Namespace != ns {
		t.Errorf("enqueued %s in namespace %q, want %q", m.Name, m.Namespace, ns)
	}
}

func (f *fixture) poll(t *testing.T) []string {
	t.Helper()
	f.p.Poll(context.Background())
	return f.enqueued(t)
}

func TestFirstPollEnqueuesAll(t *testing.T) {
	f := newFixture(t, nil)
	if got := f.poll(t); !slices.Equal(got, everyone) {
		t.Fatalf("first poll enqueued %v, want %v", got, everyone)
	}
	s := f.p.Snapshot()
	if !s.AgentReachable || !s.PolledAt.Equal(f.now) || s.Polls != 1 || s.LastError != "" ||
		s.Document.Agent.AppliedVersion != "v1" || len(s.Document.Devices) != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
	if f.holder.Snapshot().Polls != 1 {
		t.Error("the snapshot was not published to the Holder")
	}
}

func TestUnchangedEnqueuesNothing(t *testing.T) {
	f := newFixture(t, nil)
	f.poll(t)
	f.now = f.now.Add(5 * time.Second)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("an unchanged document enqueued %v", got)
	}
	if s := f.p.Snapshot(); s.Polls != 2 || !s.PolledAt.Equal(f.now) {
		t.Errorf("snapshot after the second poll: Polls %d PolledAt %s", s.Polls, s.PolledAt)
	}
}

func TestDeviceChangeEnqueuesOne(t *testing.T) {
	f := newFixture(t, nil)
	f.poll(t)

	f.src.set(func(d *agentapi.Observations) { d.Devices[0].Calls = 1 })
	if got := f.poll(t); !slices.Equal(got, []string{"Phone/desk"}) {
		t.Fatalf("desk's call enqueued %v, want only desk", got)
	}
	f.src.set(func(d *agentapi.Observations) {
		d.Devices = append(d.Devices, agentapi.DeviceObservations{Device: kitchenID, Fetches: []agentapi.Fetch{{File: "SEP0200E2E00002.cnf.xml"}}})
	})
	if got := f.poll(t); !slices.Equal(got, []string{"Phone/kitchen"}) {
		t.Fatalf("kitchen appearing enqueued %v, want only kitchen", got)
	}
	f.src.set(func(d *agentapi.Observations) { d.Devices = d.Devices[1:] })
	if got := f.poll(t); !slices.Equal(got, []string{"Phone/desk"}) {
		t.Fatalf("desk disappearing enqueued %v, want only desk", got)
	}
	// Unknown devices are discovery's, not a device change.
	f.src.set(func(d *agentapi.Observations) {
		d.UnknownDevices = []agentapi.UnknownDevice{{MAC: "0200E2E0FFFF", Source: "192.168.20.50"}}
	})
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("an unknown device enqueued %v", got)
	}
}

func TestAgentChangeEnqueuesAll(t *testing.T) {
	for name, change := range map[string]func(d *agentapi.Observations){
		"AsteriskUp":     func(d *agentapi.Observations) { d.Agent.AsteriskUp = false },
		"AppliedVersion": func(d *agentapi.Observations) { d.Agent.AppliedVersion = "v2" },
		"LastApplyError": func(d *agentapi.Observations) { d.Agent.LastApplyVersion, d.Agent.LastApplyError = "v3", "verify" },
		"StartedAt":      func(d *agentapi.Observations) { d.Agent.StartedAt = d.Agent.StartedAt.Add(time.Hour) },
		"firmware file":  func(d *agentapi.Observations) { d.Firmware.Files[0].Present = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.poll(t)
			f.src.set(change)
			if got := f.poll(t); !slices.Equal(got, everyone) {
				t.Fatalf("enqueued %v, want every device %v", got, everyone)
			}
			if got := f.poll(t); len(got) != 0 {
				t.Fatalf("the poll after enqueued %v, want nothing", got)
			}
		})
	}
}

func TestFailureKeepsDocument(t *testing.T) {
	f := newFixture(t, nil)
	f.poll(t)
	good := f.p.Snapshot()

	f.now = f.now.Add(5 * time.Second)
	f.src.fail(errors.New("dial tcp 10.0.0.9:9440: connection refused"))
	if got := f.poll(t); !slices.Equal(got, everyone) {
		t.Fatalf("the first failure enqueued %v, want every device", got)
	}
	s := f.p.Snapshot()
	if s.AgentReachable || s.Polls != 2 || !s.PolledAt.Equal(good.PolledAt) ||
		s.Document.Agent.AppliedVersion != "v1" || len(s.Document.Devices) != 2 ||
		!strings.Contains(s.LastError, "connection refused") {
		t.Fatalf("snapshot after a failure = %+v; want the last good document and PolledAt, unreachable, Polls 2", s)
	}

	f.now = f.now.Add(5 * time.Second)
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("a second failure enqueued %v, want nothing", got)
	}
	if s := f.p.Snapshot(); s.Polls != 3 || s.AgentReachable || !s.PolledAt.Equal(good.PolledAt) {
		t.Fatalf("snapshot after a second failure = %+v", s)
	}
	if n := strings.Count(f.log.String(), "polling the agent failed"); n != 1 {
		t.Errorf("the same failure was logged %d times, want once:\n%s", n, f.log)
	}
}

func TestRecoveryEnqueuesAll(t *testing.T) {
	f := newFixture(t, nil)
	f.poll(t)
	f.src.fail(errors.New("timeout"))
	f.poll(t)
	f.src.set(func(*agentapi.Observations) {})
	f.now = f.now.Add(time.Minute)
	if got := f.poll(t); !slices.Equal(got, everyone) {
		t.Fatalf("recovery enqueued %v, want every device", got)
	}
	if s := f.p.Snapshot(); !s.AgentReachable || s.LastError != "" || !s.PolledAt.Equal(f.now) {
		t.Fatalf("snapshot after recovery = %+v", s)
	}
	if !strings.Contains(f.log.String(), "answers again") {
		t.Errorf("the recovery was not logged:\n%s", f.log)
	}
}

func TestFirstPollFailing(t *testing.T) {
	f := newFixture(t, nil)
	f.src.fail(errors.New("no route to host"))
	if got := f.poll(t); !slices.Equal(got, everyone) {
		t.Fatalf("a failed first poll enqueued %v, want every device (decide them Unknown)", got)
	}
	s := f.p.Snapshot()
	if !s.Started() || s.AgentReachable || !s.PolledAt.IsZero() || len(s.Document.Devices) != 0 {
		t.Fatalf("snapshot = %+v; want Started, unreachable, no document", s)
	}
}

func TestOnSnapshotOnSuccessOnly(t *testing.T) {
	var calls []string
	f := newFixture(t, func(c *poller.Config) {
		c.OnSnapshot = []agentview.SnapshotFunc{
			func(_ context.Context, s agentview.Snapshot) {
				calls = append(calls, "first:"+s.Document.Agent.AppliedVersion)
			},
			func(_ context.Context, s agentview.Snapshot) {
				if !s.AgentReachable {
					t.Error("OnSnapshot got an unreachable snapshot")
				}
				calls = append(calls, "second")
			},
		}
	})
	f.poll(t)
	f.src.fail(errors.New("refused"))
	f.poll(t)
	f.src.set(func(d *agentapi.Observations) { d.Agent.AppliedVersion = "v2" })
	f.poll(t)
	want := []string{"first:v1", "second", "first:v2", "second"}
	if !slices.Equal(calls, want) {
		t.Fatalf("OnSnapshot calls %v, want %v", calls, want)
	}
}

func TestUnknownIDSkipped(t *testing.T) {
	f := newFixture(t, nil)
	f.src.set(func(d *agentapi.Observations) {
		d.Devices = append(d.Devices, agentapi.DeviceObservations{Device: "phone-ghost"})
	})
	f.poll(t)
	f.src.set(func(d *agentapi.Observations) { d.Devices[2].Calls = 2 })
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("a device the index lacks enqueued %v", got)
	}
}

func TestNotCompiled(t *testing.T) {
	f := newFixture(t, nil, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "unrelated"}})
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("enqueued %v with nothing compiled", got)
	}
	if s := f.p.Snapshot(); !s.Started() || !s.AgentReachable {
		t.Fatalf("snapshot not published: %+v", s)
	}
	if s := f.log.String(); strings.Contains(s, "error") {
		t.Errorf("nothing compiled was logged as an error:\n%s", s)
	}
}

func TestReadErrorRetriesNextPoll(t *testing.T) {
	var failing sync.Mutex
	broken := true
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(compiledConfigMap(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				failing.Lock()
				defer failing.Unlock()
				if broken {
					return errors.New("cache not synced")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	f := newFixture(t, func(c *poller.Config) { c.Reader = reader })
	if got := f.poll(t); len(got) != 0 {
		t.Fatalf("enqueued %v without an index", got)
	}
	failing.Lock()
	broken = false
	failing.Unlock()
	if got := f.poll(t); !slices.Equal(got, everyone) {
		t.Fatalf("the next poll enqueued %v, want every device (the first attempt's enqueue never finished)", got)
	}
}

func TestBlockedChangesDoNotWedge(t *testing.T) {
	// Nobody reads unbuffered channels: the enqueue gives up after twice
	// the interval, and the next poll tries the same devices again.
	unbuffered := agentview.NewChanges(0)
	f := newFixture(t, func(c *poller.Config) { c.Changes = unbuffered })
	start := time.Now()
	f.p.Poll(context.Background())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Poll blocked %s on a full Changes channel", d)
	}
	got := make(chan string, 10)
	go func() {
		e := <-unbuffered.Phones
		got <- e.Object.Name
	}()
	f.p.Poll(context.Background())
	select {
	case name := <-got:
		if name != "desk" {
			t.Errorf("enqueued %s first, want desk", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the next poll did not enqueue again")
	}
}

func TestHungAgentTimesOut(t *testing.T) {
	f := newFixture(t, nil)
	f.src.block = true
	start := time.Now()
	f.p.Poll(context.Background())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Poll of a hung agent took %s; want about twice the 50ms interval", d)
	}
	if s := f.p.Snapshot(); s.AgentReachable || !strings.Contains(s.LastError, "deadline") {
		t.Fatalf("snapshot = %+v, want unreachable with a deadline error", s)
	}
}

func TestCancelledPollPublishesNothing(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.p.Poll(ctx)
	if s := f.p.Snapshot(); s.Started() {
		t.Fatalf("a poll cut short by shutdown published %+v", s)
	}
}

func TestStartPollsAtOnceAndStops(t *testing.T) {
	f := newFixture(t, func(c *poller.Config) { c.Interval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.p.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for f.src.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.src.callCount() != 1 {
		t.Fatalf("Start made %d polls before its first tick, want 1 at once", f.src.callCount())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop when its context ended")
	}

	// And it keeps polling every Interval.
	f = newFixture(t, func(c *poller.Config) { c.Interval = 10 * time.Millisecond })
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- f.p.Start(ctx) }()
	deadline = time.Now().Add(5 * time.Second)
	for f.src.callCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if f.src.callCount() < 3 {
		t.Fatalf("%d polls, want at least 3", f.src.callCount())
	}
}

func TestNeedsLeaderElection(t *testing.T) {
	if !newFixture(t, nil).p.NeedLeaderElection() {
		t.Fatal("NeedLeaderElection = false, want true: only the leader may poll and enqueue")
	}
}

func TestMetricsObserveEverySnapshot(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := metrics.New(metrics.Config{Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(c *poller.Config) { c.Metrics = m })
	at := time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC)
	f.src.set(func(d *agentapi.Observations) {
		d.Agent.LastApplyVersion, d.Agent.LastApplyError, d.Agent.LastApplyAt = "v9", "verify: kitchen missing", &at
	})
	f.poll(t)
	f.poll(t)
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), metrics.NameApplyFailures+" 1") {
		t.Fatalf("metrics after two polls of one failed apply:\n%s", rec.Body)
	}
}

// TestAgainstHTTPAgent runs the poller on the real client against an
// httptest agent that refuses the token and echoes it: the snapshot's
// error never holds it.
func TestAgainstHTTPAgent(t *testing.T) {
	const tok = "not-a-secret-agent-token"
	ok := true
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		good := ok
		mu.Unlock()
		if !good || r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"token ` + r.Header.Get("Authorization") + ` refused"}`))
			return
		}
		_, _ = w.Write([]byte(`{"agent":{"appliedVersion":"v1","asteriskUp":true},"devices":[{"device":"phone-desk"}]}`))
	}))
	defer srv.Close()
	c, err := agentclient.New(agentclient.Config{URL: srv.URL, Tokens: staticTokens(tok)})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(cfg *poller.Config) { cfg.Source = c })
	if got := f.poll(t); !slices.Equal(got, everyone) {
		t.Fatalf("enqueued %v", got)
	}
	if s := f.p.Snapshot(); !s.AgentReachable || s.Device(deskID) == nil {
		t.Fatalf("snapshot = %+v", s)
	}
	mu.Lock()
	ok = false
	mu.Unlock()
	f.poll(t)
	s := f.p.Snapshot()
	if s.AgentReachable || !strings.Contains(s.LastError, "401") || strings.Contains(s.LastError, tok) {
		t.Fatalf("LastError = %q: want a 401 without the token", s.LastError)
	}
	if strings.Contains(f.log.String(), tok) {
		t.Fatalf("the token appears in the log:\n%s", f.log)
	}
}

type staticTokens string

func (s staticTokens) Tokens(context.Context) (string, string, error) { return string(s), "", nil }

func TestNewValidates(t *testing.T) {
	good := func() poller.Config {
		return poller.Config{
			Source: &source{}, Reader: fake.NewClientBuilder().Build(), Namespace: ns, Names: names,
			Holder: &agentview.Holder{}, Changes: agentview.NewChanges(1),
		}
	}
	if _, err := poller.New(good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*poller.Config){
		"no source":    func(c *poller.Config) { c.Source = nil },
		"no reader":    func(c *poller.Config) { c.Reader = nil },
		"no namespace": func(c *poller.Config) { c.Namespace = "" },
		"bad names":    func(c *poller.Config) { c.Names = install.NewNames("") },
		"no holder":    func(c *poller.Config) { c.Holder = nil },
		"no changes":   func(c *poller.Config) { c.Changes = agentview.Changes{} },
		"bad interval": func(c *poller.Config) { c.Interval = -time.Second },
	} {
		cfg := good()
		mutate(&cfg)
		if _, err := poller.New(cfg); err == nil {
			t.Errorf("New with %s passed", name)
		}
	}
}
