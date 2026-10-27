package bundle_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/bundle"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/controller/secrets"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

var env *ctrltest.Env

func TestMain(m *testing.M) { os.Exit(ctrltest.Main(m, &env)) }

var (
	names    = install.NewNames("patchbay")
	deskID   = drivers.DeviceID(drivers.KindPhone, "desk")
	laptopID = drivers.DeviceID(drivers.KindSoftphone, "laptop")
	deskSec  = compile.DeviceSecretName(deskID)
)

func settings() compile.Settings {
	return compile.Settings{
		PBXAddress:      "192.168.0.240",
		SIPPort:         5060,
		RTPPorts:        compile.PortRange{First: 10000, Last: 10099},
		DeviceHTTPPort:  8088,
		NTPServer:       "192.168.20.1",
		Timezone:        "UTC",
		Signalling:      drivers.SignallingPJSIP,
		NumberPlan:      compile.DefaultNumberPlan(),
		AllowedNetworks: []string{"192.168.20.0/24", "192.168.0.0/24"},
		AMISecret:       names.AMIRef(),
		PushSecret:      names.PushRef(),
	}
}

// flaky wraps the 79x5 driver; its Render fails while fail is set.
type flaky struct {
	drivers.Driver
	fail *atomic.Bool
}

func (f flaky) Render(dev drivers.Device, ctx drivers.RenderContext) (drivers.Files, error) {
	if f.fail.Load() {
		return nil, errors.New("render exploded")
	}
	return f.Driver.Render(dev, ctx)
}

// event is one recorded Event.
type event struct {
	regarding string // Kind/name
	eventType string
	reason    string
	action    string
	note      string
}

// recorder records Events (events.EventRecorder).
type recorder struct {
	mu     sync.Mutex
	events []event
}

func (r *recorder) Eventf(regarding, _ runtime.Object, eventType, reason, action, note string, args ...any) {
	name := fmt.Sprintf("%T", regarding)
	if o, ok := regarding.(client.Object); ok {
		name = strings.TrimPrefix(fmt.Sprintf("%T", o), "*v1.") + "/" + o.GetName()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event{name, eventType, reason, action, fmt.Sprintf(note, args...)})
}

func (r *recorder) take() []event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

var _ events.EventRecorder = (*recorder)(nil)

// writes records every write the reconciler's client makes: "verb
// Kind/name".
type writes struct {
	mu  sync.Mutex
	ops []string
}

func (w *writes) add(verb string, obj client.Object) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ops = append(w.ops, verb+" "+strings.TrimPrefix(fmt.Sprintf("%T", obj), "*v1.")+"/"+obj.GetName())
}

func (w *writes) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.ops
	w.ops = nil
	return out
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

type fixture struct {
	ns   string
	c    client.Client // direct, for the test's own reads and writes
	w    *writes
	rec  *recorder
	reg  *prometheus.Registry
	log  *logSink
	fail *atomic.Bool
	// inject, when set, is what the next create or update of pbx-config
	// or pbx-secrets returns instead of writing (once).
	inject  atomic.Pointer[error]
	r       *bundle.Reconciler
	secrets *secrets.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, settings())
}

