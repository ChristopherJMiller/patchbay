package bundle_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/bundle"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/secrets"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// These run without envtest (-short too).

func unitConfig(t *testing.T) bundle.Config {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().Build()
	sm, err := secrets.New(secrets.Config{Client: c, Namespace: "telephony", Names: names, Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	return bundle.Config{
		Client: c, Namespace: "telephony", Names: names, Settings: settings(), Registry: reg,
		Secrets: sm, Recorder: &recorder{},
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := bundle.New(unitConfig(t)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*bundle.Config){
		"no client":    func(c *bundle.Config) { c.Client = nil },
		"no namespace": func(c *bundle.Config) { c.Namespace = "" },
		"bad names":    func(c *bundle.Config) { c.Names = install.NewNames("Not_A_Name") },
		"another install's settings": func(c *bundle.Config) {
			c.Settings.AMISecret = install.NewNames("other").AMIRef()
		},
		"invalid settings": func(c *bundle.Config) { c.Settings.SIPPort = 0 },
		"no registry":      func(c *bundle.Config) { c.Registry = nil },
		"no secrets":       func(c *bundle.Config) { c.Secrets = nil },
		"no recorder":      func(c *bundle.Config) { c.Recorder = nil },
	} {
		cfg := unitConfig(t)
		mutate(&cfg)
		if _, err := bundle.New(cfg); err == nil {
			t.Errorf("New with %s passed", name)
		}
	}
}

func TestKeyIsTheInstall(t *testing.T) {
	r, err := bundle.New(unitConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if k := r.Key(); k.Namespace != "telephony" || k.Name != names.ConfigMap {
		t.Fatalf("Key = %v, want telephony/%s", k, names.ConfigMap)
	}
}

func TestWatchPredicates(t *testing.T) {
	r, err := bundle.New(unitConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	owned := func(apiVersion, kind string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: "x", UID: "u"}}
	}
	secret := func(namespace, name string, owners []metav1.OwnerReference) client.Object {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, OwnerReferences: owners}}
	}
	gv := v1alpha1.GroupVersion.String()
	for _, tc := range []struct {
		name string
		obj  client.Object
		want bool
	}{
		{"AMI", secret("telephony", names.AMISecret, nil), true},
		{"agent token", secret("telephony", names.AgentTokenSecret, nil), true},
		{"pbx-secrets", secret("telephony", names.ProjectedSecret, nil), true},
		{"owned by a Phone", secret("telephony", "patchbay-phone-desk", owned(gv, string(drivers.KindPhone))), true},
		{"owned by a Softphone", secret("telephony", "patchbay-softphone-laptop", owned(gv, string(drivers.KindSoftphone))), true},
		{"owned by a Person", secret("telephony", "x", owned(gv, "Person")), false},
		{"owned by another group's Phone", secret("telephony", "x", owned("example.com/v1", "Phone")), false},
		{"broken owner apiVersion", secret("telephony", "x", owned("a/b/c", "Phone")), false},
		{"unrelated", secret("telephony", "registry-creds", nil), false},
		{"another namespace", secret("other", names.AMISecret, nil), false},
	} {
		if got := r.WatchesSecret(tc.obj); got != tc.want {
			t.Errorf("WatchesSecret(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	cm := func(namespace, name string) client.Object {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	}
	for _, tc := range []struct {
		obj  client.Object
		want bool
	}{
		{cm("telephony", names.ConfigMap), true},
		{cm("telephony", names.Settings), false},
		{cm("other", names.ConfigMap), false},
	} {
		if got := r.WatchesConfigMap(tc.obj); got != tc.want {
			t.Errorf("WatchesConfigMap(%s/%s) = %v, want %v", tc.obj.GetNamespace(), tc.obj.GetName(), got, tc.want)
		}
	}
}
