package softphone

import (
	"maps"
	"slices"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Name is the driver's name.
const Name = "softphone"

// Driver is the softphone driver. It has no state: everything it knows is
// Go data in this package.
type Driver struct{}

var _ drivers.Driver = (*Driver)(nil)

// New returns the softphone driver.
func New() *Driver { return &Driver{} }

// Name returns "softphone".
func (*Driver) Name() string { return Name }

// Models returns the driver's only model, drivers.ModelSoftphone.
func (*Driver) Models() []string { return []string{drivers.ModelSoftphone} }

// Profile returns the softphone profile for drivers.ModelSoftphone. The
// profile's slices and maps are copies, so a caller cannot change the
// driver's data.
func (*Driver) Profile(model string) (drivers.Profile, bool) {
	if model != drivers.ModelSoftphone {
		return drivers.Profile{}, false
	}
	return cloneProfile(profile), true
}

// Firmware returns nil: a softphone has no firmware.
func (*Driver) Firmware() []drivers.FirmwareLoad { return nil }

// ProvisioningFile is the bundle path of a softphone's provisioning
// document: "provisioning/" + device ID + ".xml". The agent serves it at the
// softphone's one-time URL and logs the fetch under this path (never the
// token), so a served fetch of it is the softphone's config fetch.
func ProvisioningFile(id string) string { return "provisioning/" + id + ".xml" }

func cloneProfile(p drivers.Profile) drivers.Profile {
	p.Hardware.Codecs = slices.Clone(p.Hardware.Codecs)
	p.Endpoint.Codecs = slices.Clone(p.Endpoint.Codecs)
	features := make(map[drivers.Signalling]map[drivers.Feature]drivers.Support, len(p.Features))
	for s, fs := range p.Features {
		features[s] = maps.Clone(fs)
	}
	p.Features = features
	return p
}
