package drivers_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// fake is the smallest Driver: it proves the interface is implementable and
// drives the registry tests.
type fake struct {
	name     string
	models   []string
	firmware []drivers.FirmwareLoad
	// profile, when set, replaces the valid default profile.
	profile func(model string) drivers.Profile
	// noProfile lists models it claims but has no profile for.
	noProfile []string
}

var _ drivers.Driver = fake{}

func (f fake) Name() string     { return f.name }
func (f fake) Models() []string { return f.models }
func (f fake) Profile(model string) (drivers.Profile, bool) {
	if !slices.Contains(f.models, model) || slices.Contains(f.noProfile, model) {
		return drivers.Profile{}, false
	}
	if f.profile != nil {
		return f.profile(model), true
	}
	return validProfile(model), true
}

// validProfile is a profile that passes ValidateProfile: the 79x5's
// release A endpoint options and password policy.
func validProfile(model string) drivers.Profile {
	return drivers.Profile{
		Model: model,
		Provisioning: drivers.Provisioning{
			Password: drivers.PasswordPolicy{Length: 12, Alphabet: drivers.Alphanumeric},
		},
		Endpoint: drivers.EndpointOptions{
			Transport:          drivers.SIPTransportTCP,
			Codecs:             []string{"g722", "ulaw"},
			SendRPID:           true,
			TrustIDOutbound:    true,
			RegistrationExpiry: time.Hour,
		},
	}
}
func (fake) Render(drivers.Device, drivers.RenderContext) (drivers.Files, error) { return nil, nil }
func (fake) Decide(dev drivers.Device, _ drivers.Observations, _ drivers.DecideSettings, _ time.Time) drivers.Decision {
	return drivers.Decision{State: dev.LastState, StatusPatch: dev.Memory}
}
func (f fake) Firmware() []drivers.FirmwareLoad { return f.firmware }

func TestRegistry(t *testing.T) {
	cisco := fake{name: "cisco79x5", models: []string{"cisco-7965", "cisco-7945", "cisco-7975"}}
	soft := fake{name: "softphone", models: []string{"softphone"}}
	r, err := drivers.NewRegistry(cisco, soft)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Models(), []string{"cisco-7945", "cisco-7965", "cisco-7975", "softphone"}; !slices.Equal(got, want) {
		t.Fatalf("Models() = %v, want %v", got, want)
	}
	d, err := r.ForModel("cisco-7945")
	if err != nil || d.Name() != "cisco79x5" {
		t.Fatalf("ForModel(cisco-7945) = %v, %v", d, err)
	}
	if _, err := r.ForModel("cisco-7940"); !errors.Is(err, drivers.ErrUnknownModel) {
		t.Fatalf("ForModel(cisco-7940) error = %v, want ErrUnknownModel", err)
	}
	if ds := r.Drivers(); len(ds) != 2 || ds[0].Name() != "cisco79x5" || ds[1].Name() != "softphone" {
		t.Fatalf("Drivers() = %v, want cisco79x5 then softphone", ds)
	}
}