// newFixtureWith is newFixture with other settings.
func newFixtureWith(t *testing.T, s compile.Settings) *fixture {
	t.Helper()
	ctrltest.Need(t, env)
	f := &fixture{
		ns: ctrltest.Namespace(t, env), c: env.Client, w: &writes{}, rec: &recorder{},
		reg: prometheus.NewRegistry(), log: &logSink{}, fail: &atomic.Bool{},
	}
	base, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := f.injected(obj); err != nil {
				return err
			}
			f.w.add("create", obj)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := f.injected(obj); err != nil {
				return err
			}
			f.w.add("update", obj)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			f.w.add("patch", obj)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			f.w.add("delete", obj)
			return c.Delete(ctx, obj, opts...)
		},
	})
	reg, err := drivers.NewRegistry(flaky{Driver: cisco79x5.New(), fail: f.fail}, softphone.New())
	if err != nil {
		t.Fatal(err)
	}
	m, err := metrics.New(metrics.Config{Registerer: f.reg})
	if err != nil {
		t.Fatal(err)
	}
	f.secrets, err = secrets.New(secrets.Config{Client: c, Namespace: f.ns, Names: names, Registry: reg, Log: f.log.logger()})
	if err != nil {
		t.Fatal(err)
	}
	f.r, err = bundle.New(bundle.Config{
		Client: c, Namespace: f.ns, Names: names, Settings: s, Registry: reg,
		Secrets: f.secrets, Recorder: f.rec, Metrics: m, Log: f.log.logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// injected is the error f.inject holds for a write of pbx-config or
// pbx-secrets, taken once.
func (f *fixture) injected(obj client.Object) error {
	if obj.GetName() != names.ConfigMap && obj.GetName() != names.ProjectedSecret {
		return nil
	}
	if e := f.inject.Swap(nil); e != nil {
		return *e
	}
	return nil
}

func (f *fixture) create(t *testing.T, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		o.SetNamespace(f.ns)
		if err := f.c.Create(context.Background(), o); err != nil {
			t.Fatalf("creating %s: %v", o.GetName(), err)
		}
	}
}

func (f *fixture) site(t *testing.T) {
	t.Helper()
	f.create(t,
		&v1alpha1.Phone{
			ObjectMeta: metav1.ObjectMeta{Name: "desk"},
			Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100", Label: "Desk"},
		},
		&v1alpha1.Softphone{
			ObjectMeta: metav1.ObjectMeta{Name: "laptop"},
			Spec:       v1alpha1.SoftphoneSpec{Number: "150", Label: "Laptop"},
		},
		&v1alpha1.Person{
			ObjectMeta: metav1.ObjectMeta{Name: "chris"},
			Spec: v1alpha1.PersonSpec{Number: "301", Name: "Chris", Rings: []v1alpha1.RingTarget{
				{Phone: "desk"}, {Softphone: "laptop"},
			}},
		},
	)
}

func (f *fixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := f.r.Reconcile(context.Background(), f.r.Key())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (f *fixture) configMap(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: names.ConfigMap}, &cm); err != nil {
		t.Fatalf("pbx-config: %v", err)
	}
	return &cm
}

func (f *fixture) secret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: name}, &s); err != nil {
		t.Fatalf("Secret %s: %v", name, err)
	}
	return &s
}

// compiled checks the pair the agent reads and returns the bundle.
func (f *fixture) compiled(t *testing.T) (compile.Bundle, *corev1.Secret) {
	t.Helper()
	cm := f.configMap(t)
	sec := f.secret(t, names.ProjectedSecret)
	b, err := compile.BundleFromConfigMap(cm.Data, cm.BinaryData)
	if err != nil {
		t.Fatalf("decoding pbx-config: %v", err)
	}
	refs, err := b.SecretRefs()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, r := range refs {
		k, err := r.ProjectedKey()
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, k)
	}
	if got := slices.Sorted(maps.Keys(sec.Data)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("pbx-secrets keys %v, want exactly the bundle's SecretRefs %v", got, want)
	}
	values, err := install.SecretValues(sec)
	if err != nil {
		t.Fatal(err)
	}
	// The agent's check: these values give this ConfigMap's version.
	if v, err := b.VersionWith(values); err != nil || v != b.Version {
		t.Errorf("VersionWith(pbx-secrets) = %s, %v; want pbx-config's version %s", v, err, b.Version)
	}
	for _, o := range []metav1.Object{cm, sec} {
		if got := o.GetAnnotations()[install.AnnotationBundleVersion]; got != b.Version {
			t.Errorf("%s bundle-version = %q, want %s", o.GetName(), got, b.Version)
		}
		for k, v := range install.ManagedLabels() {
			if o.GetLabels()[k] != v {
				t.Errorf("%s label %s = %q, want %q", o.GetName(), k, o.GetLabels()[k], v)
			}
		}
		if len(o.GetOwnerReferences()) != 0 {
			t.Errorf("%s has owners %v", o.GetName(), o.GetOwnerReferences())
		}
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("pbx-secrets type %q", sec.Type)
	}
	c, err := install.ReadCompiled(context.Background(), f.c, f.ns, names)
	if err != nil || c.Secrets == nil || c.Index.Version != b.Version {
		t.Errorf("ReadCompiled = %+v, %v; want the version with its secrets", c.Index.Version, err)
	}
	return b, sec
}

