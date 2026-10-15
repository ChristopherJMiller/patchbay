package compile_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
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

// compileOK compiles and fails the test on an error.
func compileOK(t *testing.T, reg *drivers.Registry, in compile.Inputs, s compile.Settings) compile.Bundle {
	t.Helper()
	b, err := compile.Compile(reg, in, s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return b
}

func TestCompileWithFakes(t *testing.T) {
	reg := fakeRegistry()

	t.Run("house", func(t *testing.T) {
		in, s := fakeHouse()
		b := compileOK(t, reg, in, s)
		desk, laptop := b.Devices["phone-desk"], b.Devices["softphone-laptop"]
		if desk.Number != "100" || desk.MAC != "D0C78915E8C8" || desk.ConfigFile != "tftp/phone-desk.cfg" ||
			!slices.Equal(desk.Files, []string{"tftp/phone-desk.cfg", "tftp/shared.cfg"}) || desk.ProvisioningToken != nil {
			t.Errorf("desk index entry %+v", desk)
		}
		if laptop.Number != "150" || laptop.MAC != "" || laptop.ConfigFile != "provisioning/softphone-laptop.xml" ||
			laptop.ProvisioningToken == nil || *laptop.ProvisioningToken != compile.ProvisioningTokenRef("softphone-laptop") {
			t.Errorf("laptop index entry %+v", laptop)
		}
		if b.AMI != (compile.AMIAccount{Username: compile.AMIUsername, Password: s.AMISecret}) {
			t.Errorf("AMI %+v", b.AMI)
		}
		// The second render carries the device's own version as its stamp.
		if !strings.Contains(string(b.Files["tftp/phone-desk.cfg"]), "stamp="+desk.Version+"\n") {
			t.Errorf("desk's file is not stamped with its version %s:\n%s", desk.Version, b.Files["tftp/phone-desk.cfg"])
		}
		// The render context, from the settings.
		if want := "tz=America/Los_Angeles patterns=600,911,1XX,3XX,7XX,2XX,0,9.,*8 directory=http://192.168.0.240:8088/directory.xml ntp=192.168.20.1\n"; string(b.Files["tftp/shared.cfg"]) != want {
			t.Errorf("shared.cfg = %q, want %q", b.Files["tftp/shared.cfg"], want)
		}
		ext := string(b.Files["asterisk/extensions.conf"])
		if !strings.Contains(ext, "exten => 301,1,Dial(PJSIP/phone-desk&PJSIP/softphone-laptop,30)\n") {
			t.Errorf("person 301 does not ring both:\n%s", ext)
		}
	})

	t.Run("a device the driver calls invalid is indexed, not rendered", func(t *testing.T) {
		in, s := fakeHouse()
		in.Phones = append(in.Phones, phone("lobby", 3, modelInvalid, "D0C78915A001", "110", "Lobby"),
			phone("attic", 4, "no-such-model", "D0C78915A002", "111", "Attic"))
		in.People = append(in.People, person("front", 5, "310", "Front", ringPhone("lobby"), ringPhone("attic")))
		fakeSecrets(&in, s)
		b := compileOK(t, reg, in, s)
		for _, id := range []string{"phone-lobby", "phone-attic"} {
			d := b.Devices[id]
			if d.MAC == "" || d.Number != "" || d.Version != "" || len(d.Files) != 0 {
				t.Errorf("%s: index entry %+v, want MAC only", id, d)
			}
			if strings.Contains(string(b.Files["asterisk/pjsip.conf"]), "["+id+"]") {
				t.Errorf("%s has a pjsip section", id)
			}
		}
		if ext := string(b.Files["asterisk/extensions.conf"]); !strings.Contains(ext, "; Person front: no routed device to ring\nexten => 310,1,Goto(patchbay-unavailable,s,1)\n") {
			t.Errorf("person 310 should play unavailable:\n%s", ext)
		}
	})

	t.Run("a shared path rendered identically is kept once", func(t *testing.T) {
		in, s := fakeHouse()
		in.Phones = append(in.Phones, phone("kitchen", 3, modelPhone, "D0C78915EBB5", "101", "Kitchen"))
		fakeSecrets(&in, s)
		b := compileOK(t, reg, in, s)
		if !slices.Contains(b.Devices["phone-kitchen"].Files, "tftp/shared.cfg") || !slices.Contains(b.Devices["phone-desk"].Files, "tftp/shared.cfg") {
			t.Errorf("both phones should list tftp/shared.cfg: %+v", b.Devices)
		}
	})

	t.Run("user text cannot form a placeholder", func(t *testing.T) {
		in, s := fakeHouse()
		in.Phones[0].Spec.Label = "@@secret:patchbay-ami/password@@"
		in.People[0].Spec.Name = "@@@secret:patchbay-phone-desk/sip-password@@@"
		b := compileOK(t, reg, in, s)
		for p, content := range b.Files {
			for _, m := range compile.PlaceholderPattern.FindAllStringSubmatch(string(content), -1) {
				if ref := (compile.SecretRef{Name: m[1], Key: m[2]}); ref != s.AMISecret && ref != compile.PasswordRef("phone-desk") && ref != compile.PasswordRef("softphone-laptop") {
					t.Errorf("%s: placeholder %s from user text", p, m[0])
				}
			}
		}
		if !strings.Contains(string(b.Files["tftp/phone-desk.cfg"]), "label=@secret:patchbay-ami/password@ ") {
			t.Errorf("label not neutralized:\n%s", b.Files["tftp/phone-desk.cfg"])
		}
	})

	for _, tc := range []struct {
		name  string
		edit  func(in *compile.Inputs, s *compile.Settings)
		is    error
		match string
	}{
		{"a plain Render error fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.Phones = append(in.Phones, phone("bad", 3, modelBroken, "D0C78915A001", "110", "Bad"))
		}, nil, "Phone bad: fake: broken"},
		{"a file outside the kind's tree fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.Phones = append(in.Phones, phone("out", 3, modelOutside, "D0C78915A001", "110", "Out"))
		}, nil, "http/phone-out.cfg is outside tftp/"},
		{"a shared path rendered differently fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.Phones = append(in.Phones, phone("a", 3, modelClash, "D0C78915A001", "110", "A"),
				phone("b", 4, modelClash, "D0C78915A002", "111", "B"))
		}, nil, "tftp/shared.cfg is rendered differently for devices"},
		{"another device's secret fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.Phones = append(in.Phones, phone("leak", 3, modelLeak, "D0C78915A001", "110", "Leak"))
		}, nil, "reference secret patchbay-ami key password, which is not its own"},
		{"a second render with other files fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.Phones = append(in.Phones, phone("wobbly", 3, modelUnstable, "D0C78915A001", "110", "Wobbly"))
		}, nil, "the second render gave other files"},
		{"a missing config document fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.Phones = append(in.Phones, phone("odd", 3, modelNoConfig, "D0C78915A001", "110", "Odd"))
		}, nil, "its config document tftp/phone-odd.cfg is not among its files"},
		{"two devices with one ID fail", func(in *compile.Inputs, _ *compile.Settings) {
			long := strings.Repeat("a", 40)
			crafted := strings.TrimPrefix(drivers.DeviceID(drivers.KindPhone, long), "phone-")
			in.Phones = append(in.Phones, phone(long, 3, modelPhone, "D0C78915A001", "110", "Long"),
				phone(crafted, 4, modelPhone, "D0C78915A002", "111", "Crafted"))
		}, nil, "have the same device ID"},
		{"a missing password fails", func(in *compile.Inputs, _ *compile.Settings) {
			delete(in.Secrets, compile.PasswordRef("phone-desk"))
		}, compile.ErrMissingSecret, "patchbay-phone-desk key sip-password"},
		{"an empty password fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.Secrets[compile.PasswordRef("softphone-laptop")] = []byte{}
		}, compile.ErrMissingSecret, "patchbay-softphone-laptop key sip-password"},
		{"a missing token fails", func(in *compile.Inputs, _ *compile.Settings) {
			delete(in.Secrets, compile.ProvisioningTokenRef("softphone-laptop"))
		}, compile.ErrMissingSecret, "provisioning-token"},
		{"a missing AMI secret fails", func(in *compile.Inputs, s *compile.Settings) {
			delete(in.Secrets, s.AMISecret)
		}, compile.ErrMissingSecret, "patchbay-ami key password"},
		{"invalid settings fail", func(_ *compile.Inputs, s *compile.Settings) {
			s.SIPPort = 0
		}, nil, "sipPort 0 is not a port"},
		{"a name that is not a DNS subdomain fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.People = append(in.People, person("Front Door", 3, "302", "Front", ringPhone("desk")))
		}, nil, `Person "Front Door"`},
		{"a name listed twice fails", func(in *compile.Inputs, _ *compile.Settings) {
			in.Softphones = append(in.Softphones, softphone("laptop", 3, "151", "Again"))
		}, nil, `Softphone "laptop" is listed twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, s := fakeHouse()
			tc.edit(&in, &s)
			for _, p := range in.Phones {
				id := drivers.DeviceID(drivers.KindPhone, p.Name)
				if _, ok := in.Secrets[compile.PasswordRef(id)]; !ok && p.Name != "desk" {
					in.Secrets[compile.PasswordRef(id)] = []byte("not-a-secret-" + id)
				}
			}
			_, err := compile.Compile(reg, in, s)
			if err == nil {
				t.Fatal("Compile succeeded; want an error")
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("error %v, want one wrapping %v", err, tc.is)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Errorf("error %q, want it to say %q", err, tc.match)
			}
		})
	}

	t.Run("no registry fails", func(t *testing.T) {
		in, s := fakeHouse()
		if _, err := compile.Compile(nil, in, s); err == nil {
			t.Fatal("Compile(nil registry) succeeded")
		}
	})
}

// TestCompileDeterministic: the same inputs, in any order, give the same
// bundle byte for byte.
func TestCompileDeterministic(t *testing.T) {
	reg := fakeRegistry()
	in, s := fakeHouse()
	in.Phones = append(in.Phones,
		phone("kitchen", 3, modelPhone, "D0C78915EBB5", "101", "Kitchen"),
		phone("garage", 3, modelPhone, "D0C78915EBB6", "102", "Garage"),
		phone("clash", 4, modelPhone, "d0c78915ebb6", "103", "Clash"), // garage's MAC
		phone("late", 5, modelPhone, "D0C78915EBB7", "101", "Late"))   // kitchen's number
	in.Softphones = append(in.Softphones, softphone("tablet", 3, "151", "Tablet"), softphone("phone", 3, "152", "Phone"))
	in.People = append(in.People, person("guests", 3, "302", "Guests", ringPhone("kitchen"), ringPhone("late")))
	in.Streams = []v1alpha1.Stream{stream("king5", 3, "705"), stream("kiro7", 3, "707")}
	fakeSecrets(&in, s)
	first := compileOK(t, reg, in, s)
	if again := compileOK(t, reg, in, s); !reflect.DeepEqual(first, again) {
		t.Fatal("two compiles of the same inputs differ")
	}
	shuffled := in
	shuffled.Phones = reversed(in.Phones)
	shuffled.Softphones = reversed(in.Softphones)
	shuffled.People = reversed(in.People)
	shuffled.Streams = reversed(in.Streams)
	if got := compileOK(t, reg, shuffled, s); !reflect.DeepEqual(first, got) {
		t.Fatal("reordering the inputs changed the bundle")
	}
	// Compile does not change its inputs.
	if in.Phones[0].Name != "desk" || in.Phones[0].Spec.Label != "Desk" {
		t.Fatal("Compile changed its inputs")
	}
}

func reversed[T any](s []T) []T {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}
