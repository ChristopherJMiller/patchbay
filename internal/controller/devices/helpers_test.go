package devices_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/devices"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

var env *ctrltest.Env

func TestMain(m *testing.M) { os.Exit(ctrltest.Main(m, &env)) }

// t0 is the instant every test time is relative to.
var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

const (
	fullname  = "pbx"
	deskMAC   = "0200E2E00001"
	deskID    = "phone-desk"
	deskIP    = "192.168.20.101"
	laptopID  = "softphone-laptop"
	laptopIP  = "192.168.0.50"
	pbxAddr   = "192.168.0.240"
	annResync = v1alpha1.AnnotationResyncRequested
)

// testSettings are the install settings of every test: the chart's
// defaults.
func testSettings(n install.Names) compile.Settings {
	return compile.Settings{
		PBXAddress:      pbxAddr,
		SIPPort:         5060,
		RTPPorts:        compile.PortRange{First: 10000, Last: 10099},
		DeviceHTTPPort:  8088,
		NTPServer:       "192.168.20.1",
		Timezone:        "America/Los_Angeles",
		Signalling:      drivers.SignallingPJSIP,
		NumberPlan:      compile.DefaultNumberPlan(),
		AllowedNetworks: []string{"192.168.20.0/24", "192.168.0.0/24"},
		AMISecret:       n.AMIRef(),
		PushSecret:      n.PushRef(),
	}
}

// clock is a settable clock, safe for a manager's goroutines.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// recorded is one Event.
type recorded struct {
	Kind, Name, Type, Reason, Action, Note string
}

func (e recorded) String() string {
	return fmt.Sprintf("%s %s/%s %s %s: %s", e.Type, e.Kind, e.Name, e.Reason, e.Action, e.Note)
}

// recorder is an events.EventRecorder that keeps every Event with the
// object it regards.
type recorder struct {
	mu     sync.Mutex
	events []recorded
}

func (r *recorder) Eventf(regarding, _ runtime.Object, eventType, reason, action, note string, args ...any) {
	e := recorded{Type: eventType, Reason: reason, Action: action, Note: fmt.Sprintf(note, args...)}
	switch o := regarding.(type) {
	case *v1alpha1.Phone:
		e.Kind, e.Name = "Phone", o.Name
	case *v1alpha1.Softphone:
		e.Kind, e.Name = "Softphone", o.Name
	case client.Object:
		e.Kind, e.Name = fmt.Sprintf("%T", o), o.GetName()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// take returns the Events recorded since the last take.
func (r *recorder) take() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// actorCall is one action the fake agent was asked for.
type actorCall struct {
	Action string
	Req    agentapi.ActionRequest
}

// fakeActor is the agent's action endpoint: it records each call and
// answers with resp and err.
type fakeActor struct {
	mu    sync.Mutex
	calls []actorCall
	resp  agentapi.ActionResponse
	err   error
}

func newFakeActor() *fakeActor { return &fakeActor{resp: agentapi.ActionResponse{Sent: true}} }

func (a *fakeActor) answer(resp agentapi.ActionResponse, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resp, a.err = resp, err
}

func (a *fakeActor) call(action string, req agentapi.ActionRequest) (agentapi.ActionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, actorCall{action, req})
	return a.resp, a.err
}

func (a *fakeActor) Resync(_ context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error) {
	return a.call("resync", req)
}

func (a *fakeActor) Restart(_ context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error) {
	return a.call("restart", req)
}

// take returns the calls since the last take.
func (a *fakeActor) take() []actorCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.calls
	a.calls = nil
	return out
}

// logs captures a logr.Logger's output, every verbosity.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 10})
}

