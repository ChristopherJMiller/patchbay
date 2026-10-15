package drivers_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// fake is the smallest Driver: it proves the interface is implementable and
// drives the registry tests.
type fake struct {
	name   string
	models []string
}

var _ drivers.Driver = fake{}

func (f fake) Name() string     { return f.name }
func (f fake) Models() []string { return f.models }
func (f fake) Profile(model string) (drivers.Profile, bool) {
	return drivers.Profile{Model: model}, slices.Contains(f.models, model)
}
func (fake) Render(drivers.Device, drivers.RenderContext) (drivers.Files, error) { return nil, nil }
func (fake) Decide(dev drivers.Device, _ drivers.Observations, _ time.Time) drivers.Decision {
	return drivers.Decision{State: drivers.StateUnknown, StatusPatch: dev.Memory}
}
func (fake) Firmware() []drivers.FirmwareFile { return nil }

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
}

func TestRegistryRejectsBadDrivers(t *testing.T) {
	if _, err := drivers.NewRegistry(fake{name: "a", models: []string{"x"}}, fake{name: "b", models: []string{"x"}}); err == nil {
		t.Error("two drivers claiming one model: want an error")
	}
	if _, err := drivers.NewRegistry(fake{name: "empty"}); err == nil {
		t.Error("a driver with no models: want an error")
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
