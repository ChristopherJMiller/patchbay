package streamstate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/streamstate"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

const ns = "telephony"

// bridge is a fake stream bridge: Observations returns a copy of its
// document (through JSON, as bridgeclient decodes a fresh one) or its
// error.
type bridge struct {
	mu    sync.Mutex
	doc   bridgeapi.Observations
	err   error
	calls int
}

func (b *bridge) Observations(context.Context) (bridgeapi.Observations, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.err != nil {
		return bridgeapi.Observations{}, b.err
	}
	raw, err := json.Marshal(b.doc)
	if err != nil {
		return bridgeapi.Observations{}, err
	}
	var out bridgeapi.Observations
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (b *bridge) set(doc bridgeapi.Observations, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.doc, b.err = doc, err
}

func (b *bridge) edit(f func(o *bridgeapi.Observations)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(&b.doc)
}

// recorder keeps every Event as "<Type> <regarded name>@<resourceVersion>
// <Reason> <Action>: <note>".
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) Eventf(regarding, _ runtime.Object, eventType, reason, action, note string, args ...any) {
	name := ""
	if o, ok := regarding.(client.Object); ok {
		name = o.GetName() + "@" + o.GetResourceVersion()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("%s %s %s %s: %s", eventType, name, reason, action, fmt.Sprintf(note, args...)))
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// unit is one fake-client test: a namespace holding Streams, the compiled
// <fullname>-streams (unless noCompiled), a fake bridge, and the
// reconciler with its poller.
type unit struct {
	t            *testing.T
	ctx          context.Context
	c            client.Client
	bridge       *bridge
	rec          *recorder
	r            *streamstate.Reconciler
	statusWrites atomic.Int64
	statusErr    atomic.Pointer[error]
	now          time.Time
}

func newUnit(t *testing.T, s compile.Settings, withBridge bool, objs ...client.Object) *unit {
	t.Helper()
	sch, err := ctrltest.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	u := &unit{t: t, ctx: context.Background(), bridge: &bridge{}, rec: &recorder{}, now: t0}
	u.c = fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&v1alpha1.Stream{}).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				u.statusWrites.Add(1)
				if e := u.statusErr.Load(); e != nil {
					return *e
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	cfg := streamstate.Config{
		Client: u.c, Namespace: ns, Names: names, Settings: s, Recorder: u.rec,
		Now: func() time.Time { return u.now },
	}
	if withBridge {
		cfg.Bridge = u.bridge
	}
	if u.r, err = streamstate.New(cfg); err != nil {
		t.Fatal(err)
	}
	return u
}

func streamObject(name, number, source string) *v1alpha1.Stream {
	s := newStream(name, number, source)
	s.Namespace = ns
	return s
}

func compiledObject(t *testing.T, d streams.Document) *corev1.ConfigMap {
	t.Helper()
	cm, err := install.StreamsConfigMap(names, ns, d)
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

// houseUnit is king5 and kiro7 compiled, and the bridge playing nothing.
func houseUnit(t *testing.T) *unit {
	t.Helper()
	d := house(t)
	u := newUnit(t, testSettings(), true,
		streamObject("king5", "705", kingSource), streamObject("kiro7", "707", kiroSource), compiledObject(t, d))
	u.bridge.set(loaded(d), nil)
	return u
}

func (u *unit) poll() { u.r.Poller().Poll(u.ctx) }

func (u *unit) reconcile(name string) ctrl.Result {
	u.t.Helper()
	res, err := u.r.Reconcile(u.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		u.t.Fatalf("reconciling %s: %v", name, err)
	}
	return res
}

func (u *unit) stream(name string) *v1alpha1.Stream {
	u.t.Helper()
	var s v1alpha1.Stream
	if err := u.c.Get(u.ctx, client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
		u.t.Fatal(err)
	}
	return &s
}

func ready(s *v1alpha1.Stream) metav1.Condition {
	if c := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionReady); c != nil {
		return *c
	}
	return metav1.Condition{}
}

func TestReconcileWritesOnlyWhenChanged(t *testing.T) {
	u := houseUnit(t)
	u.poll()
	u.reconcile("king5")
	s := u.stream("king5")
	r := ready(s)
	if s.Status.State != streamstate.StateIdle || r.Status != metav1.ConditionTrue || r.Reason != v1alpha1.ReasonIdle ||
		r.ObservedGeneration != s.Generation || s.Status.ObservedGeneration != s.Generation || !r.LastTransitionTime.Equal(ptime(t0)) ||
		s.Status.TunerPool != "hdhomerun" {
		t.Fatalf("status %+v", s.Status)
	}
	if c := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionNumberConflict); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("NumberConflict %+v", c)
	}
	if s.Status.Conditions[0].Type != v1alpha1.ConditionReady {
		t.Errorf("conditions %+v, want Ready first", s.Status.Conditions)
	}
	if e := u.rec.take(); len(e) != 1 || !strings.HasPrefix(e[0], "Normal king5@") || !strings.HasSuffix(e[0], "StateChanged Decide: None -> Idle: nobody is listening") {
		t.Errorf("events %q", e)
	}
	if n := u.statusWrites.Load(); n != 1 {
		t.Fatalf("%d status writes, want 1", n)
	}

	// Judged again, polled again, later: nothing to write.
	u.now = t0.Add(10 * time.Second)
	u.poll()
	u.reconcile("king5")
	if n := u.statusWrites.Load(); n != 1 || u.stream("king5").ResourceVersion != s.ResourceVersion || len(u.rec.take()) != 0 {
		t.Errorf("judging again wrote %d times in all, or emitted", n)
	}

	// Two listeners: Playing, written once.
	u.bridge.edit(func(o *bridgeapi.Observations) { stream(o, "king5").Listeners = 2 })
	u.poll()
	u.reconcile("king5")
	u.reconcile("king5")
	s = u.stream("king5")
	if s.Status.State != streamstate.StatePlaying || s.Status.Listeners != 2 || u.statusWrites.Load() != 2 {
		t.Errorf("status %+v after %d writes; want Playing with 2 listeners, 2 writes", s.Status, u.statusWrites.Load())
	}
	if e := u.rec.take(); len(e) != 1 || !strings.HasSuffix(e[0], "Idle -> Playing: 2 listeners; tuner pool hdhomerun") ||
		!strings.Contains(e[0], "king5@"+s.ResourceVersion+" ") {
		t.Errorf("events %q; want one StateChanged regarding the written version %s", e, s.ResourceVersion)
	}
}

