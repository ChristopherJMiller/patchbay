package compile_test

import (
	"maps"
	"reflect"
	"regexp"
	"slices"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// deviceVersions is each device's version.
func deviceVersions(b compile.Bundle) map[string]string {
	out := map[string]string{}
	for id, d := range b.Devices {
		out[id] = d.Version
	}
	return out
}

// changed lists the devices whose version differs between a and b.
func changed(a, b compile.Bundle) []string {
	va, vb := deviceVersions(a), deviceVersions(b)
	ids := maps.Clone(va)
	maps.Copy(ids, vb)
	var out []string
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		if va[id] != vb[id] {
			out = append(out, id)
		}
	}
	return out
}

// stampPattern is the fake driver's version stamp.
var stampPattern = regexp.MustCompile(`stamp=[0-9a-f]*`)

// unstamped is the files with every version stamp blanked: a device whose
// version changes gets a new stamp (the 79x5's versionStamp), so it
// notices; nothing else may differ when only a secret value changed.
func unstamped(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for p, c := range files {
		out[p] = stampPattern.ReplaceAll(c, []byte("stamp="))
	}
	return out
}

// TestVersions pins what changes which version: a device's version moves
// only when what that device fetches moves (or its own secret), and the
// bundle version moves with anything.
func TestVersions(t *testing.T) {
	reg := fakeRegistry()
	base := func() (compile.Inputs, compile.Settings) {
		in, s := fakeHouse()
		in.Phones = append(in.Phones, phone("kitchen", 3, modelPhone, "D0C78915EBB5", "101", "Kitchen"))
		fakeSecrets(&in, s)
		return in, s
	}
	in, s := base()
	before := compileOK(t, reg, in, s)
	if again := compileOK(t, reg, in, s); again.Version != before.Version || !reflect.DeepEqual(again, before) {
		t.Fatal("the same inputs gave another bundle")
	}

	for _, tc := range []struct {
		name string
		edit func(in *compile.Inputs, s *compile.Settings)
		// devices whose version must change; nil: none.
		devices []string
		// sameFiles: no file may change but the version stamps of the
		// devices whose version changed (only a secret value did).
		sameFiles bool
	}{
		{"a phone's password", func(in *compile.Inputs, _ *compile.Settings) {
			in.Secrets[compile.PasswordRef("phone-desk")] = []byte("not-a-secret-desk-0002")
		}, []string{"phone-desk"}, true},
		{"a softphone's password", func(in *compile.Inputs, _ *compile.Settings) {
			in.Secrets[compile.PasswordRef("softphone-laptop")] = []byte("not-a-secret-laptop-0002")
		}, []string{"softphone-laptop"}, true},
		{"a token rotation", func(in *compile.Inputs, _ *compile.Settings) {
			in.Secrets[compile.ProvisioningTokenRef("softphone-laptop")] = []byte("not-a-secret-token-0002")
		}, nil, true},
		{"the AMI password", func(in *compile.Inputs, s *compile.Settings) {
			in.Secrets[s.AMISecret] = []byte("not-a-secret-ami-0002")
		}, nil, true},
		{"a phone's label", func(in *compile.Inputs, _ *compile.Settings) {
			in.Phones[0].Spec.Label = "Study"
		}, []string{"phone-desk"}, false},
		{"the time zone (every phone's shared file)", func(_ *compile.Inputs, s *compile.Settings) {
			s.Timezone = "America/New_York"
		}, []string{"phone-desk", "phone-kitchen"}, false},
		{"the allowed networks (Asterisk only)", func(_ *compile.Inputs, s *compile.Settings) {
			s.AllowedNetworks = append(s.AllowedNetworks, "10.0.0.0/8")
		}, nil, false},
		{"the RTP ports (Asterisk only)", func(_ *compile.Inputs, s *compile.Settings) {
			s.RTPPorts = compile.PortRange{First: 20000, Last: 20099}
		}, nil, false},
		{"a person (Asterisk and the directory only)", func(in *compile.Inputs, _ *compile.Settings) {
			in.People[0].Spec.Rings = in.People[0].Spec.Rings[:1]
		}, nil, false},
		{"a new device", func(in *compile.Inputs, s *compile.Settings) {
			in.Softphones = append(in.Softphones, softphone("tablet", 9, "151", "Tablet"))
			fakeSecrets(in, *s)
		}, []string{"softphone-tablet"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, s := base()
			tc.edit(&in, &s)
			after := compileOK(t, reg, in, s)
			if after.Version == before.Version {
				t.Error("the bundle version did not change")
			}
			if got := changed(before, after); !slices.Equal(got, tc.devices) {
				t.Errorf("device versions changed for %v, want %v", got, tc.devices)
			}
			if same := reflect.DeepEqual(unstamped(before.Files), unstamped(after.Files)); same != tc.sameFiles {
				t.Errorf("files unchanged = %t, want %t", same, tc.sameFiles)
			}
		})
	}
}
