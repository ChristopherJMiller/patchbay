package install_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

const ns = "telephony"

var (
	names    = install.NewNames("patchbay")
	deskID   = drivers.DeviceID(drivers.KindPhone, "desk")
	laptopID = drivers.DeviceID(drivers.KindSoftphone, "laptop")
)

func TestNewNames(t *testing.T) {
	want := install.Names{
		Fullname:         "patchbay",
		ConfigMap:        "patchbay-pbx-config",
		ProjectedSecret:  "patchbay-pbx-secrets",
		AgentTokenSecret: "patchbay-agent-token",
		AMISecret:        "patchbay-ami",
		Settings:         "patchbay-settings",
		AgentService:     "patchbay-agent",
	}
	if names != want {
		t.Fatalf("NewNames = %+v, want %+v", names, want)
	}
	if err := names.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := names.AMIRef(); got != (compile.SecretRef{Name: "patchbay-ami", Key: "password"}) {
		t.Errorf("AMIRef = %+v", got)
	}
	if got := names.AgentTokenRef(); got != (compile.SecretRef{Name: "patchbay-agent-token", Key: "token"}) {
		t.Errorf("AgentTokenRef = %+v", got)
	}
	for _, bad := range []string{"", strings.Repeat("a", install.MaxFullnameLength+1), "Patchbay", "patch.bay", "-patchbay"} {
		if err := install.NewNames(bad).Validate(); err == nil {
			t.Errorf("NewNames(%q).Validate() = nil, want an error", bad)
		}
	}
	if err := install.NewNames(strings.Repeat("a", install.MaxFullnameLength)).Validate(); err != nil {
		t.Errorf("a %d-character fullname: %v", install.MaxFullnameLength, err)
	}
}

func settings() compile.Settings {
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
		AMISecret:       names.AMIRef(),
	}
}

func TestCheckSettings(t *testing.T) {
	if err := install.CheckSettings(settings(), names); err != nil {
		t.Fatal(err)
	}
	other := settings()
	other.AMISecret = install.NewNames("telephony-patchbay").AMIRef()
	if err := install.CheckSettings(other, names); err == nil || !strings.Contains(err.Error(), "fullname") {
		t.Errorf("another install's AMI Secret: %v, want a fullname error", err)
	}
	invalid := settings()
	invalid.SIPPort = 0
	if err := install.CheckSettings(invalid, names); err == nil {
		t.Error("invalid settings passed")
	}
}

func TestDecideSettings(t *testing.T) {
	got := install.DecideSettings(settings())
	want := drivers.DecideSettings{Signalling: drivers.SignallingPJSIP, StaleAfter: 30 * time.Second, PBXAddress: "192.168.0.240"}
	if got != want {
		t.Fatalf("DecideSettings = %+v, want %+v", got, want)
	}
	if install.PollInterval != 5*time.Second {
		t.Errorf("PollInterval = %s, want 5s (plan §1.4)", install.PollInterval)
	}
}

func TestURLs(t *testing.T) {
	s := settings()
	if got := install.DeviceHTTPURL(s); got != "http://192.168.0.240:8088" {
		t.Errorf("DeviceHTTPURL = %q", got)
	}
	if got := install.ProvisioningURL(s, "tok"); got != "http://192.168.0.240:8088/p/tok" {
		t.Errorf("ProvisioningURL = %q", got)
	}
	if got := install.AgentURL(names, ns, 9440); got != "http://patchbay-agent.telephony.svc:9440" {
		t.Errorf("AgentURL = %q", got)
	}
	// The base is the one Compile renders into device files.
	b, _ := compileFixture(t)
	sep := string(b.Files["tftp/SEP0200E2E00001.cnf.xml"])
	if want := "<directoryURL>" + install.DeviceHTTPURL(s) + "/directory.xml</directoryURL>"; !strings.Contains(sep, want) {
		t.Errorf("the SEP file does not hold %s:\n%s", want, sep)
	}
}

