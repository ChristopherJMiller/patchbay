package compile_test

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// t0 is the instant every test time is relative to.
var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// Fake models. fakeDriver serves each; what it renders is chosen by model.
const (
	modelPhone    = "fake-phone"    // tftp/<ID>.cfg and the shared tftp/shared.cfg
	modelInvalid  = "fake-invalid"  // Render fails with drivers.ErrInvalidSpec
	modelBroken   = "fake-broken"   // Render fails with a plain error
	modelOutside  = "fake-outside"  // renders into http/
	modelClash    = "fake-clash"    // tftp/shared.cfg with per-device content
	modelLeak     = "fake-leak"     // references the AMI secret
	modelUnstable = "fake-unstable" // the second render adds a file
	modelNoConfig = "fake-noconfig" // renders a file other than its profile's config file
)

// fakeDriver renders a deterministic file per device into its tree.
type fakeDriver struct{}

var _ drivers.Driver = fakeDriver{}

func (fakeDriver) Name() string { return "fake" }

func (fakeDriver) Models() []string {
	return []string{modelPhone, modelInvalid, modelBroken, modelOutside, modelClash, modelLeak, modelUnstable,
		modelNoConfig, drivers.ModelSoftphone}
}

func (fakeDriver) Profile(model string) (drivers.Profile, bool) {
	p := drivers.Profile{
		Model: model,
		Provisioning: drivers.Provisioning{
			ConfigFile:   "{ID}.cfg",
			LineLabelMax: 30,
			Password:     drivers.PasswordPolicy{Length: 12, Alphabet: drivers.Alphanumeric},
		},
		Endpoint: drivers.EndpointOptions{
			Transport:          drivers.SIPTransportTCP,
			Codecs:             []string{"g722", "ulaw"},
			SendRPID:           true,
			TrustIDOutbound:    true,
			RegistrationExpiry: 600 * time.Second,
		},
	}
	if model == drivers.ModelSoftphone {
		p.Provisioning.ConfigFile = "provisioning/{ID}.xml"
		p.Provisioning.LineLabelMax = 64
		p.Endpoint.Transport = drivers.SIPTransportUDP
		p.Endpoint.ForceRport, p.Endpoint.RewriteContact, p.Endpoint.RTPSymmetric = true, true, true
		p.Endpoint.QualifyFrequency = 60 * time.Second
	}
	return p, true
}

func (fakeDriver) Render(dev drivers.Device, ctx drivers.RenderContext) (drivers.Files, error) {
	own := fmt.Sprintf("id=%s number=%s label=%s password=%s stamp=%s\n", dev.ID, dev.Number, dev.Label, ctx.SIPPassword, dev.ConfigVersion)
	shared := fmt.Sprintf("tz=%s patterns=%s directory=%s services=%s ntp=%s\n", ctx.Timezone, strings.Join(ctx.DialPatterns, ","), ctx.DirectoryURL, ctx.ServicesURL, ctx.NTPServer)
	switch dev.Model {
	case modelPhone:
		return drivers.Files{"tftp/" + dev.ID + ".cfg": []byte(own), "tftp/shared.cfg": []byte(shared)}, nil
	case drivers.ModelSoftphone:
		doc := fmt.Sprintf("id=%s number=%s password=%s pbx=%s:%d\n", dev.ID, dev.Number, ctx.SIPPassword, ctx.PBXAddress, ctx.SIPPort)
		return drivers.Files{"provisioning/" + dev.ID + ".xml": []byte(doc)}, nil
	case modelInvalid:
		return nil, fmt.Errorf("%w: fake says no", drivers.ErrInvalidSpec)
	case modelBroken:
		return nil, fmt.Errorf("fake: broken")
	case modelOutside:
		return drivers.Files{"tftp/" + dev.ID + ".cfg": []byte(own), "http/" + dev.ID + ".cfg": []byte(own)}, nil
	case modelClash:
		return drivers.Files{"tftp/" + dev.ID + ".cfg": []byte(own), "tftp/shared.cfg": []byte(own)}, nil
	case modelLeak:
		return drivers.Files{"tftp/" + dev.ID + ".cfg": []byte(own + compile.SecretRef{Name: "patchbay-ami", Key: "password"}.Placeholder())}, nil
	case modelUnstable:
		files := drivers.Files{"tftp/" + dev.ID + ".cfg": []byte(own)}
		if dev.ConfigVersion != "" {
			files["tftp/"+dev.ID+".extra"] = []byte(own)
		}
		return files, nil
	case modelNoConfig:
		return drivers.Files{"tftp/" + dev.ID + ".other": []byte(own)}, nil
	}
	return nil, fmt.Errorf("%w: fake serves no model %q", drivers.ErrInvalidSpec, dev.Model)
}