func (l *logs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// fixture is one test's install: its own namespace, compiled bundle, fake
// agent (a Holder and a fakeActor), clock, recorder and reconcilers. The
// reconcilers read through a direct client (no cache), wrapped to count
// status writes and to fail Secret writes on demand.
type fixture struct {
	t        *testing.T
	ctx      context.Context
	ns       string
	names    install.Names
	settings compile.Settings
	reg      *drivers.Registry

	client       client.Client
	statusWrites atomic.Int64
	failSecrets  atomic.Bool
	statusErr    atomic.Pointer[error]
	// statusErrOnce makes statusErr fail one status write, then clear.
	statusErrOnce atomic.Bool
	failList      atomic.Bool
	// stalePhone, when set, is what Gets of that Phone return: a cache
	// that has not seen the latest write.
	stalePhone atomic.Pointer[v1alpha1.Phone]

	holder *agentview.Holder
	actor  *fakeActor
	rec    *recorder
	clock  *clock
	logs   *logs
	rand   io.Reader

	// agent and store are what the next snapshot reports; compile sets the
	// agent's applied version to what it compiled.
	agent agentapi.AgentStatus
	store agentapi.FirmwareStore
	polls uint64

	bundle     compile.Bundle
	phones     *devices.PhoneReconciler
	softphones *devices.SoftphoneReconciler
}

func newFixture(t *testing.T, opts ...func(*fixture)) *fixture {
	t.Helper()
	ctrltest.Need(t, env)
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t:      t,
		ctx:    context.Background(),
		ns:     ctrltest.Namespace(t, env),
		names:  install.NewNames(fullname),
		reg:    reg,
		holder: &agentview.Holder{},
		actor:  newFakeActor(),
		rec:    &recorder{},
		clock:  &clock{t: t0},
		logs:   &logs{},
		agent:  agentapi.AgentStatus{StartedAt: t0.Add(-time.Hour), AsteriskUp: true, AsteriskVersion: "22.11.0"},
		store:  agentapi.FirmwareStore{Files: []agentapi.FirmwareFileStatus{}},
	}
	f.settings = testSettings(f.names)
	direct, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	f.client = interceptor.NewClient(direct, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			f.statusWrites.Add(1)
			if e := f.statusErr.Load(); e != nil {
				if f.statusErrOnce.Load() {
					f.statusErr.Store(nil)
				}
				return *e
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if p, ok := obj.(*v1alpha1.Phone); ok {
				if stale := f.stalePhone.Load(); stale != nil && stale.Name == key.Name {
					stale.DeepCopyInto(p)
					return nil
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if f.failList.Load() {
				return errors.New("injected list failure")
			}
			return c.List(ctx, list, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok && f.failSecrets.Load() {
				return apierrors.NewConflict(corev1.Resource("secrets"), obj.GetName(), fmt.Errorf("injected"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	for _, o := range opts {
		o(f)
	}
	cfg := f.config()
	if f.phones, err = devices.NewPhoneReconciler(cfg); err != nil {
		t.Fatal(err)
	}
	if f.softphones, err = devices.NewSoftphoneReconciler(cfg); err != nil {
		t.Fatal(err)
	}
	return f
}

// config is the reconcilers' Config.
func (f *fixture) config() devices.Config {
	return devices.Config{
		Client:    f.client,
		Namespace: f.ns,
		Names:     f.names,
		Settings:  f.settings,
		Registry:  f.reg,
		Observer:  f.holder,
		Actor:     f.actor,
		Changes:   agentview.NewChanges(agentview.DefaultChangesBuffer),
		Recorder:  f.rec,
		Rand:      f.rand,
		Now:       f.clock.Now,
		Log:       f.logs.logger(),
	}
}

func (f *fixture) at(d time.Duration) { f.clock.Set(t0.Add(d)) }

// newPhone creates a Phone.
func (f *fixture) newPhone(name, mac, number, label string, opts ...func(*v1alpha1.Phone)) {
	f.t.Helper()
	p := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: v1alpha1.PhoneSpec{
			Model: v1alpha1.PhoneModelCisco7945, MAC: v1alpha1.MAC(mac), Number: v1alpha1.Number(number), Label: label,
		},
	}
	for _, o := range opts {
		o(p)
	}
	if err := env.Client.Create(f.ctx, p); err != nil {
		f.t.Fatalf("creating Phone %s: %v", name, err)
	}

}

// newSoftphone creates a Softphone.
func (f *fixture) newSoftphone(name, number, label string) {
	f.t.Helper()
	s := &v1alpha1.Softphone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec:       v1alpha1.SoftphoneSpec{Number: v1alpha1.Number(number), Label: label},
	}
	if err := env.Client.Create(f.ctx, s); err != nil {
		f.t.Fatalf("creating Softphone %s: %v", name, err)
	}

}

// edit changes an object with a fresh read, retrying on a conflict.
func edit[T client.Object](f *fixture, obj T, change func(T)) {
	f.t.Helper()
	for range 5 {
		if err := env.Client.Get(f.ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			f.t.Fatal(err)
		}
		change(obj)
		err := env.Client.Update(f.ctx, obj)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			f.t.Fatal(err)
		}
	}
	f.t.Fatalf("editing %s: too many conflicts", obj.GetName())
}

// secretOf reads a device's Secret.
func (f *fixture) secretOf(id string) *corev1.Secret {
	f.t.Helper()
	var s corev1.Secret
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: compile.DeviceSecretName(id)}, &s); err != nil {
		f.t.Fatal(err)
	}
	return &s
}

// ensureSecret creates a Secret with the given keys, or fills the keys it
// lacks, never changing a value it has: what core's secrets.Manager does.
func (f *fixture) ensureSecret(name string, want map[string]string) {
	f.t.Helper()
	var s corev1.Secret
	err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, &s)
	switch {
	case apierrors.IsNotFound(err):
		s = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns, Labels: install.ManagedLabels()}, Data: map[string][]byte{}}
		for k, v := range want {
			s.Data[k] = []byte(v)
		}
		if err := env.Client.Create(f.ctx, &s); err != nil {
			f.t.Fatal(err)
		}
		return
	case err != nil:
		f.t.Fatal(err)
	}
	changed := false
	for k, v := range want {
		if len(s.Data[k]) == 0 {
			if s.Data == nil {
				s.Data = map[string][]byte{}
			}
			s.Data[k], changed = []byte(v), true
		}
	}
	if changed {
		if err := env.Client.Update(f.ctx, &s); err != nil {
			f.t.Fatal(err)
		}
	}
}

