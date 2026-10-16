// Package sensor holds the seams between the agent's sensors (design §8.2)
// and its observation store, which turns what they report into
// GET /v1/observations:
//
//   - sensor 1, the fetch log: the file servers (TFTP :69 and HTTP :6970 in
//     the separate file server process, device HTTP :8088 in the agent)
//     report every transfer as a FetchEvent to a FetchRecorder; the file
//     server does so over a unix socket, one JSON line per event (wire.go);
//   - sensor 2, AMI, is internal to the agent's core (package ami);
//   - sensor 3, the phones' web pages: the scraper asks ScrapeTargets which
//     phones to read and hands each result to a ScrapeRecorder;
//   - sensor 4, the firmware store, is a FirmwareSensor.
//
// The package holds types, their validation and the wire format only, so
// the servers, the scraper, the store and the observation store depend on
// it and never on each other.
package sensor

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// FetchEvent is one transfer as the server that handled it saw it: one
// request for one file, whatever the outcome. The observation store
// attributes it to a device and turns it into an api.Fetch.
//
// Rules for the server that reports it:
//   - one event per request, reported when the transfer finished or failed
//     (a request refused before any transfer, such as an unknown file, is
//     a notFound event);
//   - File is what the device asked for, without one leading "/", except
//     for a softphone's provisioning document: that request carries the
//     one-time token, which never appears in an event, a log line or a
//     metric, so File is then the document's bundle path
//     (provisioning/<device ID>.xml), or ProvisioningUnknownFile when the
//     token matched no device;
//   - a server resolves the live version once per request
//     (layout.Current), serves only from that version's directory, and
//     reports it in BundleVersion, with the bundle path it served or looked
//     for in BundlePath. A firmware file, or a name the bundle cannot hold,
//     has neither;
//   - Device and Version are set only by a server that knows them for
//     certain: device HTTP, from the provisioning grant the token matched.
//     Every other event leaves them empty and the store derives them (by
//     MAC, by token, or by source address; and Version from the index of
//     BundleVersion, layout.IndexFile).
type FetchEvent struct {
	// Transport is the server that handled the request.
	Transport api.Transport `json:"transport"`
	// File is the requested name (see the rules above).
	File string `json:"file"`
	// Result is the outcome.
	Result api.FetchResult `json:"result"`
	// At is when the transfer finished or failed.
	At time.Time `json:"at"`
	// Source is the device's IP address, canonical (netip.Addr.String of
	// the unmapped address): no port, no brackets.
	Source string `json:"source"`
	// BundleVersion is the live version the request was resolved in, or
	// empty (firmware, no version live yet).
	BundleVersion string `json:"bundleVersion,omitempty"`
	// BundlePath is the bundle path served or looked for, e.g.
	// tftp/SEPD0C78915E8C8.cnf.xml, http/directory.xml or
	// provisioning/softphone-den.xml. Empty for firmware.
	BundlePath string `json:"bundlePath,omitempty"`
	// Device is the device ID, when the server knows it for certain.
	Device string `json:"device,omitempty"`
	// Version is the device's config version (compile.BundleDevice.Version)
	// of the file served, when the server knows it for certain. It is what
	// a softphone's lastConfigFetch.version becomes, so device HTTP sets it
	// on every served provisioning document.
	Version string `json:"version,omitempty"`
	// Error describes a FetchError. Never a path outside the tree, never a
	// token.
	Error string `json:"error,omitempty"`
}

// ProvisioningUnknownFile is FetchEvent.File for a provisioning request
// whose token matched no grant, spent or not: the URL prefix without its
// leading "/", and without the token.
const ProvisioningUnknownFile = "p/"

// provisioningElement is the first element of the provisioning URL path
// (api.ProvisioningPathPrefix, "/p/").
var provisioningElement = strings.Trim(api.ProvisioningPathPrefix, "/")

// ProvisioningName reports whether a requested name or path could carry a
// one-time provisioning token: an element "p" (any case) followed by
// another, splitting at "/" and "\" and skipping empty elements (p/<token>,
// /p/<token>, //p/<token>, x/../p/<token>, p\<token>). The file server
// holds no token to compare with, so it never looks such a name up and
// reports it as ProvisioningUnknownFile; the agent drops any other event
// from it that has one. No manifest name (one element) and no bundle path
// the compiler renders has such an element.
func ProvisioningName(name string) bool {
	els := strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' })
	for i := 0; i+1 < len(els); i++ {
		if strings.EqualFold(els[i], provisioningElement) {
			return true
		}
	}
	return false
}

// Limits on a FetchEvent's fields.
const (
	// MaxFileLength is the longest File: TFTP names and URL paths a device
	// can send are short; anything longer is refused before it is
	// reported.
	MaxFileLength = 255
	// MaxErrorLength is the longest Error.
	MaxErrorLength = 512
)

