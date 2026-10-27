package numbers_test

import (
	"context"
	"fmt"
	"os"
	"slices"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/numbers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

var env *ctrltest.Env

func TestMain(m *testing.M) { os.Exit(ctrltest.Main(m, &env)) }

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func testSettings(n install.Names) compile.Settings {
	return compile.Settings{
		PBXAddress:      "192.168.0.240",
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

// recorder keeps every Event's regarded object name and note.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) Eventf(regarding, _ runtime.Object, eventType, reason, action, note string, args ...any) {
	name := ""
	if o, ok := regarding.(client.Object); ok {
		name = fmt.Sprintf("%T/%s", o, o.GetName())
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

// fixture is one test's namespace, compiled as the bundle reconciler would,
// and both reconcilers reading through a direct client that counts status
// writes.
type fixture struct {
	t            *testing.T
	ctx          context.Context
	ns           string
	names        install.Names
	settings     compile.Settings
	statusWrites atomic.Int64
	statusErr    atomic.Pointer[error]
	client       client.Client
	rec          *recorder
	people       *numbers.PersonReconciler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctrltest.Need(t, env)
	f := &fixture{t: t, ctx: context.Background(), ns: ctrltest.Namespace(t, env), names: install.NewNames("pbx"), rec: &recorder{}}
	f.settings = testSettings(f.names)
	direct, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	f.client = interceptor.NewClient(direct, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			f.statusWrites.Add(1)
			if e := f.statusErr.Load(); e != nil {
				return *e
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	cfg := f.config()
	if f.people, err = numbers.NewPersonReconciler(cfg); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) config() numbers.Config {
	return numbers.Config{
		Client: f.client, Namespace: f.ns, Names: f.names, Settings: f.settings, Recorder: f.rec,
		Now: func() time.Time { return t0 },
	}
}

func (f *fixture) create(obj client.Object) {
	f.t.Helper()
	obj.SetNamespace(f.ns)
	if err := env.Client.Create(f.ctx, obj); err != nil {
		f.t.Fatalf("creating %s: %v", obj.GetName(), err)
	}
}

func meta_(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }

func (f *fixture) newPhone(name, mac, number string) {
	f.create(&v1alpha1.Phone{ObjectMeta: meta_(name),
		Spec: v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: v1alpha1.MAC(mac), Number: v1alpha1.Number(number)}})
}

func (f *fixture) newSoftphone(name, number string) {
	f.create(&v1alpha1.Softphone{ObjectMeta: meta_(name), Spec: v1alpha1.SoftphoneSpec{Number: v1alpha1.Number(number)}})
}

// newPerson creates a Person ringing targets, each "phone <name>" or
// "softphone <name>".
func (f *fixture) newPerson(name, number string, targets ...string) {
	p := &v1alpha1.Person{ObjectMeta: meta_(name), Spec: v1alpha1.PersonSpec{Number: v1alpha1.Number(number), Name: "Chris"}}
	for _, t := range targets {
		kind, n, _ := strings.Cut(t, " ")
		if kind == "phone" {
			p.Spec.Rings = append(p.Spec.Rings, v1alpha1.RingTarget{Phone: v1alpha1.LocalName(n)})
		} else {
			p.Spec.Rings = append(p.Spec.Rings, v1alpha1.RingTarget{Softphone: v1alpha1.LocalName(n)})
		}
	}
	f.create(p)
}

// compile compiles the namespace and writes pbx-secrets then pbx-config,
// as the bundle reconciler does.
func (f *fixture) compile() compile.Bundle {
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
	values := map[compile.SecretRef][]byte{f.names.AMIRef(): []byte("not-a-secret-ami"), f.names.PushRef(): []byte("not-a-secret-push")}
	for _, p := range phones.Items {
		values[compile.PasswordRef(drivers.DeviceID(drivers.KindPhone, p.Name))] = []byte("not-a-secret-password")
	}
	for _, s := range softphones.Items {
		id := drivers.DeviceID(drivers.KindSoftphone, s.Name)
		values[compile.PasswordRef(id)] = []byte("not-a-secret-password")
		values[compile.ProvisioningTokenRef(id)] = []byte("notasecrettoken")
	}
	reg, err := builtin.Registry()
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := compile.Compile(reg, compile.Inputs{Phones: phones.Items, Softphones: softphones.Items, People: people.Items,
		Streams: streams.Items, Secrets: values}, f.settings)
	if err != nil {
		f.t.Fatal(err)
	}
	refs, err := b.SecretRefs()
	if err != nil {
		f.t.Fatal(err)
	}
	used := map[compile.SecretRef][]byte{}
	for _, r := range refs {
		used[r] = values[r]
	}
	s, err := install.ProjectedSecret(f.names, f.ns, b, used)
	if err != nil {
		f.t.Fatal(err)
	}
	f.upsert(s, func(cur client.Object) { cur.(*corev1.Secret).Data = s.Data })
	cm, err := install.ConfigMap(f.names, f.ns, b)
	if err != nil {
		f.t.Fatal(err)
	}
	f.upsert(cm, func(cur client.Object) {
		cur.(*corev1.ConfigMap).Data, cur.(*corev1.ConfigMap).BinaryData = cm.Data, cm.BinaryData
	})
	return b
}

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

func req(f *fixture, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: name}}
}

func (f *fixture) person(name string) v1alpha1.Person {
	f.t.Helper()
	if _, err := f.people.Reconcile(f.ctx, req(f, name)); err != nil {
		f.t.Fatalf("reconciling Person %s: %v", name, err)
	}
	var p v1alpha1.Person
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, &p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// want checks the state and both conditions, in order Ready then
// NumberConflict, each with the generation and t0.
func want(t *testing.T, state string, generation int64, gotState string, conds []metav1.Condition, wantConds ...metav1.Condition) {
	t.Helper()
	if gotState != state {
		t.Errorf("state %q; want %q", gotState, state)
	}
	if len(conds) != len(wantConds) {
		t.Fatalf("conditions %+v; want %d", conds, len(wantConds))
	}
	at := metav1.NewTime(t0)
	for i, w := range wantConds {
		c := conds[i]
		if c.Type != w.Type || c.Status != w.Status || c.Reason != w.Reason || c.Message != w.Message ||
			c.ObservedGeneration != generation || !c.LastTransitionTime.Equal(&at) {
			t.Errorf("condition %d: %+v; want %s %s %s %q at generation %d", i, c, w.Type, w.Status, w.Reason, w.Message, generation)
		}
	}
}

func cond(typ string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: status, Reason: reason, Message: message}
}

var noConflict = cond(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, "")

// TestPersonRouted: a Person whose targets all exist and are routed is
// Ready, naming them.
func TestPersonRouted(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", "0200E2E00001", "100")
	f.newSoftphone("laptop", "150")
	f.newPerson("chris", "301", "phone desk", "softphone laptop", "phone desk")
	f.compile()
	p := f.person("chris")
	want(t, numbers.StateReady, p.Generation, p.Status.State, p.Status.Conditions,
		cond(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonRouted, "rings phone desk, softphone laptop"), noConflict)
	if p.Status.ObservedGeneration != p.Generation {
		t.Errorf("observedGeneration %d; want %d", p.Status.ObservedGeneration, p.Generation)
	}
	if e := f.rec.take(); !slices.Equal(e, []string{
		"Normal *v1alpha1.Person/chris StateChanged Decide: None -> Ready: rings phone desk, softphone laptop"}) {
		t.Errorf("events %q", e)
	}
}

// TestPersonMissingTargets: a missing target and one not routed (it lost
// its number to an older Phone) make a Person Incomplete; with no routed
// target left it is Unavailable.
func TestPersonMissingTargets(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", "0200E2E00001", "100")
	f.newPhone("older", "0200E2E00002", "101")
	f.newPhone("y", "0200E2E00003", "101") // newer: 101 is older's
	f.newPerson("chris", "301", "phone desk", "softphone x", "phone y")
	f.compile()
	p := f.person("chris")
	want(t, numbers.StateIncomplete, p.Generation, p.Status.State, p.Status.Conditions,
		cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonMissingRingTargets,
			"rings phone desk; not softphone x (does not exist), phone y (not routed)"), noConflict)

	var desk v1alpha1.Phone
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "desk"}, &desk); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Delete(f.ctx, &desk); err != nil {
		t.Fatal(err)
	}
	f.compile()
	f.rec.take()
	p = f.person("chris")
	want(t, numbers.StateUnavailable, p.Generation, p.Status.State, p.Status.Conditions,
		cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonMissingRingTargets,
			`not phone desk (does not exist), softphone x (does not exist), phone y (not routed): dialling 301 plays "not available"`),
		noConflict)
	if e := f.rec.take(); len(e) != 1 || !strings.Contains(e[0], "Incomplete -> Unavailable") {
		t.Errorf("events %q; want one Incomplete -> Unavailable", e)
	}
}