// indexOf is the position of op in ops, or -1.
func indexOf(ops []string, op string) int { return slices.Index(ops, op) }

// scrape is the registry's text.
func (f *fixture) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(f.reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// noSecretsLeaked checks every Event and log line against every value of
// every Secret in the namespace.
func (f *fixture) noSecretsLeaked(t *testing.T, evs []event) {
	t.Helper()
	var list corev1.SecretList
	if err := f.c.List(context.Background(), &list, client.InNamespace(f.ns)); err != nil {
		t.Fatal(err)
	}
	logged := f.log.String()
	for _, s := range list.Items {
		for k, v := range s.Data {
			if len(v) < 8 {
				continue
			}
			if strings.Contains(logged, string(v)) {
				t.Errorf("Secret %s key %s's value appears in the log", s.Name, k)
			}
			for _, e := range evs {
				if strings.Contains(e.note, string(v)) {
					t.Errorf("Secret %s key %s's value appears in an Event: %+v", s.Name, k, e)
				}
			}
		}
	}
}

func TestWritesSecretsThenConfigMap(t *testing.T) {
	f := newFixture(t)
	f.site(t)
	if res := f.reconcile(t); res != (ctrl.Result{}) {
		t.Errorf("Result = %+v, want none", res)
	}
	ops := f.w.take()
	s, c := indexOf(ops, "create Secret/"+names.ProjectedSecret), indexOf(ops, "create ConfigMap/"+names.ConfigMap)
	if s < 0 || c < 0 || s > c {
		t.Fatalf("writes %v: want pbx-secrets created before pbx-config", ops)
	}
	for _, want := range []string{"create Secret/" + names.AMISecret, "create Secret/" + names.PushSecret, "create Secret/" + names.AgentTokenSecret, "create Secret/" + deskSec} {
		if i := indexOf(ops, want); i < 0 || i > s {
			t.Errorf("writes %v: want %s before the bundle", ops, want)
		}
	}
	b, sec := f.compiled(t)
	if _, ok := b.Devices[deskID]; !ok {
		t.Errorf("the index lacks %s: %v", deskID, slices.Collect(maps.Keys(b.Devices)))
	}
	if got := sec.Data[deskSec+"_"+compile.SecretKeySIPPassword]; !bytes.Equal(got, f.secret(t, deskSec).Data[compile.SecretKeySIPPassword]) {
		t.Error("pbx-secrets holds another desk password than desk's Secret")
	}

	evs := f.rec.take()
	if len(evs) != 1 || evs[0].regarding != "ConfigMap/"+names.ConfigMap || evs[0].eventType != corev1.EventTypeNormal ||
		evs[0].reason != install.EventReasonConfigWritten || evs[0].action != install.EventActionCompile ||
		evs[0].note != fmt.Sprintf("version %s written: %d files, %d devices, 0 streams", b.Version[:12], len(b.Files), len(b.Devices)) {
		t.Errorf("Events %+v, want one ConfigWritten on pbx-config", evs)
	}
	if m := f.scrape(t); !strings.Contains(m, `patchbay_config_version_info{version="`+b.Version+`"} 1`) {
		t.Errorf("config_version_info lacks %s:\n%s", b.Version, m)
	}
	f.noSecretsLeaked(t, evs)
}

func TestNoWritesWhenUnchanged(t *testing.T) {
	f := newFixture(t)
	f.site(t)
	f.reconcile(t)
	f.w.take()
	f.rec.take()
	cm, sec := f.configMap(t), f.secret(t, names.ProjectedSecret)
	desk := f.secret(t, deskSec)

	f.reconcile(t)
	if ops := f.w.take(); len(ops) != 0 {
		t.Fatalf("a second reconcile wrote %v", ops)
	}
	if evs := f.rec.take(); len(evs) != 0 {
		t.Errorf("a second reconcile emitted %+v", evs)
	}
	for _, o := range []struct{ before, after metav1.Object }{
		{cm, f.configMap(t)}, {sec, f.secret(t, names.ProjectedSecret)}, {desk, f.secret(t, deskSec)},
	} {
		if o.before.GetResourceVersion() != o.after.GetResourceVersion() {
			t.Errorf("%s changed (resourceVersion %s -> %s)", o.before.GetName(), o.before.GetResourceVersion(), o.after.GetResourceVersion())
		}
	}
}

func TestResourceChangeRecompiles(t *testing.T) {
	f := newFixture(t)
	f.site(t)
	f.reconcile(t)
	before, _ := f.compiled(t)
	f.w.take()

	var p v1alpha1.Phone
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: "desk"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Label = "Desk Two"
	if err := f.c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	after, _ := f.compiled(t)
	if after.Version == before.Version {
		t.Fatal("a label change kept the version")
	}
	if after.Devices[deskID].Version == before.Devices[deskID].Version {
		t.Error("desk's own version did not change")
	}
	if after.Devices[laptopID].Version != before.Devices[laptopID].Version {
		t.Error("laptop's version changed with desk's label")
	}
	ops := f.w.take()
	want := []string{"update Secret/" + names.ProjectedSecret, "update ConfigMap/" + names.ConfigMap}
	if !slices.Equal(ops, want) {
		t.Fatalf("writes %v, want %v (the Secret's new version first)", ops, want)
	}
}

func TestDeletedDeviceSecretHeals(t *testing.T) {
	f := newFixture(t)
	f.site(t)
	f.reconcile(t)
	before, _ := f.compiled(t)
	old := f.secret(t, deskSec)
	if err := f.c.Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	f.w.take()
	f.reconcile(t)
	after, sec := f.compiled(t)
	healed := f.secret(t, deskSec)
	if healed.UID == old.UID {
		t.Fatal("the Secret was not recreated")
	}
	pw := healed.Data[compile.SecretKeySIPPassword]
	if bytes.Equal(pw, old.Data[compile.SecretKeySIPPassword]) {
		t.Error("the regenerated password equals the deleted one")
	}
	if after.Version == before.Version || after.Devices[deskID].Version == before.Devices[deskID].Version {
		t.Error("a new password kept the bundle's or desk's version")
	}
	if !bytes.Equal(sec.Data[deskSec+"_"+compile.SecretKeySIPPassword], pw) {
		t.Error("pbx-secrets does not hold the new password")
	}
	ops := f.w.take()
	if c, s, m := indexOf(ops, "create Secret/"+deskSec), indexOf(ops, "update Secret/"+names.ProjectedSecret),
		indexOf(ops, "update ConfigMap/"+names.ConfigMap); c < 0 || s < c || m < s {
		t.Fatalf("writes %v: want desk's Secret, then pbx-secrets, then pbx-config", ops)
	}
	f.noSecretsLeaked(t, f.rec.take())
}

func TestEditedConfigMapRewritten(t *testing.T) {
	f := newFixture(t)
	f.site(t)
	f.reconcile(t)
	good := f.configMap(t)
	ctx := context.Background()

	edits := map[string]func(cm *corev1.ConfigMap){
		"a file": func(cm *corev1.ConfigMap) {
			for k := range cm.Data {
				if k != compile.IndexKey {
					cm.Data[k] += "\n; injected"
					return
				}
			}
		},
		"the index": func(cm *corev1.ConfigMap) {
			cm.Data[compile.IndexKey] = strings.Replace(cm.Data[compile.IndexKey], `"100"`, `"199"`, 1)
		},
		"an added key":   func(cm *corev1.ConfigMap) { cm.Data["extra"] = "x" },
		"the annotation": func(cm *corev1.ConfigMap) { cm.Annotations[install.AnnotationBundleVersion] = strings.Repeat("0", 64) },
		"the labels":     func(cm *corev1.ConfigMap) { delete(cm.Labels, install.LabelManagedBy) },
	}
	for _, name := range slices.Sorted(maps.Keys(edits)) {
		cm := f.configMap(t)
		edits[name](cm)
		if err := f.c.Update(ctx, cm); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f.reconcile(t)
		now := f.configMap(t)
		if !maps.Equal(now.Data, good.Data) || now.Annotations[install.AnnotationBundleVersion] != good.Annotations[install.AnnotationBundleVersion] ||
			now.Labels[install.LabelManagedBy] != install.ManagedBy {
			t.Errorf("an edit of %s was not written back", name)
		}
	}

	if err := f.c.Delete(ctx, good); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if now := f.configMap(t); now.UID == good.UID || !maps.Equal(now.Data, good.Data) {
		t.Error("a deleted pbx-config was not recreated with the same bundle")
	}

	// An edited pbx-secrets is written back too.
	sec := f.secret(t, names.ProjectedSecret)
	want := maps.Clone(sec.Data)
	for k := range sec.Data {
		sec.Data[k] = []byte("notAsecretEdited")
		break
	}
	if err := f.c.Update(ctx, sec); err != nil {
		t.Fatal(err)
	}
	f.w.take()
	f.reconcile(t)
	if got := f.secret(t, names.ProjectedSecret).Data; !maps.EqualFunc(got, want, bytes.Equal) {
		t.Error("an edited pbx-secrets was not written back")
	}
	if ops := f.w.take(); !slices.Equal(ops, []string{"update Secret/" + names.ProjectedSecret}) {
		t.Errorf("writes %v, want only pbx-secrets", ops)
	}
	f.compiled(t)
}

func TestCompileErrorKeepsOldBundle(t *testing.T) {
	f := newFixture(t)
	f.site(t)
	f.reconcile(t)
	f.rec.take()
	cm, sec := f.configMap(t), f.secret(t, names.ProjectedSecret)

	f.fail.Store(true)
	var p v1alpha1.Phone
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: "desk"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Label = "Desk Two"
	if err := f.c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	f.w.take()
	res := f.reconcile(t)
	if res.RequeueAfter != bundle.CompileRetryAfter || bundle.CompileRetryAfter != 5*time.Minute {
		t.Errorf("Result = %+v, want RequeueAfter 5m", res)
	}
	if ops := f.w.take(); len(ops) != 0 {
		t.Errorf("a failed compile wrote %v", ops)
	}
	if f.configMap(t).ResourceVersion != cm.ResourceVersion || f.secret(t, names.ProjectedSecret).ResourceVersion != sec.ResourceVersion {
		t.Error("a failed compile changed the bundle")
	}
	evs := f.rec.take()
	if len(evs) != 1 || evs[0].regarding != "ConfigMap/"+names.ConfigMap || evs[0].eventType != corev1.EventTypeWarning ||
		evs[0].reason != install.EventReasonCompileFailed || evs[0].action != install.EventActionCompile ||
		!strings.Contains(evs[0].note, "render exploded") || !strings.Contains(evs[0].note, "Phone desk") {
		t.Errorf("Events %+v, want one CompileFailed Warning on pbx-config naming the cause", evs)
	}
	if m := f.scrape(t); !strings.Contains(m, metrics.NameCompileFailures+" 1") {
		t.Errorf("compile failures not counted:\n%s", m)
	}

	// Fixed: the new version is written.
	f.fail.Store(false)
	f.reconcile(t)
	if f.configMap(t).ResourceVersion == cm.ResourceVersion {
		t.Error("the fixed compile wrote nothing")
	}
}

func TestCompileFailedTargets(t *testing.T) {
	// Before pbx-config exists, the settings ConfigMap.
	f := newFixture(t)
	f.fail.Store(true)
	f.create(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.Settings}, Data: map[string]string{install.SettingsKey: "{}"}})
	f.site(t)
	f.reconcile(t)
	evs := f.rec.take()
	if len(evs) != 1 || evs[0].regarding != "ConfigMap/"+names.Settings || evs[0].reason != install.EventReasonCompileFailed {
		t.Errorf("Events %+v, want CompileFailed on the settings ConfigMap", evs)
	}
	var cm corev1.ConfigMap
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: names.ConfigMap}, &cm); !apierrors.IsNotFound(err) {
		t.Errorf("a failed first compile wrote pbx-config (%v)", err)
	}

	// Neither: only the log.
	f = newFixture(t)
	f.fail.Store(true)
	f.site(t)
	if res := f.reconcile(t); res.RequeueAfter != bundle.CompileRetryAfter {
		t.Errorf("Result = %+v", res)
	}
	if evs := f.rec.take(); len(evs) != 0 {
		t.Errorf("Events %+v with no ConfigMap to regard", evs)
	}
	if !strings.Contains(f.log.String(), "render exploded") {
		t.Errorf("the failure was not logged:\n%s", f.log)
	}
}

