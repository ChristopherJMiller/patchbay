package secrets_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/secrets"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

const ns = "telephony"

var (
	names     = install.NewNames("patchbay")
	deskID    = drivers.DeviceID(drivers.KindPhone, "desk")
	laptopID  = drivers.DeviceID(drivers.KindSoftphone, "laptop")
	deskName  = compile.DeviceSecretName(deskID)
	laptopSec = compile.DeviceSecretName(laptopID)
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

func registry(t *testing.T) *drivers.Registry {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func desk() *v1alpha1.Phone {
	return &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "desk", UID: "uid-desk"},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100"},
	}
}

func laptop() *v1alpha1.Softphone {
	return &v1alpha1.Softphone{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "laptop", UID: "uid-laptop"},
		Spec:       v1alpha1.SoftphoneSpec{Number: "150"},
	}
}

// writes counts the writes a fake client made.
type writes struct {
	mu    sync.Mutex
	names []string // "create <name>", "update <name>"
}

func (w *writes) record(op string, obj client.Object) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.names = append(w.names, op+" "+obj.GetName())
}

func (w *writes) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.names
	w.names = nil
	return out
}

type fixture struct {
	c   client.Client
	w   *writes
	m   *secrets.Manager
	log *strings.Builder
}

func newFixture(t *testing.T, reg *drivers.Registry, objs ...client.Object) *fixture {
	t.Helper()
	w := &writes{}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				w.record("create", obj)
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				w.record("update", obj)
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				w.record("patch", obj)
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	log := &strings.Builder{}
	var mu sync.Mutex
	m, err := secrets.New(secrets.Config{
		Client: c, Namespace: ns, Names: names, Registry: reg,
		Log: funcr.New(func(prefix, args string) {
			mu.Lock()
			defer mu.Unlock()
			log.WriteString(prefix + " " + args + "\n")
		}, funcr.Options{Verbosity: 10}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{c: c, w: w, m: m, log: log}
}

func (f *fixture) secret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
		t.Fatalf("Secret %s: %v", name, err)
	}
	return &s
}

func alnum(t *testing.T, what string, v []byte, n int) {
	t.Helper()
	if len(v) != n {
		t.Errorf("%s has %d characters, want %d", what, len(v), n)
	}
	for _, b := range v {
		if !strings.ContainsRune(drivers.Alphanumeric, rune(b)) {
			t.Errorf("%s holds %q, not an alphanumeric", what, b)
			return
		}
	}
}

func managed(t *testing.T, s *corev1.Secret) {
	t.Helper()
	for k, v := range install.ManagedLabels() {
		if s.Labels[k] != v {
			t.Errorf("Secret %s label %s = %q, want %q", s.Name, k, s.Labels[k], v)
		}
	}
}

func ownedBy(t *testing.T, s *corev1.Secret, kind, name string, uid types.UID) {
	t.Helper()
	if len(s.OwnerReferences) != 1 {
		t.Fatalf("Secret %s owners = %+v, want one", s.Name, s.OwnerReferences)
	}
	o := s.OwnerReferences[0]
	want := metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: kind, Name: name, UID: uid,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
	if o.APIVersion != want.APIVersion || o.Kind != want.Kind || o.Name != want.Name || o.UID != want.UID ||
		!ptr.Deref(o.Controller, false) || !ptr.Deref(o.BlockOwnerDeletion, false) {
		t.Errorf("Secret %s owner = %+v, want %+v", s.Name, o, want)
	}
}

func TestEnsureCreatesDeviceSecrets(t *testing.T) {
	f := newFixture(t, registry(t))
	got, err := f.m.Ensure(context.Background(), []v1alpha1.Phone{*desk()}, []v1alpha1.Softphone{*laptop()})
	if err != nil {
		t.Fatal(err)
	}

	d := f.secret(t, deskName)
	alnum(t, "desk sip-password", d.Data[compile.SecretKeySIPPassword], 12)
	if len(d.Data) != 1 {
		t.Errorf("desk Secret keys = %v, want only sip-password", d.Data)
	}
	if d.Type != corev1.SecretTypeOpaque {
		t.Errorf("desk Secret type %q", d.Type)
	}
	managed(t, d)
	ownedBy(t, d, "Phone", "desk", "uid-desk")

	l := f.secret(t, laptopSec)
	alnum(t, "laptop sip-password", l.Data[compile.SecretKeySIPPassword], 24)
	alnum(t, "laptop provisioning-token", l.Data[compile.SecretKeyProvisioningToken], install.ProvisioningTokenLength)
	if len(l.Data) != 2 {
		t.Errorf("laptop Secret has %d keys, want sip-password and provisioning-token", len(l.Data))
	}
	managed(t, l)
	ownedBy(t, l, "Softphone", "laptop", "uid-laptop")

	ami := f.secret(t, names.AMISecret)
	want := map[compile.SecretRef][]byte{
		compile.PasswordRef(deskID):            d.Data[compile.SecretKeySIPPassword],
		compile.PasswordRef(laptopID):          l.Data[compile.SecretKeySIPPassword],
		compile.ProvisioningTokenRef(laptopID): l.Data[compile.SecretKeyProvisioningToken],
		names.AMIRef():                         ami.Data[install.AMIPasswordKey],
		names.PushRef():                        f.secret(t, names.PushSecret).Data[install.PushPasswordKey],
	}
	if len(got) != len(want) {
		t.Errorf("Ensure returned %d values, want %d", len(got), len(want))
	}
	for ref, v := range want {
		if !bytes.Equal(got[ref], v) {
			t.Errorf("Ensure returned another value for %s/%s than the Secret holds", ref.Name, ref.Key)
		}
	}

	// The returned values compile.
	if _, err := compile.Compile(registry(t), compile.Inputs{
		Phones: []v1alpha1.Phone{*desk()}, Softphones: []v1alpha1.Softphone{*laptop()}, Secrets: got,
	}, settings()); err != nil {
		t.Errorf("Compile with Ensure's values: %v", err)
	}
}

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
		AllowedNetworks: []string{"192.168.20.0/24"},
		AMISecret:       names.AMIRef(),
		PushSecret:      names.PushRef(),
	}
}

