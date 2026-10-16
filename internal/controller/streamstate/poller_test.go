package streamstate_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/controller/streamstate"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

// drain is every Stream name the poller has enqueued so far, sorted.
func drain(p *streamstate.Poller) []string {
	var out []string
	for {
		select {
		case ev := <-p.Changes():
			if ev.Object.Namespace != ns {
				return append(out, "wrong namespace "+ev.Object.Namespace)
			}
			out = append(out, ev.Object.Name)
		default:
			slices.Sort(out)
			return out
		}
	}
}

func reachableMetric(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, metrics.NameStreamBridgeReachable+" "); ok {
			return v
		}
	}
	return ""
}

// TestPollerEnqueues: the first poll enqueues every Stream, and so does a
// change of reachability, of the bridge's streams.json version or start,
// or of the pools; otherwise only the Streams whose observation changed.
// A failed poll keeps the last good document.
func TestPollerEnqueues(t *testing.T) {
	sch, err := ctrltest.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	listFails := false
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		streamObject("king5", "705", kingSource), streamObject("kiro7", "707", kiroSource),
		streamObject("fox13", "713", foxSource), streamObject("groovesalad", "750", radioSource),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if listFails {
				return errors.New("cache not started")
			}
			return cl.List(ctx, l, opts...)
		},
	}).Build()
	reg := prometheus.NewRegistry()
	m, err := metrics.New(metrics.Config{Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	b := &bridge{}
	d := house(t)
	b.set(loaded(d), nil)
	now := t0
	r, err := streamstate.New(streamstate.Config{
		Client: c, Namespace: ns, Names: names, Settings: testSettings(), Bridge: b, Recorder: &recorder{},
		Metrics: m, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Poller()
	ctx := context.Background()
	all := []string{"fox13", "groovesalad", "king5", "kiro7"}
	step := func(what string, want []string, edit func()) streamstate.Snapshot {
		t.Helper()
		edit()
		now = now.Add(5 * time.Second)
		p.Poll(ctx)
		if got := drain(p); !slices.Equal(got, want) {
			t.Errorf("%s: enqueued %v, want %v", what, got, want)
		}
		return p.Snapshot()
	}
	if s := p.Snapshot(); s.Started() || reachableMetric(t, reg) != "" {
		t.Fatalf("before the first poll: %+v", s)
	}

	s := step("the first poll", all, func() {})
	if !s.Started() || !s.Reachable || s.Polls != 1 || !s.PolledAt.Equal(now) || s.LastError != "" || s.Document.Bridge.StreamsVersion != d.Version {
		t.Errorf("snapshot %+v", s)
	}
	if v := reachableMetric(t, reg); v != "1" {
		t.Errorf("reachable metric %q, want 1", v)
	}
	step("nothing changed", nil, func() {})
	step("king5 has a listener", []string{"king5"}, func() {
		b.edit(func(o *bridgeapi.Observations) { stream(o, "king5").Listeners = 1 })
	})
	step("kiro7 failed and fox13 played", []string{"fox13", "kiro7"}, func() {
		b.edit(func(o *bridgeapi.Observations) {
			stream(o, "kiro7").LastFailure = &bridgeapi.Failure{At: t0, Reason: bridgeapi.FailureSourceFailed}
			stream(o, "fox13").LastPlayed = at(t0)
		})
	})
	step("groovesalad is gone from the bridge's document", []string{"groovesalad"}, func() {
		b.edit(func(o *bridgeapi.Observations) {
			o.Streams = slices.DeleteFunc(o.Streams, func(s bridgeapi.StreamObservations) bool { return s.Name == "groovesalad" })
		})
	})
	step("a session in the pool", all, func() {
		b.edit(func(o *bridgeapi.Observations) {
			session(o, kingSource, bridgeapi.SessionPlaying, 1, "king5")
		})
	})
	good := p.Snapshot()

	s = step("the bridge stops answering", all, func() { b.set(good.Document, errors.New("bridgeclient: connection refused")) })
	if s.Reachable || s.LastError != "bridgeclient: connection refused" || !s.PolledAt.Equal(good.PolledAt) ||
		s.Document.Bridge.StreamsVersion != good.Document.Bridge.StreamsVersion || len(s.Document.Pools[0].Sessions) != 1 || s.Polls != good.Polls+1 {
		t.Errorf("after a failure: %+v; want the last good document and its time, unreachable", s)
	}
	if v := reachableMetric(t, reg); v != "0" {
		t.Errorf("reachable metric %q, want 0", v)
	}
	step("it still does not", nil, func() {})
	s = step("it answers again", all, func() { b.set(good.Document, nil) })
	if !s.Reachable || s.LastError != "" || !s.PolledAt.Equal(now) {
		t.Errorf("recovered: %+v", s)
	}
	step("it loaded a new streams.json", all, func() {
		b.edit(func(o *bridgeapi.Observations) { o.Bridge.StreamsVersion = strings.Repeat("b", 64) })
	})
	step("it restarted", all, func() {
		b.edit(func(o *bridgeapi.Observations) { o.Bridge.StartedAt = now })
	})

	// The Streams cannot be listed: nothing is committed, so the next
	// poll enqueues them all.
	listFails = true
	step("a change the cache cannot list for", nil, func() {
		b.edit(func(o *bridgeapi.Observations) { o.Pools[0].Sessions = nil })
	})
	listFails = false
	step("the cache is back", all, func() {})

	// A poll cut short by its context publishes nothing.
	before := p.Snapshot()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	p.Poll(cancelled)
	if after := p.Snapshot(); after.Polls != before.Polls {
		t.Errorf("a cancelled poll published %+v", after)
	}
	if !p.NeedLeaderElection() {
		t.Error("the poller runs on every replica")
	}
}

// TestPollerStart: Start polls at once and every Interval, until its
// context ends.
func TestPollerStart(t *testing.T) {
	sch, err := ctrltest.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).Build()
	b := &bridge{}
	b.set(loaded(house(t)), nil)
	r, err := streamstate.New(streamstate.Config{
		Client: c, Namespace: ns, Names: names, Settings: testSettings(), Bridge: b, Recorder: &recorder{},
		Interval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Poller().Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.Poller().Snapshot().Polls < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("%d polls after 5 s", r.Poller().Snapshot().Polls)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}
	if !r.Poller().Snapshot().Reachable {
		t.Error("not reachable")
	}
}