func TestFirstReconcileWithNoResources(t *testing.T) {
	f := newFixture(t)
	f.reconcile(t)
	tok := f.secret(t, names.AgentTokenSecret)
	if len(tok.Data[install.AgentTokenKey]) != 43 {
		t.Errorf("agent token has %d characters, want 43", len(tok.Data[install.AgentTokenKey]))
	}
	ami := f.secret(t, names.AMISecret)
	if len(ami.Data[install.AMIPasswordKey]) != install.AMIPasswordLength {
		t.Error("no AMI password")
	}
	b, sec := f.compiled(t)
	if len(b.Devices) != 0 {
		t.Errorf("devices %v, want none", b.Devices)
	}
	if want := []string{names.AMISecret + "_" + install.AMIPasswordKey, names.PushSecret + "_" + install.PushPasswordKey}; !slices.Equal(slices.Sorted(maps.Keys(sec.Data)), want) {
		t.Errorf("pbx-secrets keys %v, want only the AMI and push passwords", slices.Sorted(maps.Keys(sec.Data)))
	}
	if _, ok := b.Files[compile.TreeAsterisk+"/pjsip.conf"]; !ok {
		t.Error("a bundle with no devices has no pjsip.conf")
	}
}

func TestForeignDeviceSecretAdopted(t *testing.T) {
	f := newFixture(t)
	f.create(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: deskSec},
		Data:       map[string][]byte{compile.SecretKeySIPPassword: []byte("notAsecret12")},
	})
	f.site(t)
	f.reconcile(t)
	s := f.secret(t, deskSec)
	if string(s.Data[compile.SecretKeySIPPassword]) != "notAsecret12" {
		t.Error("adopting changed the password")
	}
	var p v1alpha1.Phone
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: "desk"}, &p); err != nil {
		t.Fatal(err)
	}
	owner := metav1.GetControllerOf(s)
	if owner == nil || owner.UID != p.UID || owner.Kind != "Phone" || !ptr.Deref(owner.BlockOwnerDeletion, false) {
		t.Errorf("owner %+v, want desk (uid %s) with blockOwnerDeletion", owner, p.UID)
	}
	if s.Labels[install.LabelManagedBy] != install.ManagedBy {
		t.Errorf("labels %v, want the managed labels added", s.Labels)
	}
	_, sec := f.compiled(t)
	if string(sec.Data[deskSec+"_"+compile.SecretKeySIPPassword]) != "notAsecret12" {
		t.Error("pbx-secrets does not hold the adopted password")
	}
	f.noSecretsLeaked(t, f.rec.take())
}