// TestPersonNotRoutedBeforeCompiled: a target created after the last
// bundle is not routed until it is compiled.
func TestPersonNotRoutedBeforeCompiled(t *testing.T) {
	f := newFixture(t)
	f.newPerson("chris", "301", "phone desk")
	f.compile()
	f.newPhone("desk", "0200E2E00001", "100")
	if p := f.person("chris"); meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady).Message !=
		`not phone desk (not routed): dialling 301 plays "not available"` {
		t.Errorf("Ready %+v", p.Status.Conditions)
	}
	f.compile()
	if p := f.person("chris"); p.Status.State != numbers.StateReady {
		t.Errorf("state %s once compiled; want Ready", p.Status.State)
	}
}

// TestPersonNumberConflict (W1 carry-over): two Persons on 301: the older
// holds it, the newer is Invalid, from compile.ResolveClaims.
func TestPersonNumberConflict(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", "0200E2E00001", "100")
	f.newPerson("a-chris", "301", "phone desk")
	f.newPerson("b-erin", "301", "phone desk")
	b := f.compile()
	older := f.person("a-chris")
	newer := f.person("b-erin")
	if older.Status.State != numbers.StateReady {
		t.Errorf("the older is %s; want Ready", older.Status.State)
	}
	const msg = "number 301 is held by Person a-chris, which is older"
	want(t, numbers.StateInvalid, newer.Generation, newer.Status.State, newer.Status.Conditions,
		cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNumberConflict, msg),
		cond(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, v1alpha1.ReasonNumberConflict, msg))
	// Compile routes 301 to the holder alone.
	conf := string(b.Files[compile.TreeAsterisk+"/extensions.conf"])
	if !strings.Contains(conf, "Person a-chris") || strings.Contains(conf, "Person b-erin") {
		t.Errorf("extensions.conf does not route 301 to a-chris alone:\n%s", conf)
	}
}

