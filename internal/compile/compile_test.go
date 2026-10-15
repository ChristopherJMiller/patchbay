package compile_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

func TestPlaceholderRoundTrip(t *testing.T) {
	for _, ref := range []compile.SecretRef{
		{Name: "phone-desk", Key: "sip-password"},
		{Name: "patchbay.agent", Key: "token"},
		{Name: "softphone-chris-laptop", Key: "provisioning_token"},
	} {
		text := "<authPassword>" + ref.Placeholder() + "</authPassword>"
		m := compile.PlaceholderPattern.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("%q: placeholder not matched in %q", ref, text)
		}
		if got := (compile.SecretRef{Name: m[1], Key: m[2]}); got != ref {
			t.Errorf("parsed %+v, want %+v", got, ref)
		}
	}
}

func TestPlaceholderPatternRejects(t *testing.T) {
	for _, text := range []string{
		"@@secret:Upper/key@@", // Secret names are lower case
		"@@secret:name@@",      // no key
		"@@secret:/key@@",      // no name
		"@@secret:name/key with space@@",
		"@secret:name/key@",
	} {
		if compile.PlaceholderPattern.MatchString(text) {
			t.Errorf("%q matched; want no match", text)
		}
	}
}

func TestDefaultNumberPlan(t *testing.T) {
	p := compile.DefaultNumberPlan()
	if p.Devices != "1XX" || p.People != "3XX" || p.Streams != "7XX" ||
		p.EchoTest != "600" || p.Emergency != "911" || len(p.Reserved) != 4 {
		t.Fatalf("default number plan drifted from design §4: %+v", p)
	}
}

// TestCompileIsAStub pins the contract until the compiler lands: Compile has
// its final signature and says plainly that it is not written yet.
func TestCompileIsAStub(t *testing.T) {
	reg, err := drivers.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	_, err = compile.Compile(reg, compile.Inputs{}, compile.Settings{NumberPlan: compile.DefaultNumberPlan()})
	if !errors.Is(err, compile.ErrNotImplemented) {
		t.Fatalf("Compile error = %v, want ErrNotImplemented", err)
	}
}

// TestDeviceSecrets pins where the controller creates, and the compiler
// references, each device's generated secrets.
func TestDeviceSecrets(t *testing.T) {
	id := drivers.DeviceID(drivers.KindSoftphone, "chris-laptop")
	if got, want := compile.PasswordRef(id), (compile.SecretRef{Name: "patchbay-softphone-chris-laptop", Key: "sip-password"}); got != want {
		t.Errorf("PasswordRef = %+v, want %+v", got, want)
	}
	if got, want := compile.ProvisioningTokenRef(id), (compile.SecretRef{Name: "patchbay-softphone-chris-laptop", Key: "provisioning-token"}); got != want {
		t.Errorf("ProvisioningTokenRef = %+v, want %+v", got, want)
	}
	// The longest device ID still makes a valid Secret name and a
	// placeholder the agent's pattern matches.
	long := drivers.DeviceID(drivers.KindSoftphone, strings.Repeat("a", 253))
	ref := compile.PasswordRef(long)
	if len(ref.Name) > 253 {
		t.Errorf("Secret name %q is longer than 253", ref.Name)
	}
	if m := compile.PlaceholderPattern.FindStringSubmatch(ref.Placeholder()); m == nil || m[1] != ref.Name || m[2] != ref.Key {
		t.Errorf("placeholder %q does not round trip: %v", ref.Placeholder(), m)
	}
}

// TestSettingsJSON pins the settings document's field names: the chart
// renders it and the controller reads it.
func TestSettingsJSON(t *testing.T) {
	raw := `{
		"pbxAddress": "192.168.0.240",
		"sipPort": 5060,
		"rtpPorts": {"first": 10000, "last": 10099},
		"deviceHTTPPort": 8088,
		"ntpServer": "192.168.20.1",
		"timezone": "America/Los_Angeles",
		"signalling": "pjsip",
		"numberPlan": {"devices": "1XX", "people": "3XX", "streams": "7XX", "echoTest": "600",
			"emergency": "911", "reserved": ["2XX", "0", "9.", "*8"]},
		"allowedNetworks": ["192.168.20.0/24", "192.168.0.0/24"],
		"amiSecret": {"name": "patchbay-ami", "key": "password"}
	}`
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var s compile.Settings
	if err := dec.Decode(&s); err != nil {
		t.Fatal(err)
	}
	want := compile.Settings{
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
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("decoded settings:\n got %+v\nwant %+v", s, want)
	}
}