func TestUnusableSecretIsCompileFailed(t *testing.T) {
	f := newFixture(t)
	const bad = "not;a<secret>value"
	f.create(t,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.Settings}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: deskSec},
			Data:       map[string][]byte{compile.SecretKeySIPPassword: []byte(bad)},
		})
	f.site(t)
	if res := f.reconcile(t); res.RequeueAfter != bundle.CompileRetryAfter {
		t.Errorf("Result = %+v, want RequeueAfter 5m", res)
	}
	evs := f.rec.take()
	if len(evs) != 1 || evs[0].reason != install.EventReasonCompileFailed || !strings.Contains(evs[0].note, deskSec) ||
		!strings.Contains(evs[0].note, compile.SecretKeySIPPassword) {
		t.Fatalf("Events %+v, want CompileFailed naming %s's sip-password", evs, deskSec)
	}
	if strings.Contains(evs[0].note, bad) || strings.Contains(f.log.String(), bad) {
		t.Errorf("the value appears in the Event or the log")
	}
	if s := f.secret(t, deskSec); string(s.Data[compile.SecretKeySIPPassword]) != bad {
		t.Error("the refused value was overwritten")
	}
	var cm corev1.ConfigMap
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: names.ConfigMap}, &cm); !apierrors.IsNotFound(err) {
		t.Errorf("pbx-config was written (%v)", err)
	}
	if m := f.scrape(t); !strings.Contains(m, metrics.NameCompileFailures+" 1") {
		t.Errorf("compile failures not counted:\n%s", m)
	}
}