func (fakeDriver) Decide(drivers.Device, drivers.Observations, drivers.DecideSettings, time.Time) drivers.Decision {
	return drivers.Decision{}
}

func (fakeDriver) Firmware() []drivers.FirmwareLoad { return nil }

func fakeRegistry() *drivers.Registry {
	reg, err := drivers.NewRegistry(fakeDriver{})
	if err != nil {
		panic(err)
	}
	return reg
}

// meta is ObjectMeta created at t0 plus age minutes.
func meta(name string, minutes int) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(t0.Add(time.Duration(minutes) * time.Minute))}
}

func phone(name string, minutes int, model, mac, number, label string) v1alpha1.Phone {
	return v1alpha1.Phone{ObjectMeta: meta(name, minutes), Spec: v1alpha1.PhoneSpec{
		Model: v1alpha1.PhoneModel(model), MAC: v1alpha1.MAC(mac), Number: v1alpha1.Number(number), Label: label,
	}}
}

func softphone(name string, minutes int, number, label string) v1alpha1.Softphone {
	return v1alpha1.Softphone{ObjectMeta: meta(name, minutes), Spec: v1alpha1.SoftphoneSpec{Number: v1alpha1.Number(number), Label: label}}
}

func person(name string, minutes int, number, display string, rings ...v1alpha1.RingTarget) v1alpha1.Person {
	return v1alpha1.Person{ObjectMeta: meta(name, minutes), Spec: v1alpha1.PersonSpec{
		Number: v1alpha1.Number(number), Name: display, Rings: rings,
	}}
}

func stream(name string, minutes int, number string) v1alpha1.Stream {
	return v1alpha1.Stream{ObjectMeta: meta(name, minutes), Spec: v1alpha1.StreamSpec{
		Number: v1alpha1.Number(number), Name: name, Source: "http://192.0.2.58:5004/auto/v5.1",
	}}
}

func ringPhone(name string) v1alpha1.RingTarget {
	return v1alpha1.RingTarget{Phone: v1alpha1.LocalName(name)}
}
func ringSoftphone(name string) v1alpha1.RingTarget {
	return v1alpha1.RingTarget{Softphone: v1alpha1.LocalName(name)}
}

// fakeSecrets holds an obviously fake value for every device secret of the
// inputs (password, and token for softphones), and the AMI secret.
func fakeSecrets(in *compile.Inputs, s compile.Settings) {
	in.Secrets = map[compile.SecretRef][]byte{s.AMISecret: []byte("not-a-secret-ami-0001"), s.PushSecret: []byte("not-a-secret-push-0001")}
	for _, p := range in.Phones {
		id := drivers.DeviceID(drivers.KindPhone, p.Name)
		in.Secrets[compile.PasswordRef(id)] = []byte("not-a-secret-" + id)
	}
	for _, sp := range in.Softphones {
		id := drivers.DeviceID(drivers.KindSoftphone, sp.Name)
		in.Secrets[compile.PasswordRef(id)] = []byte("not-a-secret-" + id)
		in.Secrets[compile.ProvisioningTokenRef(id)] = []byte("not-a-secret-token-" + id)
	}
}

// houseSettings are the release A install's settings (chart defaults).
func houseSettings() compile.Settings {
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
		AMISecret:       compile.SecretRef{Name: "patchbay-ami", Key: "password"},
		PushSecret:      compile.SecretRef{Name: "patchbay-push", Key: "password"},
	}
}

// fakeHouse is the house with fake drivers: desk, laptop, and a person
// ringing both.
func fakeHouse() (compile.Inputs, compile.Settings) {
	s := houseSettings()
	in := compile.Inputs{
		Phones:     []v1alpha1.Phone{phone("desk", 0, modelPhone, "D0C78915E8C8", "100", "Desk")},
		Softphones: []v1alpha1.Softphone{softphone("laptop", 1, "150", "Chris (laptop)")},
		People:     []v1alpha1.Person{person("chris", 2, "301", "Chris", ringPhone("desk"), ringSoftphone("laptop"))},
	}
	fakeSecrets(&in, s)
	return in, s
}