func TestRegistryRejectsBadDrivers(t *testing.T) {
	if _, err := drivers.NewRegistry(fake{name: "a", models: []string{"x"}}, fake{name: "b", models: []string{"x"}}); err == nil {
		t.Error("two drivers claiming one model: want an error")
	}
	if _, err := drivers.NewRegistry(fake{name: "empty"}); err == nil {
		t.Error("a driver with no models: want an error")
	}
	if _, err := drivers.NewRegistry(fake{name: "a", models: []string{"x", "y"}, noProfile: []string{"y"}}); err == nil {
		t.Error("a model without a profile: want an error")
	}
	for name, mutate := range map[string]func(*drivers.Profile){
		"a profile naming another model": func(p *drivers.Profile) { p.Model = "other" },
		"bad endpoint options":           func(p *drivers.Profile) { p.Endpoint.Transport = "tls" },
		"a short password":               func(p *drivers.Profile) { p.Provisioning.Password.Length = 6 },
	} {
		profile := func(model string) drivers.Profile {
			p := validProfile(model)
			mutate(&p)
			return p
		}
		if _, err := drivers.NewRegistry(fake{name: "a", models: []string{"x"}, profile: profile}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestPasswordPolicyValidate(t *testing.T) {
	for _, p := range []drivers.PasswordPolicy{
		{Length: 12, Alphabet: drivers.Alphanumeric},
		{Length: 24, Alphabet: drivers.Alphanumeric},
		{Length: 8, Alphabet: "0123456789"},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for _, p := range []drivers.PasswordPolicy{
		{Length: 7, Alphabet: drivers.Alphanumeric},
		{Length: 12, Alphabet: ""},
		{Length: 12, Alphabet: "aaaaaaaaaaaa"},             // too few distinct characters
		{Length: 12, Alphabet: drivers.Alphanumeric + ";"}, // would need escaping
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%+v: want an error", p)
		}
	}
	if len(drivers.Alphanumeric) != 62 {
		t.Errorf("Alphanumeric has %d characters, want 62", len(drivers.Alphanumeric))
	}
}

func TestProfileSupportsDefaultsToUnsupported(t *testing.T) {
	p := drivers.Profile{Features: map[drivers.Signalling]map[drivers.Feature]drivers.Support{
		drivers.SignallingPJSIP: {
			drivers.FeatureHold:     drivers.Supported,
			drivers.FeatureTransfer: drivers.Untested,
			drivers.FeatureBLF:      drivers.Unsupported,
		},
	}}
	for _, tc := range []struct {
		s    drivers.Signalling
		f    drivers.Feature
		want drivers.Support
	}{
		{drivers.SignallingPJSIP, drivers.FeatureHold, drivers.Supported},
		{drivers.SignallingPJSIP, drivers.FeatureTransfer, drivers.Untested},
		{drivers.SignallingPJSIP, drivers.FeatureBLF, drivers.Unsupported},
		{drivers.SignallingPJSIP, drivers.FeaturePark, drivers.Unsupported},        // not listed
		{drivers.Signalling("chan_sip"), drivers.FeatureHold, drivers.Unsupported}, // unknown backend
	} {
		if got := p.Supports(tc.s, tc.f); got != tc.want {
			t.Errorf("Supports(%s, %s) = %s, want %s", tc.s, tc.f, got, tc.want)
		}
	}
}

// digest returns a well-formed fake SHA-256 (test data, not a real file's).
func digest(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// twoLoads is the shape of the real 79x5 manifest: two loads that both carry
// term45.default.loads, with different contents.
func twoLoads() []drivers.FirmwareLoad {
	return []drivers.FirmwareLoad{
		{Name: "SIP45.9-4-2SR4-3S", Models: []string{"cisco-7945"}, Files: []drivers.FirmwareFile{
			{Name: "SIP45.9-4-2SR4-3S.loads", SHA256: digest('a'), Size: 673},
			{Name: "apps45.9-4-2SR4-3.sbn", SHA256: digest('b'), Size: 4637904},
			{Name: "term45.default.loads", SHA256: digest('c'), Size: 673, Shared: true},
		}},
		{Name: "SIP45.9-4-2SR3-1S", Models: []string{"cisco-7945"}, Files: []drivers.FirmwareFile{
			{Name: "SIP45.9-4-2SR3-1S.loads", SHA256: digest('d'), Size: 673},
			{Name: "term45.default.loads", SHA256: digest('e'), Size: 673, Shared: true},
		}},
	}
}

func TestValidateFirmware(t *testing.T) {
	if err := drivers.ValidateFirmware(nil); err != nil {
		t.Errorf("no firmware: %v", err)
	}
	if err := drivers.ValidateFirmware(twoLoads()); err != nil {
		t.Fatalf("two loads sharing term45.default.loads: %v", err)
	}
	if l, ok := drivers.LoadByName(twoLoads(), "SIP45.9-4-2SR3-1S"); !ok || len(l.Files) != 2 {
		t.Errorf("LoadByName(SR3-1) = %+v, %v", l, ok)
	}
	if _, ok := drivers.LoadByName(twoLoads(), "jar45sccp.9-4-2SR4-3"); ok {
		t.Error("LoadByName found a load that is not in the manifest")
	}

	for name, mutate := range map[string]func([]drivers.FirmwareLoad){
		"a shared name not marked Shared": func(l []drivers.FirmwareLoad) { l[1].Files[1].Shared = false },
		"one file name in two loads":      func(l []drivers.FirmwareLoad) { l[1].Files[0].Name = "apps45.9-4-2SR4-3.sbn" },
		"a duplicate load":                func(l []drivers.FirmwareLoad) { l[1].Name = l[0].Name },
		"a load without a name":           func(l []drivers.FirmwareLoad) { l[0].Name = "" },
		"a load without models":           func(l []drivers.FirmwareLoad) { l[0].Models = nil },
		"a load without files":            func(l []drivers.FirmwareLoad) { l[1].Files = nil },
		"a file listed twice in a load":   func(l []drivers.FirmwareLoad) { l[0].Files[1].Name = l[0].Files[0].Name },
		"a path, not a file name":         func(l []drivers.FirmwareLoad) { l[0].Files[0].Name = "../x.loads" },
		"an upper-case digest":            func(l []drivers.FirmwareLoad) { l[0].Files[0].SHA256 = digest('A') },
		"a short digest":                  func(l []drivers.FirmwareLoad) { l[0].Files[0].SHA256 = "abc" },
		"a zero size":                     func(l []drivers.FirmwareLoad) { l[0].Files[0].Size = 0 },
	} {
		loads := twoLoads()
		mutate(loads)
		if err := drivers.ValidateFirmware(loads); err == nil {
			t.Errorf("%s: ValidateFirmware = nil, want an error", name)
		}
	}
}

func TestRegistryRejectsABadManifest(t *testing.T) {
	bad := twoLoads()
	bad[1].Files[1].Shared = false
	if _, err := drivers.NewRegistry(fake{name: "cisco79x5", models: []string{"cisco-7945"}, firmware: bad}); err == nil {
		t.Error("a driver whose manifest breaks ValidateFirmware: want an error")
	}
	if _, err := drivers.NewRegistry(fake{name: "cisco79x5", models: []string{"cisco-7945"}, firmware: twoLoads()}); err != nil {
		t.Errorf("a driver with a valid manifest: %v", err)
	}
}

// TestPendingFirmware pins how a manifest says "no genuine digest yet": a
// pending file is valid in a manifest, but never staged, so its load is
// never available.
func TestPendingFirmware(t *testing.T) {
	loads := twoLoads()
	loads[0].Files[1].SHA256 = drivers.SHA256Pending
	if err := drivers.ValidateFirmware(loads); err != nil {
		t.Fatalf("a manifest with a pending digest: %v", err)
	}
	if got := loads[0].PendingFiles(); !slices.Equal(got, []string{"apps45.9-4-2SR4-3.sbn"}) {
		t.Errorf("PendingFiles = %v", got)
	}
	if got := loads[1].PendingFiles(); got != nil {
		t.Errorf("PendingFiles of a pinned load = %v, want none", got)
	}
	if !loads[0].Files[1].Pending() || loads[0].Files[0].Pending() {
		t.Error("Pending() disagrees with the manifest")
	}
	// Even a store that claims the pending file is present cannot make the
	// load available.
	store := agentapi.FirmwareStore{Files: []agentapi.FirmwareFileStatus{
		{Load: "SIP45.9-4-2SR4-3S", Name: "SIP45.9-4-2SR4-3S.loads", Present: true},
		{Load: "SIP45.9-4-2SR4-3S", Name: "apps45.9-4-2SR4-3.sbn", Present: true},
		{Load: "SIP45.9-4-2SR4-3S", Name: "term45.default.loads", Present: true},
	}}
	if got := loads[0].Missing(store); !slices.Equal(got, []string{"apps45.9-4-2SR4-3.sbn"}) {
		t.Errorf("Missing with a pending file = %v", got)
	}
	for _, digest := range []string{"", "PENDING", "pending "} {
		bad := twoLoads()
		bad[0].Files[0].SHA256 = digest
		if err := drivers.ValidateFirmware(bad); err == nil {
			t.Errorf("digest %q: want an error", digest)
		}
	}
}

func TestFirmwareMissing(t *testing.T) {
	l := twoLoads()[0]
	if got := l.Missing(agentapi.FirmwareStore{}); !slices.Equal(got,
		[]string{"SIP45.9-4-2SR4-3S.loads", "apps45.9-4-2SR4-3.sbn", "term45.default.loads"}) {
		t.Errorf("Missing from an empty store = %v", got)
	}
	store := agentapi.FirmwareStore{Files: []agentapi.FirmwareFileStatus{
		{Load: "SIP45.9-4-2SR4-3S", Name: "SIP45.9-4-2SR4-3S.loads", Present: true},
		{Load: "SIP45.9-4-2SR4-3S", Name: "apps45.9-4-2SR4-3.sbn", Present: false}, // wrong digest
		// term45.default.loads is staged, but for the other load.
		{Load: "SIP45.9-4-2SR3-1S", Name: "term45.default.loads", Present: true},
	}}
	if got := l.Missing(store); !slices.Equal(got, []string{"apps45.9-4-2SR4-3.sbn", "term45.default.loads"}) {
		t.Errorf("Missing = %v", got)
	}
	store.Files[1].Present = true
	store.Files = append(store.Files, agentapi.FirmwareFileStatus{Load: "SIP45.9-4-2SR4-3S", Name: "term45.default.loads", Present: true})
	if got := l.Missing(store); len(got) != 0 {
		t.Errorf("Missing with every file staged = %v, want none", got)
	}
}