func TestAPIErrorReturned(t *testing.T) {
	ctrltest.Need(t, env)
	boom := errors.New("apiserver unavailable")
	for name, funcs := range map[string]interceptor.Funcs{
		"list": {List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom }},
		"create": {Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == names.ConfigMap {
				return boom
			}
			return c.Create(ctx, obj, opts...)
		}},
		"secrets": {Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == names.AMISecret {
				return boom
			}
			return c.Create(ctx, obj, opts...)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ns := ctrltest.Namespace(t, env)
			base, err := client.NewWithWatch(env.Config, client.Options{Scheme: env.Scheme})
			if err != nil {
				t.Fatal(err)
			}
			c := interceptor.NewClient(base, funcs)
			reg, err := drivers.NewRegistry(cisco79x5.New(), softphone.New())
			if err != nil {
				t.Fatal(err)
			}
			sm, err := secrets.New(secrets.Config{Client: c, Namespace: ns, Names: names, Registry: reg})
			if err != nil {
				t.Fatal(err)
			}
			rec := &recorder{}
			r, err := bundle.New(bundle.Config{Client: c, Namespace: ns, Names: names, Settings: settings(), Registry: reg, Secrets: sm, Recorder: rec})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(context.Background(), r.Key()); !errors.Is(err, boom) {
				t.Fatalf("Reconcile = %v, want the API error", err)
			}
			if evs := rec.take(); len(evs) != 0 {
				t.Errorf("an API error emitted %+v", evs)
			}
		})
	}
}

