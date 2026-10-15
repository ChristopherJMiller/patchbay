package cisco79x5_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

func TestDriverIdentity(t *testing.T) {
	d := cisco79x5.New()
	if d.Name() != "cisco79x5" || cisco79x5.Name != "cisco79x5" {
		t.Errorf("Name() = %q", d.Name())
	}
	// The CRD's model enum, in its order.
	want := []string{
		string(v1alpha1.PhoneModelCisco7945),
		string(v1alpha1.PhoneModelCisco7965),
		string(v1alpha1.PhoneModelCisco7975),
	}
	if got := d.Models(); !slices.Equal(got, want) {
		t.Errorf("Models() = %v, want %v", got, want)
	}
	d.Models()[0] = "changed"
	if d.Models()[0] != want[0] {
		t.Error("Models() returns the driver's own slice")
	}
	if _, ok := d.Profile("cisco-7940"); ok {
		t.Error("Profile(cisco-7940) is ok")
	}
	reg, err := drivers.NewRegistry(d)
	if err != nil {
		t.Fatalf("NewRegistry rejects the driver: %v", err)
	}
	if got := reg.Models(); !slices.Equal(got, want) {
		t.Errorf("registry models = %v", got)
	}
}

func TestProfiles(t *testing.T) {
	d := cisco79x5.New()
	for _, m := range d.Models() {
		p, ok := d.Profile(m)
		if !ok {
			t.Fatalf("no profile for %s", m)
		}
		if err := drivers.ValidateProfile(m, p); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}

	p, _ := d.Profile("cisco-7945")
	wantHW := drivers.Hardware{
		LineKeys:           2,
		Softkeys:           4,
		Display:            drivers.Size{Width: 320, Height: 240},
		ServiceArea:        drivers.Size{Width: 298, Height: 156},
		Wallpaper:          drivers.Size{Width: 320, Height: 212},
		WallpaperThumbnail: drivers.Size{Width: 80, Height: 53},
		RingtoneFormat:     "raw µ-law, 8 kHz, 8-bit mono, 240–16080 samples",
		Codecs:             []string{"g722", "ulaw", "alaw", "g729a", "ilbc", "l16"},
	}
	if !reflect.DeepEqual(p.Hardware, wantHW) {
		t.Errorf("7945 hardware = %+v, want %+v", p.Hardware, wantHW)
	}
	wantProv := drivers.Provisioning{
		ConfigFile:      "SEP{MAC}.cnf.xml",
		DefaultFirmware: "SIP45.9-4-2SR4-3S",
		ScreenLabelMax:  12,
		LineLabelMax:    30,
		Password:        drivers.PasswordPolicy{Length: 12, Alphabet: drivers.Alphanumeric},
		ResyncByNotify:  true,
	}
	if !reflect.DeepEqual(p.Provisioning, wantProv) {
		t.Errorf("7945 provisioning = %+v, want %+v", p.Provisioning, wantProv)
	}
	// Plan §2(a).
	wantEP := drivers.EndpointOptions{
		Transport:          drivers.SIPTransportTCP,
		Codecs:             []string{"g722", "ulaw"},
		ForceRport:         false,
		RewriteContact:     false,
		RTPSymmetric:       false,
		DirectMedia:        false,
		SendRPID:           true,
		SendPAI:            false,
		TrustIDOutbound:    true,
		QualifyFrequency:   0,
		RegistrationExpiry: 600 * time.Second,
	}
	if !reflect.DeepEqual(p.Endpoint, wantEP) {
		t.Errorf("7945 endpoint = %+v, want %+v", p.Endpoint, wantEP)
	}

	// Design §8.5 and plan §4 T1: honest levels, every feature listed.
	levels := map[drivers.Support][]drivers.Feature{
		drivers.Supported: {
			drivers.FeatureHold, drivers.FeatureWideband, drivers.FeatureRFC4733DTMF,
			drivers.FeatureCallerIDUpdate, drivers.FeatureDistinctiveRing,
		},
		drivers.Untested: {
			drivers.FeatureTransfer, drivers.FeaturePickup, drivers.FeatureMeetMe, drivers.FeatureSpeedDial,
			drivers.FeatureServiceURL, drivers.FeatureWallpaper, drivers.FeatureCustomRingtones,
			drivers.FeatureXMLServices, drivers.FeaturePushNotification, drivers.FeatureRTPRx,
		},
		drivers.Unsupported: {
			drivers.FeatureBLF, drivers.FeaturePark, drivers.FeatureConference, drivers.FeatureDNDSync,
			drivers.FeatureCallForwardSync, drivers.FeatureIntercom, drivers.FeatureSecondLine,
		},
	}
	listed := 0
	for level, fs := range levels {
		for _, f := range fs {
			if got, ok := p.Features[drivers.SignallingPJSIP][f]; !ok || got != level {
				t.Errorf("pjsip %s = %q (listed %v), want %s", f, got, ok, level)
			}
			listed++
		}
	}
	if got := len(p.Features[drivers.SignallingPJSIP]); got != listed {
		t.Errorf("pjsip feature table has %d entries, want %d", got, listed)
	}
	if len(p.Features) != 1 {
		t.Errorf("feature tables for %d backends, want pjsip only", len(p.Features))
	}

	for model, want := range map[string]struct {
		lineKeys int
		load     string
	}{
		"cisco-7965": {6, "SIP45.9-4-2SR4-3S"},
		"cisco-7975": {8, "SIP75.9-4-2SR4-3S"},
	} {
		q, _ := d.Profile(model)
		if q.Hardware.LineKeys != want.lineKeys || q.Provisioning.DefaultFirmware != want.load {
			t.Errorf("%s: line keys %d, default %s; want %d, %s", model, q.Hardware.LineKeys,
				q.Provisioning.DefaultFirmware, want.lineKeys, want.load)
		}
		if !reflect.DeepEqual(q.Endpoint, wantEP) || !reflect.DeepEqual(q.Features, p.Features) {
			t.Errorf("%s: endpoint or features differ from the 7945's", model)
		}
	}

	// Each call returns a fresh copy.
	p.Hardware.Codecs[0] = "changed"
	p.Features[drivers.SignallingPJSIP][drivers.FeatureBLF] = drivers.Supported
	again, _ := d.Profile("cisco-7945")
	if again.Hardware.Codecs[0] != "g722" || again.Supports(drivers.SignallingPJSIP, drivers.FeatureBLF) != drivers.Unsupported {
		t.Error("Profile returns shared data")
	}
}
