package messages_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/messages"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

var names = install.NewNames("patchbay")

func testSettings(t *testing.T) compile.Settings {
	t.Helper()
	s, err := settings.Load(filepath.Join("..", "settings", "testdata", "chart-defaults.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func registry(t *testing.T) *drivers.Registry {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func deskPhone(namespace string) *v1alpha1.Phone {
	return &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "desk", Namespace: namespace},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100"},
	}
}

// registered is a snapshot in which the agent answers and desk is
// registered until an hour after at.
func registered(at time.Time, edit ...func(*agentapi.DeviceObservations)) agentview.Snapshot {
	d := agentapi.DeviceObservations{
		Device:       drivers.DeviceID(drivers.KindPhone, "desk"),
		Registration: &agentapi.Registration{Contact: "sip:100@192.168.20.100", ExpiresAt: at.Add(time.Hour)},
	}
	for _, e := range edit {
		e(&d)
	}
	return agentview.Snapshot{
		Document: agentapi.Observations{Devices: []agentapi.DeviceObservations{d}},
		PolledAt: at, AgentReachable: true, Polls: 1,
	}
}

// actor is the agent's push action: it records every push and answers
// with resp and err; with ack set, a sent page becomes the holder's
// acknowledged page, as the agent's next poll would report it.
type actor struct {
	mu     sync.Mutex
	pushes []agentapi.PushRequest
	resp   agentapi.PushResponse
	err    error
	ack    *agentview.Holder
	now    func() time.Time
}

func newActor() *actor { return &actor{resp: agentapi.PushResponse{Sent: true, Phone: "status 0"}} }

func (a *actor) Resync(context.Context, agentapi.ActionRequest) (agentapi.ActionResponse, error) {
	return agentapi.ActionResponse{}, errors.New("not the device reconcilers")
}

func (a *actor) Restart(context.Context, agentapi.ActionRequest) (agentapi.ActionResponse, error) {
	return agentapi.ActionResponse{}, errors.New("not the device reconcilers")
}

func (a *actor) Push(_ context.Context, req agentapi.PushRequest) (agentapi.PushResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pushes = append(a.pushes, req)
	if a.ack != nil && a.err == nil && a.resp.Sent {
		s := a.ack.Snapshot()
		for i := range s.Document.Devices {
			if s.Document.Devices[i].Device == req.Device {
				s.Document.Devices[i].Page = req.PageVersion // "" for a clear
			}
		}
		s.PolledAt = a.now()
		a.ack.Set(s)
	}
	return a.resp, a.err
}

func (a *actor) answer(resp agentapi.PushResponse, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resp, a.err = resp, err
}

func (a *actor) take() []agentapi.PushRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.pushes
	a.pushes = nil
	return out
}

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

type shell struct {
	c      client.Client
	r      *messages.Reconciler
	holder *agentview.Holder
	actor  *actor
	rec    *events.FakeRecorder
	clock  *clock
}