// TestWatchesTrigger runs the reconciler in a manager: it compiles at
// start with no resources, recompiles for a new Phone, heals a deleted
// device Secret and writes back a deleted pbx-config.
func TestWatchesTrigger(t *testing.T) {
	ctrltest.Need(t, env)
	ns := ctrltest.Namespace(t, env)
	mgr := ctrltest.NewManager(t, env, ns)
	reg, err := drivers.NewRegistry(cisco79x5.New(), softphone.New())
	if err != nil {
		t.Fatal(err)
	}
	sm, err := secrets.New(secrets.Config{Client: mgr.GetClient(), Namespace: ns, Names: names, Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	r, err := bundle.New(bundle.Config{
		Client: mgr.GetClient(), Namespace: ns, Names: names, Settings: settings(), Registry: reg,
		Secrets: sm, Recorder: mgr.GetEventRecorder(install.EventRecorderName),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctrltest.StartManager(t, mgr)
	ctx := context.Background()

	index := func() (compile.Index, bool) {
		c, err := install.ReadCompiled(ctx, env.Client, ns, names)
		if err != nil || c.Secrets == nil {
			return compile.Index{}, false
		}
		return c.Index, true
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	waitFor("the first bundle (no resources)", func() bool { _, ok := index(); return ok })

	if err := env.Client.Create(ctx, &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "desk"},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	var withDesk compile.Index
	waitFor("desk in pbx-config", func() bool {
		idx, ok := index()
		withDesk = idx
		return ok && idx.Devices[deskID].Version != ""
	})

	var old corev1.Secret
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: deskSec}, &old); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Delete(ctx, &old); err != nil {
		t.Fatal(err)
	}
	waitFor("a new version after desk's Secret was deleted", func() bool {
		idx, ok := index()
		return ok && idx.Version != withDesk.Version && idx.Devices[deskID].Version != withDesk.Devices[deskID].Version
	})
	var healed corev1.Secret
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: deskSec}, &healed); err != nil {
		t.Fatal(err)
	}
	if healed.UID == old.UID || bytes.Equal(healed.Data[compile.SecretKeySIPPassword], old.Data[compile.SecretKeySIPPassword]) {
		t.Error("desk's Secret was not regenerated with a new password")
	}

	idx, _ := index()
	var cm corev1.ConfigMap
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: names.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Delete(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	waitFor("pbx-config written back", func() bool {
		var now corev1.ConfigMap
		err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: names.ConfigMap}, &now)
		return err == nil && now.UID != cm.UID
	})
	if now, _ := index(); now.Version != idx.Version {
		t.Errorf("the rewritten pbx-config has version %s, want %s", now.Version, idx.Version)
	}

	// The ConfigWritten Events reached the API server.
	waitFor("a ConfigWritten Event", func() bool {
		var list eventsList
		return list.has(ctx, t, ns, install.EventReasonConfigWritten)
	})
}