func TestEnsureWritesNothingWhenComplete(t *testing.T) {
	f := newFixture(t, registry(t))
	ctx := context.Background()
	first, err := f.m.Ensure(ctx, []v1alpha1.Phone{*desk()}, []v1alpha1.Softphone{*laptop()})
	if err != nil {
		t.Fatal(err)
	}
	if w := f.w.take(); len(w) != 5 {
		t.Fatalf("first Ensure wrote %v, want 5 creates", w)
	}
	second, err := f.m.Ensure(ctx, []v1alpha1.Phone{*desk()}, []v1alpha1.Softphone{*laptop()})
	if err != nil {
		t.Fatal(err)
	}
	if w := f.w.take(); len(w) != 0 {
		t.Fatalf("second Ensure wrote %v, want nothing", w)
	}
	for ref, v := range first {
		if !bytes.Equal(second[ref], v) {
			t.Errorf("%s/%s changed between two Ensures", ref.Name, ref.Key)
		}
	}
}

func TestEnsureFillsMissingKey(t *testing.T) {
	for name, data := range map[string]map[string][]byte{
		"missing": {compile.SecretKeySIPPassword: []byte("not0a0secret0password000")},
		"empty":   {compile.SecretKeySIPPassword: []byte("not0a0secret0password000"), compile.SecretKeyProvisioningToken: {}},
	} {
		t.Run(name, func(t *testing.T) {
			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: laptopSec, Labels: install.ManagedLabels()},
				Type:       corev1.SecretTypeOpaque,
				Data:       data,
			}
			f := newFixture(t, registry(t), laptop(), existing)
			got, err := f.m.Ensure(context.Background(), nil, []v1alpha1.Softphone{*laptop()})
			if err != nil {
				t.Fatal(err)
			}
			s := f.secret(t, laptopSec)
			if string(s.Data[compile.SecretKeySIPPassword]) != "not0a0secret0password000" {
				t.Error("the existing sip-password was changed")
			}
			alnum(t, "provisioning-token", s.Data[compile.SecretKeyProvisioningToken], install.ProvisioningTokenLength)
			if !bytes.Equal(got[compile.ProvisioningTokenRef(laptopID)], s.Data[compile.SecretKeyProvisioningToken]) {
				t.Error("Ensure returned another token than it stored")
			}
			ownedBy(t, s, "Softphone", "laptop", "uid-laptop")
		})
	}
}

