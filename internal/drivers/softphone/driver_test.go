package softphone_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

func TestDriverIdentity(t *testing.T) {
	d := softphone.New()
	if d.Name() != "softphone" || softphone.Name != "softphone" {
		t.Errorf("Name() = %q", d.Name())
	}
	if got := d.Models(); !slices.Equal(got, []string{drivers.ModelSoftphone}) {
		t.Errorf("Models() = %v, want [softphone]", got)
	}
	if fw := d.Firmware(); fw != nil {
		t.Errorf("Firmware() = %v, want nil", fw)
	}
	if got := softphone.ProvisioningFile("softphone-chris-laptop"); got != "provisioning/softphone-chris-laptop.xml" {
		t.Errorf("ProvisioningFile = %q", got)
	}
}

func TestProfile(t *testing.T) {
	d := softphone.New()
	if _, ok := d.Profile("cisco-7945"); ok {
		t.Error("Profile(cisco-7945) ok, want false")
	}
	p, ok := d.Profile(drivers.ModelSoftphone)
	if !ok {
		t.Fatal("Profile(softphone) not ok")
	}
	if err := drivers.ValidateProfile(drivers.ModelSoftphone, p); err != nil {
		t.Fatalf("ValidateProfile: %v", err)
	}
	want := drivers.Profile{
		Model:    "softphone",
		Hardware: drivers.Hardware{Codecs: []string{"g722", "ulaw", "alaw"}},
		Provisioning: drivers.Provisioning{
			ConfigFile:      "provisioning/{ID}.xml",
			DefaultFirmware: "",
			ScreenLabelMax:  0,
			LineLabelMax:    64,
			Password:        drivers.PasswordPolicy{Length: 24, Alphabet: drivers.Alphanumeric},
			ResyncByNotify:  false,
		},
		// docs/plans/release-a.md §2(a), softphone column.
		Endpoint: drivers.EndpointOptions{
			Transport:          drivers.SIPTransportUDP,
			Codecs:             []string{"g722", "ulaw"},
			ForceRport:         true,
			RewriteContact:     true,
			RTPSymmetric:       true,
			DirectMedia:        false,
			SendRPID:           false,
			SendPAI:            true,
			TrustIDOutbound:    true,
			QualifyFrequency:   0,
			RegistrationExpiry: 600 * time.Second,
		},
		Features: map[drivers.Signalling]map[drivers.Feature]drivers.Support{
			drivers.SignallingPJSIP: {
				drivers.FeatureHold:        drivers.Supported,
				drivers.FeatureTransfer:    drivers.Supported,
				drivers.FeatureWideband:    drivers.Supported,
				drivers.FeatureRFC4733DTMF: drivers.Supported,
			},
		},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("Profile(softphone) =\n%+v\nwant\n%+v", p, want)
	}
	// Absent means Unsupported: Linphone never shows a mid-call caller ID
	// update (it reads no Remote-Party-ID; see profile.go), and none of the
	// Cisco-only features exist.
	for _, f := range []drivers.Feature{drivers.FeatureCallerIDUpdate, drivers.FeatureBLF, drivers.FeaturePark,
		drivers.FeatureIntercom, drivers.FeatureSecondLine} {
		if got := p.Supports(drivers.SignallingPJSIP, f); got != drivers.Unsupported {
			t.Errorf("Supports(pjsip, %s) = %s, want unsupported", f, got)
		}
	}

	// The caller gets a copy: changing it does not change the driver.
	p.Endpoint.Codecs[0] = "alaw"
	p.Hardware.Codecs[0] = "alaw"
	p.Features[drivers.SignallingPJSIP][drivers.FeatureHold] = drivers.Unsupported
	if again, _ := d.Profile(drivers.ModelSoftphone); !reflect.DeepEqual(again, want) {
		t.Error("changing a returned profile changed the driver's")
	}
}