type eventsList struct{}

func (eventsList) has(ctx context.Context, t *testing.T, ns, reason string) bool {
	t.Helper()
	var list corev1.EventList
	if err := env.Client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Items {
		if e.Reason == reason && e.InvolvedObject.Name == names.ConfigMap {
			return true
		}
	}
	return false
}

// TestCacheLagIsNotAnError: the manager's client reads from an informer
// cache, which learns of a write a moment after it succeeds, so a
// reconcile right after one writes from a stale copy: an update conflicts,
// a create finds the object already there. That is a quick requeue, not a
// reconcile error (logged as ERROR, counted, backed off); other API errors
// are still returned.
func TestCacheLagIsNotAnError(t *testing.T) {
	f := newFixture(t)
	f.site(t)
	lag := func(name string, err error) {
		t.Helper()
		f.inject.Store(&err)
		res, rerr := f.r.Reconcile(context.Background(), f.r.Key())
		if rerr != nil || res.RequeueAfter <= 0 || res.RequeueAfter > time.Second {
			t.Errorf("%s: %+v, %v; want a requeue within a second and no error", name, res, rerr)
		}
		if f.inject.Load() != nil {
			t.Errorf("%s: the reconcile did not write", name)
		}
	}
	gr := schema.GroupResource{Resource: "secrets"}
	lag("creating pbx-secrets: already exists", apierrors.NewAlreadyExists(gr, names.ProjectedSecret))
	f.reconcile(t)
	f.configMap(t) // written once the lag has passed

	var p v1alpha1.Phone
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: "desk"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Label = "Desk Two"
	if err := f.c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	lag("updating pbx-secrets: conflict", apierrors.NewConflict(gr, names.ProjectedSecret, errors.New("the object has been modified")))
	cm := f.configMap(t)
	f.reconcile(t)
	if f.configMap(t).ResourceVersion == cm.ResourceVersion {
		t.Error("the reconcile after the conflict wrote nothing")
	}

	forbidden := error(apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, names.ConfigMap, errors.New("RBAC")))
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.ns, Name: "desk"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Label = "Desk Three"
	if err := f.c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	f.inject.Store(&forbidden)
	if _, err := f.r.Reconcile(context.Background(), f.r.Key()); !apierrors.IsForbidden(err) {
		t.Errorf("a Forbidden write: error %v; want it returned", err)
	}
}

// TestCompileFailedRelatedAccepted: the API server takes a CompileFailed
// Event whose related object names its error (install.CompileFailedRelated).
func TestCompileFailedRelatedAccepted(t *testing.T) {
	f := newFixture(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.ConfigMap}}
	f.create(t, cm)
	note := "compile: Phone desk: cisco79x5: no time zone"
	ev := &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.ns, Name: "compile-failed-related"},
		EventTime:  metav1.NowMicro(), ReportingController: install.EventRecorderName, ReportingInstance: "test",
		Action: install.EventActionCompile, Reason: install.EventReasonCompileFailed, Type: corev1.EventTypeWarning, Note: note,
		Regarding: corev1.ObjectReference{APIVersion: "v1", Kind: "ConfigMap", Namespace: f.ns, Name: cm.Name, UID: cm.UID},
		Related:   install.CompileFailedRelated(cm, note),
	}
	if err := f.c.Create(context.Background(), ev); err != nil {
		t.Fatalf("the API server refused the Event: %v", err)
	}
}