// compile compiles the namespace as the bundle reconciler would: every
// Secret ensured, compile.Compile, then pbx-secrets and pbx-config. The
// fake agent then reports the version applied.
func (f *fixture) compile() compile.Bundle {
	f.t.Helper()
	b := f.compileOnly()
	f.writeSecrets(b)
	f.writeConfigMap(b)
	f.agent.AppliedVersion, f.agent.LastApplyVersion, f.agent.LastApplyError = b.Version, b.Version, ""
	return b
}

// compileOnly ensures the Secrets and compiles, writing nothing else.
func (f *fixture) compileOnly() compile.Bundle {
	f.t.Helper()
	var (
		phones     v1alpha1.PhoneList
		softphones v1alpha1.SoftphoneList
		people     v1alpha1.PersonList
		streams    v1alpha1.StreamList
	)
	for _, l := range []client.ObjectList{&phones, &softphones, &people, &streams} {
		if err := env.Client.List(f.ctx, l, client.InNamespace(f.ns)); err != nil {
			f.t.Fatal(err)
		}
	}
	f.ensureSecret(f.names.AMISecret, map[string]string{install.AMIPasswordKey: "not-a-secret-ami"})
	f.ensureSecret(f.names.PushSecret, map[string]string{install.PushPasswordKey: "not-a-secret-push"})
	values := map[compile.SecretRef][]byte{f.names.AMIRef(): f.secretValue(f.names.AMIRef()), f.names.PushRef(): f.secretValue(f.names.PushRef())}
	for _, p := range phones.Items {
		id := drivers.DeviceID(drivers.KindPhone, p.Name)
		f.ensureSecret(compile.DeviceSecretName(id), map[string]string{compile.SecretKeySIPPassword: "not-a-secret-" + id})
		values[compile.PasswordRef(id)] = f.secretValue(compile.PasswordRef(id))
	}
	for _, s := range softphones.Items {
		id := drivers.DeviceID(drivers.KindSoftphone, s.Name)
		f.ensureSecret(compile.DeviceSecretName(id), map[string]string{
			compile.SecretKeySIPPassword:       "not-a-secret-" + id,
			compile.SecretKeyProvisioningToken: "notasecrettoken" + strings.ReplaceAll(id, "-", ""),
		})
		values[compile.PasswordRef(id)] = f.secretValue(compile.PasswordRef(id))
		values[compile.ProvisioningTokenRef(id)] = f.secretValue(compile.ProvisioningTokenRef(id))
	}
	b, err := compile.Compile(f.reg, compile.Inputs{
		Phones: phones.Items, Softphones: softphones.Items, People: people.Items, Streams: streams.Items, Secrets: values,
	}, f.settings)
	if err != nil {
		f.t.Fatalf("compile: %v", err)
	}
	f.bundle = b
	return b
}