// TestPersonNotInPlan: a Person on a device number is Invalid.
func TestPersonNotInPlan(t *testing.T) {
	f := newFixture(t)
	f.newPerson("chris", "100", "phone desk")
	f.compile()
	p := f.person("chris")
	want(t, numbers.StateInvalid, p.Generation, p.Status.State, p.Status.Conditions,
		cond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNumberNotInPlan, "number 100 is not in the Person range 3XX"),
		noConflict)
}

// TestStatusWrittenOnlyWhenChanged: judging again on unchanged input writes
// nothing.
func TestStatusWrittenOnlyWhenChanged(t *testing.T) {
	f := newFixture(t)
	f.newPerson("chris", "301", "phone desk")
	f.compile()
	p := f.person("chris")
	if n := f.statusWrites.Load(); n != 1 {
		t.Fatalf("%d status writes; want 1", n)
	}
	if p2 := f.person("chris"); f.statusWrites.Load() != 1 || p2.ResourceVersion != p.ResourceVersion {
		t.Errorf("judging again: %d status writes; want 1 and an unchanged resourceVersion", f.statusWrites.Load())
	}
	if e := f.rec.take(); len(e) != 1 {
		t.Errorf("events %q; want the first StateChanged", e)
	}
}

// TestNothingBeforeCompiled: no status while pbx-config does not exist.
func TestNothingBeforeCompiled(t *testing.T) {
	f := newFixture(t)
	f.newPerson("chris", "301", "phone desk")
	if p := f.person("chris"); p.Status.State != "" || f.statusWrites.Load() != 0 {
		t.Errorf("status %+v; want none", p.Status)
	}
	// Missing resources are done.
	if _, err := f.people.Reconcile(f.ctx, req(f, "gone")); err != nil {
		t.Error(err)
	}
}