// compileFixture compiles Phone desk and Softphone laptop with generated
// secrets, as the bundle reconciler will.
func compileFixture(t *testing.T) (compile.Bundle, map[compile.SecretRef][]byte) {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	created := metav1.NewTime(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	values := map[compile.SecretRef][]byte{}
	gen := func(ref compile.SecretRef, f func() (string, error)) {
		v, err := f()
		if err != nil {
			t.Fatal(err)
		}
		values[ref] = []byte(v)
	}
	gen(compile.PasswordRef(deskID), func() (string, error) {
		return install.GeneratePassword(nil, drivers.PasswordPolicy{Length: 12, Alphabet: drivers.Alphanumeric})
	})
	gen(compile.PasswordRef(laptopID), func() (string, error) {
		return install.GeneratePassword(nil, drivers.PasswordPolicy{Length: 24, Alphabet: drivers.Alphanumeric})
	})
	gen(compile.ProvisioningTokenRef(laptopID), func() (string, error) { return install.GenerateProvisioningToken(nil) })
	gen(names.AMIRef(), func() (string, error) { return install.GenerateAMIPassword(nil) })
	b, err := compile.Compile(reg, compile.Inputs{
		Phones: []v1alpha1.Phone{{
			ObjectMeta: metav1.ObjectMeta{Name: "desk", CreationTimestamp: created},
			Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100", Label: "Desk"},
		}},
		Softphones: []v1alpha1.Softphone{{
			ObjectMeta: metav1.ObjectMeta{Name: "laptop", CreationTimestamp: created},
			Spec:       v1alpha1.SoftphoneSpec{Number: "150"},
		}},
		Secrets: values,
	}, settings())
	if err != nil {
		t.Fatal(err)
	}
	return b, values
}

func TestConfigMapAndProjectedSecret(t *testing.T) {
	b, values := compileFixture(t)
	cm, err := install.ConfigMap(names, ns, b)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := install.ProjectedSecret(names, ns, b, values)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []metav1.Object{cm, sec} {
		if o.GetNamespace() != ns || o.GetAnnotations()[install.AnnotationBundleVersion] != b.Version ||
			!maps.Equal(o.GetLabels(), install.ManagedLabels()) {
			t.Errorf("%s: namespace %q, labels %v, annotations %v", o.GetName(), o.GetNamespace(), o.GetLabels(), o.GetAnnotations())
		}
	}
	if cm.Name != names.ConfigMap || sec.Name != names.ProjectedSecret || sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("names %s, %s, type %s", cm.Name, sec.Name, sec.Type)
	}

	// The ConfigMap is the bundle; its index decodes alone.
	back, err := compile.BundleFromConfigMap(cm.Data, cm.BinaryData)
	if err != nil || back.Version != b.Version {
		t.Fatalf("BundleFromConfigMap: version %q, %v", back.Version, err)
	}
	idx, err := install.IndexOf(cm)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Version != b.Version || !slices.Equal(slices.Sorted(maps.Keys(idx.Devices)), []string{deskID, laptopID}) {
		t.Errorf("IndexOf = version %s, devices %v", idx.Version, slices.Sorted(maps.Keys(idx.Devices)))
	}

	// The Secret holds exactly SecretRefs, under their projected keys, with
	// the compiled values, and they give the bundle's version.
	refs, err := b.SecretRefs()
	if err != nil {
		t.Fatal(err)
	}
	got, err := install.SecretValues(sec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(refs) {
		t.Errorf("projected %d keys, want %d", len(got), len(refs))
	}
	for _, r := range refs {
		if !bytes.Equal(got[r], values[r]) {
			t.Errorf("projected %s/%s differs", r.Name, r.Key)
		}
	}
	if v, err := b.VersionWith(got); err != nil || v != b.Version {
		t.Errorf("VersionWith(projected) = %s, %v; want %s", v, err, b.Version)
	}

	// Values that are not the compiled ones, or missing, are refused.
	wrong := maps.Clone(values)
	wrong[compile.PasswordRef(deskID)] = []byte("not-a-secret-x")
	if _, err := install.ProjectedSecret(names, ns, b, wrong); err == nil {
		t.Error("values of another version were projected")
	}
	missing := maps.Clone(values)
	delete(missing, names.AMIRef())
	if _, err := install.ProjectedSecret(names, ns, b, missing); !errors.Is(err, compile.ErrMissingSecret) {
		t.Errorf("a missing value: %v, want ErrMissingSecret", err)
	}
	bad := sec.DeepCopy()
	bad.Data["not-projected"] = []byte("x")
	if _, err := install.SecretValues(bad); err == nil {
		t.Error("a key that is not a projected key decoded")
	}
}

func TestIndexOfRefuses(t *testing.T) {
	for name, data := range map[string]map[string]string{
		"no index":    {},
		"not JSON":    {compile.IndexKey: "{"},
		"bad version": {compile.IndexKey: `{"version":"v1","devices":{}}`},
	} {
		if _, err := install.IndexOf(&corev1.ConfigMap{Data: data}); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	idx, err := install.IndexOf(&corev1.ConfigMap{Data: map[string]string{
		compile.IndexKey: `{"version":"` + strings.Repeat("a", 64) + `"}`,
	}})
	if err != nil || idx.Devices == nil {
		t.Errorf("an index without devices: %+v, %v", idx, err)
	}
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestReadCompiled(t *testing.T) {
	ctx := context.Background()
	if _, err := install.ReadCompiled(ctx, newClient(t), ns, names); !errors.Is(err, install.ErrNotCompiled) {
		t.Fatalf("no ConfigMap: %v, want ErrNotCompiled", err)
	}

	b, values := compileFixture(t)
	cm, err := install.ConfigMap(names, ns, b)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := install.ProjectedSecret(names, ns, b, values)
	if err != nil {
		t.Fatal(err)
	}
	token := string(values[compile.ProvisioningTokenRef(laptopID)])

	c, err := install.ReadCompiled(ctx, newClient(t, cm.DeepCopy()), ns, names)
	if err != nil {
		t.Fatal(err)
	}
	if c.Index.Version != b.Version || c.Secrets != nil || c.ConfigMap == nil || c.ConfigMap.Name != names.ConfigMap {
		t.Errorf("ConfigMap only: version %s, secrets %v, configmap %v", c.Index.Version, c.Secrets != nil, c.ConfigMap != nil)
	}
	if _, ok := c.ProvisioningToken(laptopID); ok {
		t.Error("a token without pbx-secrets")
	}

	c, err = install.ReadCompiled(ctx, newClient(t, cm.DeepCopy(), sec.DeepCopy()), ns, names)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c.ProvisioningToken(laptopID); !ok || got != token {
		t.Errorf("ProvisioningToken(laptop) = %v, want the compiled token", ok)
	}
	if _, ok := c.ProvisioningToken(deskID); ok {
		t.Error("a phone has a provisioning token")
	}
	if d, ok := c.Device(deskID); !ok || d.Version == "" || d.Number != "100" {
		t.Errorf("Device(desk) = %+v, %v", d, ok)
	}

	// pbx-secrets written ahead of its ConfigMap: its values are not used.
	ahead := sec.DeepCopy()
	ahead.Annotations[install.AnnotationBundleVersion] = strings.Repeat("b", 64)
	c, err = install.ReadCompiled(ctx, newClient(t, cm.DeepCopy(), ahead), ns, names)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.ProvisioningToken(laptopID); ok || c.Secrets != nil {
		t.Error("the values of another version were used")
	}

	broken := cm.DeepCopy()
	broken.Data[compile.IndexKey] = "{"
	if _, err := install.ReadCompiled(ctx, newClient(t, broken), ns, names); err == nil || errors.Is(err, install.ErrNotCompiled) {
		t.Errorf("an undecodable index: %v", err)
	}
}

// repeat is a reader of one byte, forever.
type repeat byte

func (r repeat) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestGenerate(t *testing.T) {
	// Every byte maps to alphabet[b % n] unless b would bias the result.
	got, err := install.Generate(bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7}), 4, "ab")
	if err != nil || got != "abab" {
		t.Errorf("Generate = %q, %v", got, err)
	}
	// 62 characters: bytes 248-255 are rejected (256 % 62 = 8).
	r := bytes.NewReader(append(bytes.Repeat([]byte{255}, 8), 0, 61, 62, 247, 1, 2, 3, 4, 5, 6, 7, 8))
	got, err = install.Generate(r, 4, drivers.Alphanumeric)
	if err != nil || got != "A9A9" {
		t.Errorf("Generate with rejected bytes = %q, %v; want A9A9", got, err)
	}
	if _, err := install.Generate(iotest.ErrReader(errors.New("boom")), 4, "ab"); err == nil {
		t.Error("a failing reader was ignored")
	}
	if got, err := install.Generate(repeat(61), 3, drivers.Alphanumeric); err != nil || got != "999" {
		t.Errorf("Generate(61, 61, ...) = %q, %v", got, err)
	}
	for _, bad := range []struct {
		n        int
		alphabet string
	}{{0, "ab"}, {4, "a"}, {4, "aa"}, {4, "é"}} {
		if _, err := install.Generate(nil, bad.n, bad.alphabet); err == nil {
			t.Errorf("Generate(%d, %q) passed", bad.n, bad.alphabet)
		}
	}

	for _, c := range []struct {
		name string
		gen  func() (string, error)
		n    int
	}{
		{"phone password", func() (string, error) {
			return install.GeneratePassword(nil, drivers.PasswordPolicy{Length: 12, Alphabet: drivers.Alphanumeric})
		}, 12},
		{"provisioning token", func() (string, error) { return install.GenerateProvisioningToken(nil) }, 32},
		{"AMI password", func() (string, error) { return install.GenerateAMIPassword(nil) }, 32},
	} {
		v, err := c.gen()
		if err != nil || len(v) != c.n || strings.Trim(v, drivers.Alphanumeric) != "" {
			t.Errorf("%s = %q, %v; want %d alphanumerics", c.name, v, err, c.n)
		}
	}
	if _, err := install.GeneratePassword(nil, drivers.PasswordPolicy{Length: 4, Alphabet: drivers.Alphanumeric}); err == nil {
		t.Error("a policy that fails Validate generated a password")
	}
	tok, err := install.GenerateAgentToken(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != install.AgentTokenBytes || len(tok) != 43 {
		t.Errorf("agent token %d characters, %d bytes, %v", len(tok), len(raw), err)
	}
	if again, _ := install.GenerateAgentToken(nil); again == tok {
		t.Error("two agent tokens are equal")
	}
}

func TestUnknownPhoneMessage(t *testing.T) {
	if got := install.UnknownPhoneMessage("D0C78915EBB5", "192.168.20.101"); got != "unknown phone D0C78915EBB5 requested config from 192.168.20.101" {
		t.Errorf("UnknownPhoneMessage = %q", got)
	}
}
