// Package provision is the seam between the agent's apply (which knows the
// live bundle and its resolved secrets) and device HTTP (which serves each
// softphone's provisioning document to the holder of its one-time token,
// design §8.4).
//
// The rules device HTTP keeps with a Grant (docs/plans/release-a.md, W2
// task level, W2.4):
//   - the token is the whole request path after agentapi.ProvisioningPathPrefix;
//     it is compared with every grant's token in constant time (as SHA-256
//     digests, so not even its length leaks), and a request that matches
//     none, or matches a spent one, gets the same 404 as any unknown path;
//   - a token is spent by its first successful match: the spend is written
//     durably (layout.StateDir/layout.SpentTokensFile, token digests only)
//     before the document is sent, so a crash cannot make it reusable, and
//     it stays spent until the live bundle no longer grants that token (the
//     controller rotates it on seeing the fetch, design §8.4 Provisioned);
//   - the document is read from Grant.BundleVersion's own directory, never
//     through the current link, so token and document always belong to one
//     version;
//   - no token appears in a log line, a fetch event, a metric or an error.
package provision

import "fmt"

// Grant is the right to fetch one softphone's provisioning document once:
// one per rendered softphone in the live bundle.
type Grant struct {
	// Device is the softphone's device ID.
	Device string
	// Token is its current one-time token, resolved from the projected
	// Secret (compile.BundleDevice.ProvisioningToken). Secret: never log
	// it. String and GoString redact it, and it has no JSON form.
	Token string `json:"-"`
	// File is the document's bundle path (compile.BundleDevice.ConfigFile,
	// provisioning/<device ID>.xml).
	File string
	// BundleVersion is the live bundle the grant belongs to: the document
	// is read from that version's directory.
	BundleVersion string
	// Version is the device's config version (compile.BundleDevice.Version),
	// reported with the fetch (sensor.FetchEvent.Version).
	Version string
}

// String describes the grant without its token.
func (g Grant) String() string {
	return fmt.Sprintf("grant{device %s, file %s, bundle %s, version %s, token redacted}",
		g.Device, g.File, g.BundleVersion, g.Version)
}

// GoString is String, so %#v redacts the token too.
func (g Grant) GoString() string { return g.String() }

// MarshalLog is what a logr logger prints for a grant: everything but the
// token.
func (g Grant) MarshalLog() any {
	return struct{ Device, File, BundleVersion, Version string }{g.Device, g.File, g.BundleVersion, g.Version}
}

// Source is implemented by apply.
type Source interface {
	// Grants returns the live bundle's version and its grants (a copy the
	// caller may keep), or ("", nil) before the first apply. Every grant's
	// BundleVersion is the returned version. It must not block on I/O:
	// device HTTP calls it on every provisioning request.
	Grants() (bundleVersion string, grants []Grant)
}