func newShell(t *testing.T, objs ...client.Object) *shell {
	t.Helper()
	scheme, err := controller.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&v1alpha1.Message{}).Build()
	s := &shell{c: c, holder: &agentview.Holder{}, actor: newActor(), rec: events.NewFakeRecorder(100), clock: &clock{t: at0}}
	s.r, err = messages.New(messages.Config{
		Client: c, Namespace: ns, Names: names, Settings: testSettings(t), Registry: registry(t),
		Observer: s.holder, Actor: s.actor, Recorder: s.rec, Now: s.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *shell) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := s.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: messages.RequestName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (s *shell) message(t *testing.T, name string) v1alpha1.Message {
	t.Helper()
	var m v1alpha1.Message
	if err := s.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (s *shell) events() []string {
	var out []string
	for {
		select {
		case e := <-s.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func newMessage(name string, created time.Time, edit ...func(*v1alpha1.Message)) *v1alpha1.Message {
	m := message(name, "desk", created, edit...)
	m.Generation = 0
	return &m
}

// TestReconcileShowsInOneWrite: the push goes out, and the status the
// reconciler writes is the verdict of deciding again with its result:
// Shown at once, one MessageShown Event. The next reconcile, before the
// agent's observations show the page, pushes nothing more (PushedPage).
func TestReconcileShowsInOneWrite(t *testing.T) {
	s := newShell(t, deskPhone(ns), newMessage("m", at0.Add(-time.Minute)))
	s.holder.Set(registered(at0))
	res := s.reconcile(t)
	pushes := s.actor.take()
	if len(pushes) != 1 || !pushes[0].Beep || pushes[0].Clear || pushes[0].Device != "phone-desk" ||
		!strings.Contains(pushes[0].Page, "text of m") || pushes[0].PageVersion != messages.PageVersion([]byte(pushes[0].Page)) {
		t.Fatalf("pushes %+v, want one page with the chime", pushes)
	}
	m := s.message(t, "m")
	if m.Status.State != v1alpha1.MessageStateShown || m.Status.ShownAt == nil || !m.Status.ShownAt.Time.Equal(at0) {
		t.Errorf("status %+v, want Shown at %s", m.Status, at0)
	}
	if evs := s.events(); len(evs) != 1 || !strings.Contains(evs[0], install.EventReasonMessageShown) {
		t.Errorf("events %v, want one MessageShown", evs)
	}
	if res.RequeueAfter != install.PollInterval {
		t.Errorf("RequeueAfter %s, want the poll interval while a Message is current", res.RequeueAfter)
	}

	s.clock.add(5 * time.Second)
	s.reconcile(t)
	if p := s.actor.take(); len(p) != 0 {
		t.Errorf("pushed again before the agent showed the page: %+v", p)
	}
	// After PushedMemory, the agent still reports no page: it restarted, so
	// the same page goes again, without the chime.
	s.clock.add(messages.PushedMemory)
	s.holder.Set(registered(s.clock.Now()))
	s.reconcile(t)
	if p := s.actor.take(); len(p) != 1 || p[0].Beep || p[0].PageVersion != pushes[0].PageVersion {
		t.Errorf("after an agent restart: %+v, want the same page without the chime", p)
	}
	if evs := s.events(); len(evs) != 0 {
		t.Errorf("events %v on a re-send", evs)
	}
}

// TestReconcilePushFailure (ruling R15): a failed push holds the Message
// PushFailing with the agent's reason and a Warning PushFailed; nothing is
// pushed again for PushRetry, then it is, and a success shows it.
func TestReconcilePushFailure(t *testing.T) {
	s := newShell(t, deskPhone(ns), newMessage("m", at0.Add(-time.Minute)))
	s.holder.Set(registered(at0))
	s.actor.answer(agentapi.PushResponse{Error: "the phone did not accept the push", Phone: "status 4"}, nil)
	res := s.reconcile(t)
	if p := s.actor.take(); len(p) != 1 {
		t.Fatalf("pushes %+v, want one", p)
	}
	m := s.message(t, "m")
	if m.Status.State != v1alpha1.MessageStateHeld || m.Status.Reason != v1alpha1.ReasonPushFailing ||
		!strings.Contains(m.Status.Message, "the phone did not accept the push (the phone answered status 4)") {
		t.Errorf("status %+v, want Held PushFailing with the agent's reason", m.Status)
	}
	if evs := s.events(); len(evs) != 1 || !strings.HasPrefix(evs[0], "Warning "+install.EventReasonPushFailed) {
		t.Errorf("events %v, want one Warning PushFailed", evs)
	}
	if res.RequeueAfter != install.PollInterval {
		t.Errorf("RequeueAfter %s, want the poll interval", res.RequeueAfter)
	}

	s.clock.add(10 * time.Second)
	s.reconcile(t)
	if p := s.actor.take(); len(p) != 0 {
		t.Errorf("pushed again %s after a failure: %+v", 10*time.Second, p)
	}
	s.clock.add(messages.PushRetry)
	s.holder.Set(registered(s.clock.Now()))
	s.actor.answer(agentapi.PushResponse{Sent: true, Phone: "status 0"}, nil)
	s.reconcile(t)
	if p := s.actor.take(); len(p) != 1 || !p[0].Beep {
		t.Errorf("the retry: %+v, want the page with the chime", p)
	}
	if m := s.message(t, "m"); m.Status.State != v1alpha1.MessageStateShown {
		t.Errorf("after the retry: %+v, want Shown", m.Status)
	}
}

// TestReconcileWaitsForFirstPoll: nothing is decided before the poller's
// first attempt has ended.
func TestReconcileWaitsForFirstPoll(t *testing.T) {
	s := newShell(t, deskPhone(ns), newMessage("m", at0.Add(-time.Minute)))
	if res := s.reconcile(t); res.RequeueAfter != install.PollInterval {
		t.Errorf("RequeueAfter %s, want the poll interval", res.RequeueAfter)
	}
	if m := s.message(t, "m"); m.Status.State != "" || len(s.actor.take()) != 0 {
		t.Errorf("decided before the first poll: %+v", m.Status)
	}
}

// TestReconcileHouse: the house ConfigMap holds a non-urgent Message in
// quiet hours; a house.json that does not parse counts as quiet hours.
func TestReconcileHouse(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"quiet", `{"quiet":true,"away":false,"updatedAt":"2026-10-26T19:00:00Z"}`},
		{"unreadable", `{"quiet":"yes"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: names.HouseConfigMap, Namespace: ns},
				Data:       map[string]string{install.HouseKey: tc.raw},
			}
			s := newShell(t, deskPhone(ns), cm, newMessage("m", at0.Add(-time.Minute)),
				newMessage("m-urgent", at0.Add(-time.Minute), urgent))
			s.holder.Set(registered(at0))
			s.reconcile(t)
			if m := s.message(t, "m"); m.Status.State != v1alpha1.MessageStateHeld || m.Status.Reason != v1alpha1.ReasonQuietHours {
				t.Errorf("m: %+v, want Held QuietHours", m.Status)
			}
			if m := s.message(t, "m-urgent"); m.Status.State != v1alpha1.MessageStateShown {
				t.Errorf("m-urgent: %+v, want Shown", m.Status)
			}
		})
	}
}

// TestReconcileDeletes: a final Message a day old is deleted; the next
// decision is at the next deletion.
func TestReconcileDeletes(t *testing.T) {
	old := newMessage("m-old", at0.Add(-26*time.Hour))
	young := newMessage("m-young", at0.Add(-2*time.Hour))
	s := newShell(t, deskPhone(ns), old, young)
	for _, f := range []struct {
		m  *v1alpha1.Message
		at time.Time
	}{{old, at0.Add(-25 * time.Hour)}, {young, at0.Add(-time.Hour)}} {
		m := s.message(t, f.m.Name)
		m.Status = v1alpha1.MessageStatus{State: v1alpha1.MessageStateExpired, Message: "expired", FinishedAt: ptime(f.at)}
		if err := s.c.Status().Update(context.Background(), &m); err != nil {
			t.Fatal(err)
		}
	}
	s.holder.Set(registered(at0))
	res := s.reconcile(t)
	var m v1alpha1.Message
	if err := s.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "m-old"}, &m); !apierrors.IsNotFound(err) {
		t.Errorf("m-old: %v, want it deleted", err)
	}
	s.message(t, "m-young")
	if res.RequeueAfter != 23*time.Hour {
		t.Errorf("RequeueAfter %s, want 23h (m-young's deletion)", res.RequeueAfter)
	}
}

// withdraw sets a Message's spec.withdrawn, as the endpoint does.
func (s *shell) withdraw(t *testing.T, name string) {
	t.Helper()
	m := s.message(t, name)
	m.Spec.Withdrawn = true
	m.Generation++
	if err := s.c.Update(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
}

// inCall is desk registered at the clock's now, holding page, with calls
// person calls up.
func (s *shell) inCall(page string, calls int) {
	s.holder.Set(registered(s.clock.Now(), func(d *agentapi.DeviceObservations) { d.Page, d.Calls = page, calls }))
}

// TestReconcileClearOwedThroughACall (ruling R16a): the last Message is
// withdrawn during a call that outlasts RetainFinished. Nothing is pushed
// during the call, but the phone is owed a clear, so the reconciler keeps
// deciding every poll interval and keeps the Message; when the call ends
// the clear goes out and the Message is deleted.
func TestReconcileClearOwedThroughACall(t *testing.T) {
	s := newShell(t, deskPhone(ns), newMessage("m", at0.Add(-time.Minute)))
	s.actor.ack, s.actor.now = s.holder, s.clock.Now
	s.holder.Set(registered(at0))
	s.reconcile(t)
	page := s.actor.take()[0].PageVersion

	s.clock.add(time.Minute)
	s.inCall(page, 1)
	s.withdraw(t, "m")
	if res := s.reconcile(t); res.RequeueAfter != install.PollInterval {
		t.Errorf("a clear owed during a call: RequeueAfter %s, want the poll interval", res.RequeueAfter)
	}
	if m := s.message(t, "m"); m.Status.State != v1alpha1.MessageStateWithdrawn {
		t.Errorf("m: %+v, want Withdrawn", m.Status)
	}
	if p := s.actor.take(); len(p) != 0 {
		t.Errorf("pushed during a call: %+v", p)
	}

	s.clock.add(25 * time.Hour)
	s.inCall(page, 1)
	if res := s.reconcile(t); res.RequeueAfter != install.PollInterval || len(s.actor.take()) != 0 {
		t.Errorf("still in the call a day on: RequeueAfter %s", res.RequeueAfter)
	}
	s.message(t, "m") // kept: its phone is owed a clear

	s.clock.add(time.Second)
	s.inCall(page, 0)
	res := s.reconcile(t)
	if p := s.actor.take(); len(p) != 1 || !p[0].Clear {
		t.Errorf("the call ended: %+v, want a clear", p)
	}
	var m v1alpha1.Message
	if err := s.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "m"}, &m); !apierrors.IsNotFound(err) {
		t.Errorf("m after the clear: %v, want it deleted", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter %s with nothing left", res.RequeueAfter)
	}
}

// TestReconcileStaleCacheDoesNotChime: right after a push the cache can
// still hold a status written before it. A re-render in that lag (here a
// withdraw) does not chime for a Message the last push showed, and does
// not take it for a new one.
func TestReconcileStaleCacheDoesNotChime(t *testing.T) {
	s := newShell(t, deskPhone(ns), newMessage("m0", at0.Add(-2*time.Minute)), newMessage("m1", at0.Add(-time.Minute)))
	s.holder.Set(registered(at0))
	s.reconcile(t)
	if p := s.actor.take(); len(p) != 1 || !p[0].Beep {
		t.Fatalf("the first push %+v, want one with the chime", p)
	}
	// The cache has not caught up with m1's Shown yet.
	m1 := s.message(t, "m1")
	m1.Status = v1alpha1.MessageStatus{}
	if err := s.c.Status().Update(context.Background(), &m1); err != nil {
		t.Fatal(err)
	}
	s.withdraw(t, "m0")
	s.clock.add(time.Second)
	s.reconcile(t)
	p := s.actor.take()
	if len(p) != 1 || p[0].Beep || strings.Contains(p[0].Page, "text of m0") || !strings.Contains(p[0].Page, "text of m1") {
		t.Errorf("the re-render %+v, want m1 alone without the chime", p)
	}
	if m := s.message(t, "m1"); m.Status.State != v1alpha1.MessageStateShown {
		t.Errorf("m1: %+v, want Shown", m.Status)
	}
}

// TestReconcileForgetsFailure: once a phone is owed nothing, its push
// failure is dropped and schedules nothing (no 30 s poll for a day).
func TestReconcileForgetsFailure(t *testing.T) {
	s := newShell(t, deskPhone(ns), newMessage("m", at0.Add(-time.Minute)))
	s.holder.Set(registered(at0))
	s.actor.answer(agentapi.PushResponse{}, errors.New("agentclient: POST /v1/actions/push: connection refused"))
	s.reconcile(t)
	if m := s.message(t, "m"); m.Status.Reason != v1alpha1.ReasonPushFailing || !strings.Contains(m.Status.Message, "connection refused") {
		t.Fatalf("m: %+v, want Held PushFailing with the agent client's error", m.Status)
	}
	s.withdraw(t, "m")
	s.clock.add(time.Second)
	if res := s.reconcile(t); res.RequeueAfter != messages.RetainFinished {
		t.Errorf("RequeueAfter %s, want only m's deletion (%s)", res.RequeueAfter, messages.RetainFinished)
	}
	// A new Message goes out at once: no retry wait is left.
	if err := s.c.Create(context.Background(), newMessage("m2", s.clock.Now())); err != nil {
		t.Fatal(err)
	}
	s.actor.answer(agentapi.PushResponse{Sent: true, Phone: "status 0"}, nil)
	s.reconcile(t)
	if p := s.actor.take(); len(p) != 2 || !p[1].Beep {
		t.Errorf("pushes %+v, want the failed one, then m2 at once", p)
	}
}

// TestPhoneViewOf: the shell's view of a Phone. A Stream call is not a
// person call (stream-call-does-not-hold), so a Message is pushed during
// one; a person call next to it holds.
func TestPhoneViewOf(t *testing.T) {
	reg := registry(t)
	phone := deskPhone(ns)
	sig := testSettings(t).Signalling
	pv := messages.PhoneViewOf(phone, reg, sig, registered(at0, func(d *agentapi.DeviceObservations) {
		d.Calls, d.StreamCalls, d.Page = 1, 1, "0123456789abcdef"
	}), at0)
	if !pv.Exists || pv.DeviceID != "phone-desk" || pv.Model != "cisco-7945" || pv.Push != drivers.Supported ||
		pv.Renderer == nil || !pv.Reachable || pv.PersonCalls != 0 || pv.HeldPage != "0123456789abcdef" {
		t.Errorf("view %+v", pv)
	}
	m := message("m", "desk", at0.Add(-time.Minute))
	if v := messages.Decide(input([]v1alpha1.Message{m}, pv)); len(v.Pushes) != 1 {
		t.Errorf("stream-call-does-not-hold: %+v, want a push", v)
	}
	pv = messages.PhoneViewOf(phone, reg, sig, registered(at0, func(d *agentapi.DeviceObservations) { d.Calls, d.StreamCalls = 2, 1 }), at0)
	if pv.PersonCalls != 1 {
		t.Errorf("PersonCalls %d, want 1", pv.PersonCalls)
	}
	if v := messages.Decide(input([]v1alpha1.Message{m}, pv)); len(v.Pushes) != 0 || v.Status["m"].Reason != v1alpha1.ReasonInCall {
		t.Errorf("a person call next to a Stream call: %+v, want Held InCall", v)
	}

	for _, tc := range []struct {
		name string
		snap agentview.Snapshot
		now  time.Time
	}{
		{"the agent does not answer", func() agentview.Snapshot { s := registered(at0); s.AgentReachable = false; return s }(), at0},
		{"the observations are stale", registered(at0), at0.Add(install.StaleAfter + time.Second)},
		{"the registration lapsed", registered(at0.Add(-2 * time.Hour)), at0},
		{"no registration", registered(at0, func(d *agentapi.DeviceObservations) { d.Registration = nil }), at0},
		{"never seen", agentview.Snapshot{AgentReachable: true, PolledAt: at0, Polls: 1}, at0},
	} {
		if pv := messages.PhoneViewOf(phone, reg, sig, tc.snap, tc.now); pv.Reachable {
			t.Errorf("%s: reachable", tc.name)
		}
	}
	other := deskPhone(ns)
	other.Spec.Model = "cisco-7940"
	if pv := messages.PhoneViewOf(other, reg, sig, registered(at0), at0); pv.Push != drivers.Unsupported || pv.Renderer != nil {
		t.Errorf("a model no driver serves: %+v", pv)
	}
}

func TestNewValidates(t *testing.T) {
	scheme, err := controller.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	good := messages.Config{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: ns, Names: names, Settings: testSettings(t),
		Registry: registry(t), Observer: &agentview.Holder{}, Actor: newActor(), Recorder: events.NewFakeRecorder(1),
	}
	if _, err := messages.New(good); err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, edit := range map[string]func(*messages.Config){
		"no client":    func(c *messages.Config) { c.Client = nil },
		"no namespace": func(c *messages.Config) { c.Namespace = "" },
		"no names":     func(c *messages.Config) { c.Names = install.Names{} },
		"no registry":  func(c *messages.Config) { c.Registry = nil },
		"no observer":  func(c *messages.Config) { c.Observer = nil },
		"no actor":     func(c *messages.Config) { c.Actor = nil },
		"no recorder":  func(c *messages.Config) { c.Recorder = nil },
		"bad interval": func(c *messages.Config) { c.Interval = -time.Second },
		"bad timezone": func(c *messages.Config) { c.Settings.Timezone = "Mars/Olympus_Mons" },
	} {
		c := good
		edit(&c)
		if _, err := messages.New(c); err == nil {
			t.Errorf("%s: New passed", name)
		}
	}
}