// TestEventsOncePerFailure: each failure the bridge reports is copied once
// and is one Event, however often it is polled and judged.
func TestEventsOncePerFailure(t *testing.T) {
	u := houseUnit(t)
	const msg = "ffmpeg exited with status 8: Server returned 5XX Server Error reply"
	u.bridge.edit(func(o *bridgeapi.Observations) {
		stream(o, "king5").LastFailure = &bridgeapi.Failure{At: t0.Add(-time.Minute + 400*time.Millisecond), Reason: bridgeapi.FailureSourceFailed, Message: msg}
	})
	u.poll()
	u.reconcile("king5")
	if e := u.rec.take(); len(e) != 2 || !strings.Contains(e[0], "None -> Failing") ||
		!strings.HasPrefix(e[1], "Warning king5@") || !strings.HasSuffix(e[1], "SourceFailed Play: SourceFailed: "+msg) {
		t.Fatalf("events %q; want StateChanged, then one SourceFailed Warning", e)
	}
	for range 3 {
		u.poll()
		u.reconcile("king5")
	}
	if e := u.rec.take(); len(e) != 0 || u.statusWrites.Load() != 1 {
		t.Fatalf("the same failure again: events %q, %d writes", e, u.statusWrites.Load())
	}

	// A new failure: one more Event, no state change.
	u.bridge.edit(func(o *bridgeapi.Observations) {
		stream(o, "king5").LastFailure = &bridgeapi.Failure{At: t0, Reason: bridgeapi.FailureSourceEnded, Message: "no audio for 10s"}
	})
	u.poll()
	u.reconcile("king5")
	u.reconcile("king5")
	if e := u.rec.take(); len(e) != 1 || !strings.HasSuffix(e[0], "SourceFailed Play: SourceEnded: no audio for 10s") {
		t.Errorf("events %q; want one SourceFailed", e)
	}

	// Refused while its source is still broken: nothing changes, no Event
	// (a refusal never hides a source failure; docs/plans/release-b.md §13).
	busy := "pool hdhomerun (1 at a time) is playing kiro7 (707)"
	u.bridge.edit(func(o *bridgeapi.Observations) {
		stream(o, "king5").LastFailure = &bridgeapi.Failure{At: t0.Add(time.Minute), Reason: bridgeapi.FailureTunerBusy, Message: busy}
	})
	u.poll()
	u.reconcile("king5")
	if e := u.rec.take(); len(e) != 0 {
		t.Errorf("a refusal of a failing stream: events %q, want none", e)
	}
	if st := u.stream("king5").Status; st.State != streamstate.StateFailing || st.LastFailure == nil || st.LastFailure.Reason != v1alpha1.StreamFailureSourceEnded {
		t.Errorf("a refusal of a failing stream: %s, lastFailure %+v; want Failing, SourceEnded kept", st.State, st.LastFailure)
	}

	// Played since, then refused: CallRefused, and Idle (a refusal is
	// history).
	u.bridge.edit(func(o *bridgeapi.Observations) {
		s := stream(o, "king5")
		s.LastPlayed = at(t0.Add(30 * time.Second))
		s.LastFailure = &bridgeapi.Failure{At: t0.Add(2 * time.Minute), Reason: bridgeapi.FailureTunerBusy, Message: busy}
	})
	u.poll()
	u.reconcile("king5")
	e := u.rec.take()
	if len(e) != 2 || !strings.Contains(e[0], "Failing -> Idle") || !strings.HasPrefix(e[1], "Normal king5@") || !strings.HasSuffix(e[1], "CallRefused Play: "+busy) {
		t.Errorf("events %q; want StateChanged to Idle, then CallRefused", e)
	}
	if f := u.stream("king5").Status.LastFailure; f == nil || f.Reason != v1alpha1.StreamFailureTunerBusy || f.Message != busy {
		t.Errorf("lastFailure %+v", f)
	}
}