var (
	versionPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	deviceIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// Validate checks an event before the store accepts it. Every reporter
// must produce valid events; the agent also validates what arrives on the
// file server's socket, which is the one place an event crosses a process
// boundary.
func (e FetchEvent) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("fetch event: "+format, args...)) }
	switch e.Transport {
	case api.TransportTFTP, api.TransportHTTP6970, api.TransportHTTPDevice:
	default:
		bad("unknown transport %q", e.Transport)
	}
	switch e.Result {
	case api.FetchServed, api.FetchNotFound:
		if e.Error != "" {
			bad("an error on a %s fetch", e.Result)
		}
	case api.FetchError:
	default:
		bad("unknown result %q", e.Result)
	}
	if err := checkText("file", e.File, MaxFileLength); err != nil {
		errs = append(errs, err)
	} else if e.File == "" || e.File[0] == '/' {
		bad("file %q is empty or starts with /", e.File)
	}
	if err := checkText("error", e.Error, MaxErrorLength); err != nil {
		errs = append(errs, err)
	}
	if e.At.IsZero() {
		bad("no time")
	}
	if a, err := netip.ParseAddr(e.Source); err != nil || a.Unmap().String() != e.Source || a.Zone() != "" {
		bad("source %q is not a canonical IP address", e.Source)
	}
	if e.BundleVersion != "" && !layout.ValidVersion(e.BundleVersion) {
		bad("bundle version %q is not a version", e.BundleVersion)
	}
	if e.BundlePath != "" {
		if e.BundleVersion == "" {
			bad("bundle path %q without a bundle version", e.BundlePath)
		}
		if _, err := compile.TreeOf(e.BundlePath); err != nil {
			errs = append(errs, fmt.Errorf("fetch event: %w", err))
		}
	}
	if e.Device != "" && (len(e.Device) > drivers.DeviceIDMaxLength || !deviceIDPattern.MatchString(e.Device)) {
		bad("device %q is not a device ID", e.Device)
	}
	if e.Version != "" {
		if !versionPattern.MatchString(e.Version) {
			bad("version %q is not a version", e.Version)
		}
		if e.Device == "" {
			bad("a version without a device")
		}
	}
	return errors.Join(errs...)
}

// checkText refuses invalid UTF-8, control characters and text over limit
// bytes: event text ends up in logs and in the observations document.
func checkText(what, s string, limit int) error {
	if len(s) > limit {
		return fmt.Errorf("fetch event: %s is longer than %d bytes", what, limit)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("fetch event: %s is not UTF-8", what)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("fetch event: %s holds a control character", what)
		}
	}
	return nil
}

// FetchRecorder receives fetch events. The observation store implements
// it. RecordFetch must be safe for concurrent use and must not block on
// I/O: servers call it on their request path. It drops an invalid event
// (and logs it) rather than fail the transfer that produced it.
type FetchRecorder interface {
	RecordFetch(FetchEvent)
}

// FetchRecorderFunc adapts a function to a FetchRecorder.
type FetchRecorderFunc func(FetchEvent)

// RecordFetch calls f.
func (f FetchRecorderFunc) RecordFetch(e FetchEvent) { f(e) }

// ScrapeTarget is one phone the scraper should read now: a Phone in the
// applied bundle whose address the agent knows (its live contact's, else
// the source of its latest attributed fetch).
type ScrapeTarget struct {
	// Device is the phone's device ID.
	Device string
	// Address is its IP address, canonical.
	Address string
	// Live is true while it has a live SIP contact. The scraper reads a
	// phone without one every 60 s and a phone with one every 5 min
	// (docs/plans/release-a.md §5.1: the agent approximates "Ready").
	Live bool
}

// ScrapeTargets is implemented by the observation store. The scraper calls
// it on every tick; it must not block on I/O.
type ScrapeTargets interface {
	ScrapeTargets(now time.Time) []ScrapeTarget
}

// ScrapeRecorder receives scrape results; the observation store implements
// it and stores w as the device's api.DeviceObservations.Web, as given.
//
// The scraper builds w by these rules, which the cisco79x5 driver relies on
// (its fold reads only a "fresh scrape"):
//   - a scrape reads /DeviceInformationX, /NetworkConfigurationX and
//     /DeviceLogX; it succeeds only if /DeviceInformationX (the load) was
//     read and parsed; the other two are best effort;
//   - success: At is when the last page was read or given up on, Error is
//     empty, and every field comes from this scrape: a best-effort page
//     that could not be read is nil (never an older scrape's), and a
//     /NetworkConfigurationX that broke off holds the fields before the
//     break;
//   - failure: At is when it failed, Error says why, and nothing from this
//     scrape is adopted: the fields are the previous successful scrape's
//     (nil if there was none).
type ScrapeRecorder interface {
	RecordScrape(device string, w api.WebScrape)
}

// FirmwareSensor is sensor 4: the firmware store's contents. The firmware
// store implements it; Status must be cheap enough to call on every
// observations request (the store caches digests by size and modification
// time) and lists every manifest file, Present only when verified.
type FirmwareSensor interface {
	Status() api.FirmwareStore
}