func TestEnsureRegeneratesDeleted(t *testing.T) {
	f := newFixture(t, registry(t))
	ctx := context.Background()
	first, err := f.m.Ensure(ctx, []v1alpha1.Phone{*desk()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.Delete(ctx, f.secret(t, deskName)); err != nil {
		t.Fatal(err)
	}
	f.w.take()
	second, err := f.m.Ensure(ctx, []v1alpha1.Phone{*desk()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.w.take(); len(w) != 1 || w[0] != "create "+deskName {
		t.Errorf("writes %v, want one create of %s", w, deskName)
	}
	ref := compile.PasswordRef(deskID)
	if bytes.Equal(first[ref], second[ref]) {
		t.Error("the regenerated password equals the deleted one")
	}
	if !bytes.Equal(second[ref], f.secret(t, deskName).Data[compile.SecretKeySIPPassword]) {
		t.Error("Ensure returned another password than it stored")
	}
	if !bytes.Equal(first[names.AMIRef()], second[names.AMIRef()]) {
		t.Error("the AMI password changed")
	}
}

// TestEnsureCreatesPushSecret: the push password is created once, as 16
// alphanumerics, kept when it exists and returned under PushRef.
func TestEnsureCreatesPushSecret(t *testing.T) {
	f := newFixture(t, registry(t))
	ctx := context.Background()
	first, err := f.m.Ensure(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	push := f.secret(t, names.PushSecret)
	alnum(t, "push password", push.Data[install.PushPasswordKey], install.PushPasswordLength)
	if len(push.OwnerReferences) != 0 {
		t.Errorf("the push Secret has owners %+v, want none", push.OwnerReferences)
	}
	managed(t, push)
	if !bytes.Equal(first[names.PushRef()], push.Data[install.PushPasswordKey]) {
		t.Error("Ensure returned another push password than it stored")
	}
	f.w.take()
	second, err := f.m.Ensure(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.w.take(); len(w) != 0 {
		t.Errorf("second Ensure wrote %v, want nothing", w)
	}
	if !bytes.Equal(second[names.PushRef()], first[names.PushRef()]) {
		t.Error("the push password changed")
	}

	// A value that is already there is kept.
	f = newFixture(t, registry(t), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.PushSecret},
		Data:       map[string][]byte{install.PushPasswordKey: []byte("notAsecretPush1")},
	})
	got, err := f.m.Ensure(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[names.PushRef()]) != "notAsecretPush1" || string(f.secret(t, names.PushSecret).Data[install.PushPasswordKey]) != "notAsecretPush1" {
		t.Error("an existing push password was changed")
	}
}

func TestEnsureAdopts(t *testing.T) {
	for name, owners := range map[string][]metav1.OwnerReference{
		"no owner": nil,
		"stale owner": {{
			APIVersion: v1alpha1.GroupVersion.String(), Kind: "Phone", Name: "desk", UID: "uid-old-desk",
			Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns, Name: deskName, OwnerReferences: owners,
					Labels: map[string]string{install.LabelManagedBy: "someone-else", "keep": "me"},
				},
				Data: map[string][]byte{compile.SecretKeySIPPassword: []byte("notAsecret12")},
			}
			f := newFixture(t, registry(t), desk(), existing)
			got, err := f.m.Ensure(context.Background(), []v1alpha1.Phone{*desk()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			s := f.secret(t, deskName)
			if string(s.Data[compile.SecretKeySIPPassword]) != "notAsecret12" || string(got[compile.PasswordRef(deskID)]) != "notAsecret12" {
				t.Error("adopting changed the password")
			}
			ownedBy(t, s, "Phone", "desk", "uid-desk")
			if s.Labels[install.LabelPartOf] != install.PartOf || s.Labels[install.LabelManagedBy] != "someone-else" || s.Labels["keep"] != "me" {
				t.Errorf("labels %v: want part-of added, the others kept", s.Labels)
			}
			if w := f.w.take(); !containsAll(w, "update "+deskName) {
				t.Errorf("writes %v, want an update of %s", w, deskName)
			}
		})
	}
}

func containsAll(have []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

func TestInstallSecrets(t *testing.T) {
	f := newFixture(t, registry(t))
	got, err := f.m.Ensure(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ami := f.secret(t, names.AMISecret)
	alnum(t, "AMI password", ami.Data[install.AMIPasswordKey], install.AMIPasswordLength)
	tok := f.secret(t, names.AgentTokenSecret)
	raw := tok.Data[install.AgentTokenKey]
	if len(raw) != 43 {
		t.Errorf("agent token has %d characters, want 43", len(raw))
	}
	if b, err := base64.RawURLEncoding.DecodeString(string(raw)); err != nil || len(b) != install.AgentTokenBytes {
		t.Errorf("agent token is not %d bytes of unpadded base64url: %d bytes, %v", install.AgentTokenBytes, len(b), err)
	}
	for _, s := range []*corev1.Secret{ami, tok} {
		if len(s.OwnerReferences) != 0 {
			t.Errorf("Secret %s has owners %+v, want none", s.Name, s.OwnerReferences)
		}
		managed(t, s)
	}
	if len(got) != 2 || !bytes.Equal(got[names.AMIRef()], ami.Data[install.AMIPasswordKey]) || !bytes.Equal(got[names.PushRef()], f.secret(t, names.PushSecret).Data[install.PushPasswordKey]) {
		t.Errorf("Ensure with no devices returned %d values, want only the AMI and push passwords", len(got))
	}
	if _, ok := got[names.AgentTokenRef()]; ok {
		t.Error("Ensure returned the agent token: Compile never reads it")
	}

	// A token written by hand with surrounding white space (kubectl
	// --from-file) is kept, as the agent trims it too.
	f = newFixture(t, registry(t), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.AgentTokenSecret},
		Data:       map[string][]byte{install.AgentTokenKey: []byte("not-a-secret-token+/=\n")},
	})
	if _, err := f.m.Ensure(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if v := string(f.secret(t, names.AgentTokenSecret).Data[install.AgentTokenKey]); v != "not-a-secret-token+/=\n" {
		t.Error("a hand-made agent token was changed")
	}
	// A blank one is filled.
	f = newFixture(t, registry(t), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.AgentTokenSecret},
		Data:       map[string][]byte{install.AgentTokenKey: []byte(" \n")},
	})
	if _, err := f.m.Ensure(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if v := f.secret(t, names.AgentTokenSecret).Data[install.AgentTokenKey]; len(v) != 43 {
		t.Errorf("a blank agent token became %d characters, want a new 43", len(v))
	}
}

func TestTokens(t *testing.T) {
	f := newFixture(t, registry(t))
	ctx := context.Background()
	if _, _, err := f.m.Tokens(ctx); err == nil || !strings.Contains(err.Error(), names.AgentTokenSecret) {
		t.Fatalf("Tokens without the Secret = %v, want an error naming it", err)
	}
	if _, err := f.m.Ensure(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	first := string(f.secret(t, names.AgentTokenSecret).Data[install.AgentTokenKey])
	cur, prev, err := f.m.Tokens(ctx)
	if err != nil || cur != first || prev != "" {
		t.Fatalf("Tokens = (current ok %v, previous %q, %v), want the Secret's token and no previous", cur == first, prev, err)
	}
	// Unchanged: still no previous.
	if cur, prev, _ := f.m.Tokens(ctx); cur != first || prev != "" {
		t.Fatal("Tokens changed without a new token")
	}

	s := f.secret(t, names.AgentTokenSecret)
	s.Data[install.AgentTokenKey] = []byte("not-a-secret-second\n")
	if err := f.c.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	cur, prev, err = f.m.Tokens(ctx)
	if err != nil || cur != "not-a-secret-second" || prev != first {
		t.Fatalf("after a change Tokens = (%q, previous is first %v, %v), want the new token and the first as previous", cur, prev == first, err)
	}
	// The previous one stays until the next change.
	if _, prev, _ := f.m.Tokens(ctx); prev != first {
		t.Fatal("the previous token was forgotten")
	}

	for _, bad := range []string{"", "  \n", "not a secret"} {
		s := f.secret(t, names.AgentTokenSecret)
		s.Data[install.AgentTokenKey] = []byte(bad)
		if err := f.c.Update(ctx, s); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.m.Tokens(ctx); err == nil {
			t.Errorf("Tokens with token %q passed", bad)
		} else if bad != "" && strings.Contains(err.Error(), strings.TrimSpace(bad)) && strings.TrimSpace(bad) != "" {
			t.Errorf("Tokens' error holds the token: %v", err)
		}
	}
}

func TestUnservedModelGetsNoPassword(t *testing.T) {
	reg, err := drivers.NewRegistry(softphone.New())
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, reg)
	got, err := f.m.Ensure(context.Background(), []v1alpha1.Phone{*desk()}, []v1alpha1.Softphone{*laptop()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[compile.PasswordRef(deskID)]; ok {
		t.Error("a Phone no driver serves got a password")
	}
	var s corev1.Secret
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: deskName}, &s); !apierrors.IsNotFound(err) {
		t.Errorf("a Phone no driver serves got a Secret (%v)", err)
	}
	if _, ok := got[compile.PasswordRef(laptopID)]; !ok {
		t.Error("the softphone has no password")
	}

	// A registry without the softphone driver: the softphone still gets its
	// token (every Softphone's ProvisioningTokenRef), but no password.
	reg, err = drivers.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	f = newFixture(t, reg)
	got, err = f.m.Ensure(context.Background(), nil, []v1alpha1.Softphone{*laptop()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[compile.PasswordRef(laptopID)]; ok {
		t.Error("an unserved softphone got a password")
	}
	if len(got[compile.ProvisioningTokenRef(laptopID)]) != install.ProvisioningTokenLength {
		t.Error("an unserved softphone has no provisioning token")
	}
}

func TestEnsureRefusesUnusableSecrets(t *testing.T) {
	const value = "bad;value"
	other := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "someone", UID: "uid-x", Controller: ptr.To(true)}
	for name, s := range map[string]*corev1.Secret{
		"value outside the alphabet": {
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: deskName},
			Data:       map[string][]byte{compile.SecretKeySIPPassword: []byte(value)},
		},
		"controlled by another object": {
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: deskName, OwnerReferences: []metav1.OwnerReference{other}},
			Data:       map[string][]byte{compile.SecretKeySIPPassword: []byte("notAsecret12")},
		},
		"not opaque": {
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: deskName},
			Type:       corev1.SecretTypeBasicAuth,
			Data:       map[string][]byte{compile.SecretKeySIPPassword: []byte("notAsecret12")},
		},
		"immutable and missing a key": {
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: deskName},
			Immutable:  ptr.To(true),
		},
		"AMI password outside the alphabet": {
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.AMISecret},
			Data:       map[string][]byte{install.AMIPasswordKey: []byte(value)},
		},
		"push password outside the alphabet": {
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.PushSecret},
			Data:       map[string][]byte{install.PushPasswordKey: []byte(value)},
		},
		"agent token with a space": {
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.AgentTokenSecret},
			Data:       map[string][]byte{install.AgentTokenKey: []byte("bad value")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, registry(t), desk(), s)
			before := f.secret(t, s.Name)
			_, err := f.m.Ensure(context.Background(), []v1alpha1.Phone{*desk()}, nil)
			if !errors.Is(err, secrets.ErrInvalid) {
				t.Fatalf("Ensure = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), s.Name) {
				t.Errorf("error %q does not name the Secret", err)
			}
			for _, v := range []string{value, "bad value"} {
				if strings.Contains(err.Error(), v) || strings.Contains(f.log.String(), v) {
					t.Errorf("the value appears in the error or the log: %v\n%s", err, f.log)
				}
			}
			after := f.secret(t, s.Name)
			if after.ResourceVersion != before.ResourceVersion {
				t.Error("the unusable Secret was written")
			}
		})
	}
}

func TestCreateAlreadyExistsIsAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
		},
	}).Build()
	m, err := secrets.New(secrets.Config{Client: c, Namespace: ns, Names: names, Registry: registry(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), nil, nil); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Ensure = %v, want the AlreadyExists error", err)
	}
}

func TestAPIErrorsReturned(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	for name, funcs := range map[string]interceptor.Funcs{
		"get": {Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		}},
		"update": {Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return boom
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: names.AMISecret},
			}).WithInterceptorFuncs(funcs).Build()
			m, err := secrets.New(secrets.Config{Client: c, Namespace: ns, Names: names, Registry: registry(t)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Ensure(context.Background(), nil, nil); !errors.Is(err, boom) || errors.Is(err, secrets.ErrInvalid) {
				t.Fatalf("Ensure = %v, want the API error", err)
			}
			if _, _, err := m.Tokens(context.Background()); name == "get" && !errors.Is(err, boom) {
				t.Fatalf("Tokens = %v, want the API error", err)
			}
		})
	}
}

func TestRandIsTheSource(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	m, err := secrets.New(secrets.Config{
		Client: c, Namespace: ns, Names: names, Registry: registry(t),
		Rand: zeros{},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Ensure(context.Background(), []v1alpha1.Phone{*desk()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Byte 0 is the alphabet's first character.
	if v := string(got[compile.PasswordRef(deskID)]); v != strings.Repeat("A", 12) {
		t.Errorf("password from a zero reader = %q, want 12 A", v)
	}

	failing := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	m, err = secrets.New(secrets.Config{Client: failing, Namespace: ns, Names: names, Registry: registry(t), Rand: broken{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), nil, nil); err == nil {
		t.Error("Ensure with a failing reader passed")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type broken struct{}

func (broken) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestNewValidates(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	for name, cfg := range map[string]secrets.Config{
		"no client":    {Namespace: ns, Names: names, Registry: registry(t)},
		"no namespace": {Client: c, Names: names, Registry: registry(t)},
		"bad names":    {Client: c, Namespace: ns, Names: install.NewNames("Bad_Name"), Registry: registry(t)},
		"no registry":  {Client: c, Namespace: ns, Names: names},
	} {
		if _, err := secrets.New(cfg); err == nil {
			t.Errorf("New with %s passed", name)
		}
	}
}

func TestNoSecretLogged(t *testing.T) {
	f := newFixture(t, registry(t), desk(), laptop(), &corev1.Secret{
		// Adopted and filled: both log.
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: laptopSec},
		Data:       map[string][]byte{compile.SecretKeySIPPassword: []byte("notAsecretPasswordAbcdef")},
	})
	ctx := context.Background()
	values, err := f.m.Ensure(ctx, []v1alpha1.Phone{*desk()}, []v1alpha1.Softphone{*laptop()})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.Delete(ctx, f.secret(t, deskName)); err != nil {
		t.Fatal(err)
	}
	again, err := f.m.Ensure(ctx, []v1alpha1.Phone{*desk()}, []v1alpha1.Softphone{*laptop()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Tokens(ctx); err != nil {
		t.Fatal(err)
	}
	tok := f.secret(t, names.AgentTokenSecret)
	firstToken := tok.Data[install.AgentTokenKey]
	tok.Data[install.AgentTokenKey] = []byte("not-a-secret-rotated")
	if err := f.c.Update(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Tokens(ctx); err != nil {
		t.Fatal(err)
	}

	logged := f.log.String()
	if !strings.Contains(logged, "created Secret") || !strings.Contains(logged, "updated Secret") {
		t.Fatalf("the log holds neither a create nor an update; the test sees nothing:\n%s", logged)
	}
	all := [][]byte{firstToken, []byte("not-a-secret-rotated")}
	for _, v := range values {
		all = append(all, v)
	}
	for _, v := range again {
		all = append(all, v)
	}
	for _, v := range all {
		if strings.Contains(logged, string(v)) {
			t.Errorf("a secret value appears in the log:\n%s", logged)
		}
	}
}