// TestGatedUntilCompiled: nothing is written before <fullname>-streams
// exists, nor before the first poll has ended; with no stream bridge,
// Streams are Unavailable without any poll.
func TestGatedUntilCompiled(t *testing.T) {
	d := house(t)
	u := newUnit(t, testSettings(), true, streamObject("king5", "705", kingSource))
	u.bridge.set(loaded(d), nil)
	u.poll()
	u.reconcile("king5")
	if n := u.statusWrites.Load(); n != 0 || u.stream("king5").Status.State != "" {
		t.Fatalf("judged before <fullname>-streams existed: %d writes", n)
	}
	if err := u.c.Create(u.ctx, compiledObject(t, d)); err != nil {
		t.Fatal(err)
	}
	u.reconcile("king5")
	if s := u.stream("king5"); s.Status.State != streamstate.StateIdle {
		t.Errorf("once compiled: %+v", s.Status)
	}
	u.reconcile("gone") // a deleted Stream is done

	// Before the first poll: nothing.
	u = newUnit(t, testSettings(), true, streamObject("king5", "705", kingSource), compiledObject(t, d))
	u.reconcile("king5")
	if n := u.statusWrites.Load(); n != 0 {
		t.Errorf("judged before the first poll: %d writes", n)
	}

	// Streams disabled: Unavailable, with no poller at all.
	s := testSettings()
	s.StreamBridge = ""
	u = newUnit(t, s, true, streamObject("king5", "705", kingSource), compiledObject(t, d))
	if u.r.Poller() != nil {
		t.Error("a poller without a stream bridge")
	}
	u.reconcile("king5")
	if st := u.stream("king5"); st.Status.State != streamstate.StateUnavailable || ready(st).Reason != v1alpha1.ReasonNoStreamBridge {
		t.Errorf("disabled: %+v", st.Status)
	}

	// The settings name a bridge, but there is nothing to poll it with.
	u = newUnit(t, testSettings(), false, streamObject("king5", "705", kingSource), compiledObject(t, d))
	u.reconcile("king5")
	if st := u.stream("king5"); st.Status.State != streamstate.StateUnknown ||
		ready(st).Message != "cannot reach the stream bridge: the controller does not poll it (no stream bridge URL)" {
		t.Errorf("no poller: %+v", st.Status)
	}
}