func (f *fixture) secretValue(r compile.SecretRef) []byte {
	f.t.Helper()
	var s corev1.Secret
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: r.Name}, &s); err != nil {
		f.t.Fatal(err)
	}
	return s.Data[r.Key]
}

// writeSecrets writes pbx-secrets for b, as core does before pbx-config.
func (f *fixture) writeSecrets(b compile.Bundle) {
	f.t.Helper()
	refs, err := b.SecretRefs()
	if err != nil {
		f.t.Fatal(err)
	}
	values := map[compile.SecretRef][]byte{}
	for _, r := range refs {
		values[r] = f.secretValue(r)
	}
	s, err := install.ProjectedSecret(f.names, f.ns, b, values)
	if err != nil {
		f.t.Fatal(err)
	}
	f.upsert(s, func(cur client.Object) { cur.(*corev1.Secret).Data = s.Data })
}

// writeConfigMap writes pbx-config for b.
func (f *fixture) writeConfigMap(b compile.Bundle) {
	f.t.Helper()
	cm, err := install.ConfigMap(f.names, f.ns, b)
	if err != nil {
		f.t.Fatal(err)
	}
	f.upsert(cm, func(cur client.Object) {
		cur.(*corev1.ConfigMap).Data, cur.(*corev1.ConfigMap).BinaryData = cm.Data, cm.BinaryData
	})
}

// upsert creates obj, or updates the existing one with set and obj's
// annotations.
func (f *fixture) upsert(obj client.Object, set func(cur client.Object)) {
	f.t.Helper()
	err := env.Client.Create(f.ctx, obj)
	if err == nil {
		return
	}
	if !apierrors.IsAlreadyExists(err) {
		f.t.Fatal(err)
	}
	cur := obj.DeepCopyObject().(client.Object)
	if err := env.Client.Get(f.ctx, client.ObjectKeyFromObject(obj), cur); err != nil {
		f.t.Fatal(err)
	}
	set(cur)
	cur.SetAnnotations(obj.GetAnnotations())
	if err := env.Client.Update(f.ctx, cur); err != nil {
		f.t.Fatal(err)
	}
}

// see publishes a successful poll at the clock's time that saw devs.
func (f *fixture) see(devs ...agentapi.DeviceObservations) {
	f.polls++
	now := f.clock.Now()
	doc := agentapi.Empty(now, f.agent)
	doc.Devices = append(doc.Devices, devs...)
	doc.Firmware = f.store
	f.holder.Set(agentview.Snapshot{Document: doc, PolledAt: now, AgentReachable: true, Polls: f.polls})
}

// unreachable publishes a failed poll: the last document stays.
func (f *fixture) unreachable() {
	f.polls++
	s := f.holder.Snapshot()
	f.holder.Set(agentview.Snapshot{Document: s.Document, PolledAt: s.PolledAt, AgentReachable: false, Polls: f.polls,
		LastError: "connection refused"})
}

func (f *fixture) reconcilePhone(name string) ctrl.Result {
	f.t.Helper()
	res, err := f.phones.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: name}})
	if err != nil {
		f.t.Fatalf("reconciling Phone %s: %v", name, err)
	}
	return res
}

