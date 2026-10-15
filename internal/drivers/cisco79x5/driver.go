package cisco79x5

import (
	"slices"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Driver is the cisco79x5 driver. Its zero value has an empty manifest; use
// New.
type Driver struct {
	// loads is the firmware manifest: Cisco's loads, plus the e2e test load
	// in binaries built with the e2e tag.
	loads []drivers.FirmwareLoad

	// tweak, when set, changes every profile the driver uses. Only tests
	// set it (export_test.go), to try a bench answer (ResyncByNotify off,
	// qualify on) before the bench gives it.
	tweak func(*drivers.Profile)
}

var _ drivers.Driver = (*Driver)(nil)

// New returns the driver with its firmware manifest.
func New() *Driver {
	return &Driver{loads: cloneLoads(append(slices.Clone(ciscoLoads), extraLoads...))}
}

// Name returns "cisco79x5".
func (*Driver) Name() string { return Name }

// Models returns cisco-7945, cisco-7965 and cisco-7975, the Phone CRD's
// model enum.
func (*Driver) Models() []string { return slices.Clone(models) }

// Profile returns a model's capability profile, a fresh copy on every call.
func (d *Driver) Profile(model string) (drivers.Profile, bool) { return d.profile(model) }

// Firmware returns the firmware manifest, a fresh copy on every call.
func (d *Driver) Firmware() []drivers.FirmwareLoad { return cloneLoads(d.loads) }

// profile is the model's profile as this driver uses it.
func (d *Driver) profile(model string) (drivers.Profile, bool) {
	p, ok := profile(model)
	if ok && d.tweak != nil {
		d.tweak(&p)
	}
	return p, ok
}

// load returns the manifest load called name, when it lists model.
func (d *Driver) load(model, name string) (drivers.FirmwareLoad, bool) {
	l, ok := drivers.LoadByName(d.loads, name)
	if !ok || !slices.Contains(l.Models, model) {
		return drivers.FirmwareLoad{}, false
	}
	return l, true
}
