// Package intaketest builds real bundles for the agent's tests: the release
// A house (desk, chris-laptop, and chris ringing both), compiled with the
// built-in drivers, with fake secrets, and the pbx-config and pbx-secrets
// objects the controller would write for it.
package intaketest

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// Install is one install's resources, settings and secret values.
type Install struct {
	Phones     []v1alpha1.Phone
	Softphones []v1alpha1.Softphone
	People     []v1alpha1.Person
	Settings   compile.Settings
	Secrets    map[compile.SecretRef][]byte
}

// AMIRef is the AMI password's Secret key in every Install.
var AMIRef = compile.SecretRef{Name: "patchbay-ami", Key: "password"}

// PushRef is the push password's Secret key in every Install.
var PushRef = compile.SecretRef{Name: "patchbay-push", Key: "password"}

var created = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// House is the release A deployment: Phone desk (100, D0C78915E8C8),
// Softphone chris-laptop (150) and Person chris (301) ringing both.
func House() *Install {
	in := &Install{
		Settings: compile.Settings{
			PBXAddress:      "192.168.0.240",
			SIPPort:         5060,
			RTPPorts:        compile.PortRange{First: 10000, Last: 10099},
			DeviceHTTPPort:  8088,
			NTPServer:       "192.168.20.1",
			Timezone:        "America/Los_Angeles",
			Signalling:      drivers.SignallingPJSIP,
			NumberPlan:      compile.DefaultNumberPlan(),
			AllowedNetworks: []string{"192.168.20.0/24", "192.168.0.0/24"},
			AMISecret:       AMIRef,
			PushSecret:      PushRef,
		},
		Secrets: map[compile.SecretRef][]byte{
			AMIRef:  []byte("not-a-secret-ami-0001"),
			PushRef: []byte("not-a-secret-push-0001"),
		},
	}
	in.AddPhone("desk", "D0C78915E8C8", "100", "Desk")
	in.AddSoftphone("chris-laptop", "150", "Chris (laptop)")
	in.People = []v1alpha1.Person{{
		ObjectMeta: metav1.ObjectMeta{Name: "chris", CreationTimestamp: metav1.NewTime(created)},
		Spec: v1alpha1.PersonSpec{Number: "301", Name: "Chris", Rings: []v1alpha1.RingTarget{
			{Phone: "desk"}, {Softphone: "chris-laptop"},
		}},
	}}
	return in
}

// AddPhone adds a cisco-7945 with its SIP password.
func (in *Install) AddPhone(name, mac, number, label string) {
	in.Phones = append(in.Phones, v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created.Add(time.Duration(len(in.Phones)) * time.Minute))},
		Spec: v1alpha1.PhoneSpec{
			Model: v1alpha1.PhoneModelCisco7945, MAC: v1alpha1.MAC(mac), Number: v1alpha1.Number(number), Label: label,
		},
	})
	id := drivers.DeviceID(drivers.KindPhone, name)
	in.Secrets[compile.PasswordRef(id)] = []byte("not-a-secret-" + name + "-0001")
}

// AddSoftphone adds a softphone with its SIP password and token.
func (in *Install) AddSoftphone(name, number, label string) {
	in.Softphones = append(in.Softphones, v1alpha1.Softphone{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created.Add(time.Hour + time.Duration(len(in.Softphones))*time.Minute))},
		Spec:       v1alpha1.SoftphoneSpec{Number: v1alpha1.Number(number), Label: label},
	})
	id := drivers.DeviceID(drivers.KindSoftphone, name)
	in.Secrets[compile.PasswordRef(id)] = []byte("not-a-secret-" + name + "-0001")
	in.Secrets[compile.ProvisioningTokenRef(id)] = []byte("not-a-secret-token-" + name + "-0001")
}

// Compile compiles the install with the built-in drivers.
func (in *Install) Compile(t testing.TB) compile.Bundle {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	b, err := compile.Compile(reg, compile.Inputs{
		Phones: in.Phones, Softphones: in.Softphones, People: in.People, Secrets: in.Secrets,
	}, in.Settings)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// SecretData is the projected Secret's data for b: every SecretRefs key,
// as the controller writes it.
func (in *Install) SecretData(t testing.TB, b compile.Bundle) map[string][]byte {
	t.Helper()
	refs, err := b.SecretRefs()
	if err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{}
	for _, r := range refs {
		key, err := r.ProjectedKey()
		if err != nil {
			t.Fatal(err)
		}
		v, ok := in.Secrets[r]
		if !ok {
			t.Fatalf("no test value for %s/%s", r.Name, r.Key)
		}
		data[key] = slices.Clone(v)
	}
	return data
}

// Resolved compiles and resolves the install, as intake hands it to apply.
func (in *Install) Resolved(t testing.TB) intake.Resolved {
	t.Helper()
	b := in.Compile(t)
	r, err := intake.Resolve(b, in.SecretData(t, b))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Values lists every secret value, sorted, for "never in a log" checks.
func (in *Install) Values() []string {
	var out []string
	for _, v := range in.Secrets {
		out = append(out, string(v))
	}
	slices.Sort(out)
	return out
}

// ConfigMap is pbx-config for b, as the controller writes it.
func ConfigMap(t testing.TB, namespace, name string, b compile.Bundle) *corev1.ConfigMap {
	t.Helper()
	data, binary, err := b.ConfigMapData()
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       data, BinaryData: binary,
	}
}

// Secret is pbx-secrets with data, at resource version rv.
func Secret(namespace, name, rv string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: rv},
		Data:       data,
	}
}

// Version is a valid bundle version made of one repeated hex digit, for
// tests that need versions but no bundle.
func Version(digit byte) string { return strings.Repeat(string(digit), 64) }