// TestStatusWriteError: a failed write is returned, recognisable, and
// quotes nothing from the object; a conflict (the cache had not seen the
// last write yet) is a quick requeue, not an error.
func TestStatusWriteError(t *testing.T) {
	f := newFixture(t)
	f.newPerson("chris", "301", "phone desk")
	f.compile()
	invalid := error(apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "Person"}, "chris",
		field.ErrorList{field.Invalid(field.NewPath("status", "state"), "quoted-value", "bad")}))
	f.statusErr.Store(&invalid)
	if _, err := f.people.Reconcile(f.ctx, req(f, "chris")); !apierrors.IsInvalid(err) || strings.Contains(err.Error(), "quoted-value") {
		t.Errorf("error %v", err)
	}
	conflict := error(apierrors.NewConflict(schema.GroupResource{Resource: "persons"}, "dana", nil))
	f.statusErr.Store(&conflict)
	f.newPerson("dana", "302", "phone desk")
	if res, err := f.people.Reconcile(f.ctx, req(f, "dana")); err != nil || res.RequeueAfter <= 0 || res.RequeueAfter > time.Second {
		t.Errorf("Person conflict: %+v, %v; want a requeue within a second and no error", res, err)
	}
	if e := f.rec.take(); len(e) != 0 {
		t.Errorf("events %q after failed writes", e)
	}
}

// TestIndexUnreadable: a pbx-config whose index does not decode is an
// error.
func TestIndexUnreadable(t *testing.T) {
	f := newFixture(t)
	f.newPerson("chris", "301", "phone desk")
	f.compile()
	var cm corev1.ConfigMap
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: f.names.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[compile.IndexKey] = "[]"
	if err := env.Client.Update(f.ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if _, err := f.people.Reconcile(f.ctx, req(f, "chris")); err == nil {
		t.Error("Person: no error")
	}
}

// TestConfigValidated: an incomplete Config is refused.
func TestConfigValidated(t *testing.T) {
	_, err := numbers.NewPersonReconciler(numbers.Config{})
	for _, w := range []string{"no client", "no namespace", "fullname", "settings", "no event recorder"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("error %v; want it to say %q", err, w)
		}
	}
	if err := numbers.Setup(nil, numbers.Config{}); err == nil {
		t.Error("Setup accepted an empty Config")
	}
}

// TestLongNoteBounded: a Person whose sixteen ring targets have long names
// has a Ready message longer than events.k8s.io's 1024-byte note; its
// StateChanged note is cut to fit, so the API server does not refuse it.
func TestLongNoteBounded(t *testing.T) {
	f := newFixture(t)
	var targets []string
	for i := range 16 {
		targets = append(targets, fmt.Sprintf("phone %s-%02d", strings.Repeat("x", 200), i))
	}
	f.newPerson("chris", "301", targets...)
	f.compile()
	p := f.person("chris")
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady); c == nil || len(c.Message) <= install.MaxEventNote {
		t.Fatalf("Ready %+v; want a message longer than an Event note", c)
	}
	e := f.rec.take()
	if len(e) != 1 || !strings.Contains(e[0], "None -> Unavailable") {
		t.Fatalf("events %q; want one None -> Unavailable", e)
	}
	if _, note, _ := strings.Cut(e[0], " Decide: "); len(note) > install.MaxEventNote || !strings.HasSuffix(note, "…") {
		t.Errorf("StateChanged note of %d bytes; want it cut to at most %d", len(note), install.MaxEventNote)
	}
}