// TestReconcileClaims: the number table comes from every resource, as
// Compile's does.
func TestReconcileClaims(t *testing.T) {
	d := house(t)
	older := streamObject("a-radio", "705", radioSource)
	older.CreationTimestamp = metav1.NewTime(t0.Add(-time.Hour))
	newer := streamObject("king5", "705", kingSource)
	newer.CreationTimestamp = metav1.NewTime(t0)
	u := newUnit(t, testSettings(), true, older, newer, streamObject("odd", "305", radioSource), compiledObject(t, d))
	u.bridge.set(loaded(d), nil)
	u.poll()
	u.reconcile("king5")
	s := u.stream("king5")
	const msg = "number 705 is held by Stream a-radio, which is older"
	nc := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionNumberConflict)
	if s.Status.State != streamstate.StateInvalid || ready(s).Reason != v1alpha1.ReasonNumberConflict || ready(s).Message != msg ||
		nc == nil || nc.Status != metav1.ConditionTrue || nc.Message != msg {
		t.Errorf("the newer Stream on 705: %+v", s.Status)
	}
	u.reconcile("odd")
	if s := u.stream("odd"); ready(s).Reason != v1alpha1.ReasonNumberNotInPlan || ready(s).Message != "number 305 is not in the Stream range 7XX" {
		t.Errorf("a Stream on 305: %+v", s.Status)
	}
}

func TestReconcileErrors(t *testing.T) {
	u := houseUnit(t)
	u.poll()
	invalid := error(apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "Stream"}, "king5",
		field.ErrorList{field.Invalid(field.NewPath("status", "state"), "quoted-value", "bad")}))
	u.statusErr.Store(&invalid)
	_, err := u.r.Reconcile(u.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "king5"}})
	if !apierrors.IsInvalid(err) || strings.Contains(err.Error(), "quoted-value") {
		t.Errorf("an invalid write: %v", err)
	}
	conflict := error(apierrors.NewConflict(schema.GroupResource{Resource: "streams"}, "king5", nil))
	u.statusErr.Store(&conflict)
	if res := u.reconcile("king5"); res.RequeueAfter <= 0 || res.RequeueAfter > time.Second {
		t.Errorf("a conflict: %+v, want a requeue within a second", res)
	}
	if e := u.rec.take(); len(e) != 0 {
		t.Errorf("events after failed writes: %q", e)
	}

	// An unreadable <fullname>-streams is an error.
	var cm corev1.ConfigMap
	if err := u.c.Get(u.ctx, client.ObjectKey{Namespace: ns, Name: names.StreamsConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[install.StreamsKey] = "[]"
	if err := u.c.Update(u.ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if _, err := u.r.Reconcile(u.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "king5"}}); err == nil {
		t.Error("an unreadable streams.json: no error")
	}

	// A failing cache is an error.
	boom := errors.New("cache not started")
	sch, err := ctrltest.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	for name, funcs := range map[string]interceptor.Funcs{
		"get": {Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		}},
		"list": {List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom }},
	} {
		c := fake.NewClientBuilder().WithScheme(sch).WithObjects(streamObject("king5", "705", kingSource), compiledObject(t, house(t))).
			WithInterceptorFuncs(funcs).Build()
		r, err := streamstate.New(streamstate.Config{Client: c, Namespace: ns, Names: names, Settings: testSettings(), Recorder: &recorder{}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(u.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "king5"}}); !errors.Is(err, boom) {
			t.Errorf("%s: %v, want the cache's error", name, err)
		}
	}
}

func TestConfigValidated(t *testing.T) {
	_, err := streamstate.New(streamstate.Config{Interval: -time.Second})
	for _, w := range []string{"no client", "no namespace", "fullname", "settings", "no event recorder", "interval"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("error %v; want it to say %q", err, w)
		}
	}
	if err := streamstate.Setup(nil, streamstate.Config{}); err == nil {
		t.Error("Setup accepted an empty Config")
	}
}