func (f *fixture) reconcileSoftphone(name string) ctrl.Result {
	f.t.Helper()
	res, err := f.softphones.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: name}})
	if err != nil {
		f.t.Fatalf("reconciling Softphone %s: %v", name, err)
	}
	return res
}

func (f *fixture) phone(name string) *v1alpha1.Phone {
	f.t.Helper()
	var p v1alpha1.Phone
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, &p); err != nil {
		f.t.Fatal(err)
	}
	return &p
}

func (f *fixture) softphone(name string) *v1alpha1.Softphone {
	f.t.Helper()
	var s v1alpha1.Softphone
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, &s); err != nil {
		f.t.Fatal(err)
	}
	return &s
}

// version is a device's compiled version in the last bundle.
func (f *fixture) version(id string) string {
	f.t.Helper()
	d, ok := f.bundle.Devices[id]
	if !ok || d.Version == "" {
		f.t.Fatalf("%s is not rendered in the bundle", id)
	}
	return d.Version
}

// ---- Observation builders ----

func fetch(file string, result agentapi.FetchResult, at time.Time) agentapi.Fetch {
	return agentapi.Fetch{File: file, Transport: agentapi.TransportTFTP, Result: result, At: at, Source: deskIP}
}

func served(file string, at time.Time) agentapi.Fetch { return fetch(file, agentapi.FetchServed, at) }

func notFound(file string, at time.Time) agentapi.Fetch {
	return fetch(file, agentapi.FetchNotFound, at)
}

// configFetch is a served fetch of desk's SEP file at a version.
func configFetch(version string, at time.Time) agentapi.Fetch {
	f := served("SEP"+deskMAC+".cnf.xml", at)
	f.Version = version
	return f
}

// provisioningFetch is a served fetch of laptop's provisioning document
// at its version.
func provisioningFetch(version string, at time.Time) agentapi.Fetch {
	return agentapi.Fetch{File: softphone.ProvisioningFile(laptopID), Transport: agentapi.TransportHTTPDevice,
		Result: agentapi.FetchServed, At: at, Source: laptopIP, Version: version}
}

// contact is a live registration that lapses at expires.
func contact(number, address string, expires time.Time) *agentapi.Registration {
	return &agentapi.Registration{
		Contact: "sip:" + number + "@" + address + ":5060", Address: address, UserAgent: "Cisco-CP7945G/9.4.2",
		CallID: "call-1", ExpiresAt: expires, Qualify: agentapi.QualifyUnknown,
	}
}

// ---- Assertions ----

// condition returns the condition of a type, failing the test without it.
func condition(t *testing.T, conds []metav1.Condition, typ string) metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		t.Fatalf("no %s condition in %v", typ, conds)
	}
	return *c
}

// wantCondition checks a condition's status and reason, and its message
// when message is not empty.
func wantCondition(t *testing.T, conds []metav1.Condition, typ string, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	c := condition(t, conds, typ)
	if c.Status != status || c.Reason != reason || (message != "" && c.Message != message) {
		t.Errorf("%s is %s %s %q; want %s %s %q", typ, c.Status, c.Reason, c.Message, status, reason, message)
	}
}

// wantEvents checks the Events since the last take, by type, reason and
// note.
func wantEvents(t *testing.T, got []recorded, want ...recorded) {
	t.Helper()
	strip := func(es []recorded) []string {
		out := make([]string, len(es))
		for i, e := range es {
			out[i] = e.String()
		}
		return out
	}
	if !slices.Equal(strip(got), strip(want)) {
		t.Errorf("events:\n got %q\nwant %q", strip(got), strip(want))
	}
}

// deskEvent is an Event regarding Phone desk.
func deskEvent(eventType, reason, action, note string) recorded {
	return recorded{Kind: "Phone", Name: "desk", Type: eventType, Reason: reason, Action: action, Note: note}
}

// laptopEvent is an Event regarding Softphone laptop.
func laptopEvent(eventType, reason, action, note string) recorded {
	return recorded{Kind: "Softphone", Name: "laptop", Type: eventType, Reason: reason, Action: action, Note: note}
}
