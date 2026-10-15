// Package drivers is the contract every device driver implements, and the
// types its pure functions take and return (design §4, §8).
//
// A driver owns one family of devices: cisco79x5 (CP-7945G/7965G/7975G, one
// firmware family) and softphone (Linphone remote provisioning) are the first
// two. Phone models are drivers in Go code, not data: another vendor means
// another driver and no resource change.
//
// Render and Decide are pure: no I/O, no clock (now is an argument), no
// randomness. They are tested with golden files and table tests. Everything
// impure (reading resources, polling the agent, writing status, sending
// actions) is the controller's job; the controller is the only component
// that decides and the only writer of status.
package drivers

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// Driver renders config for, and decides the lifecycle of, one family of
// devices.
type Driver interface {
	// Name is the driver's name, e.g. "cisco79x5".
	Name() string

	// Models lists the model names this driver serves, e.g. "cisco-7945".
	// A model belongs to exactly one driver.
	Models() []string

	// Profile is the capability profile of one of the driver's models. ok
	// is false for a model the driver does not serve.
	Profile(model string) (p Profile, ok bool)

	// Render produces the device's provisioning files (for the 79x5,
	// tftp/SEP<MAC>.cnf.xml; for a softphone, provisioning/<id>.xml). Pure.
	// Secret values never appear in the output, only the placeholders ctx
	// carries.
	Render(dev Device, ctx RenderContext) (Files, error)

	// Decide is one step of the device's state machine: from the device
	// (intent plus memory), what the sensors see now and the install's
	// settings, it returns the state, conditions, actions and next memory.
	// Pure; first matching row of the driver's ordered rule table wins
	// (design §8.1: Decide(spec, observations, status, settings, now)).
	//
	// Rules every driver keeps, whatever its table:
	//   - a spec that breaks a driver rule is Invalid: an unknown model, a
	//     feature the backend cannot deliver, NumberConflict or MACConflict,
	//     and a target load (dev.Firmware) that is not a load in Firmware();
	//   - a human request (dev.Request differs from
	//     dev.Memory.HandledRequest) is answered at most once: the Decision
	//     that acts on it sets StatusPatch.HandledRequest to dev.Request.
	Decide(dev Device, obs Observations, s DecideSettings, now time.Time) Decision

	// Firmware is the driver's firmware manifest: the loads it can stage,
	// each with its files and their pinned SHA-256s. Nil for drivers without
	// firmware. ValidateFirmware states the rules a manifest keeps.
	Firmware() []FirmwareLoad
}

// Files maps a bundle path (see compile.TreeOf: the first element is the
// tree) to its contents. A driver writes only into the tree its devices fetch
// from: tftp/ for phones, provisioning/ for softphones. The compiler refuses
// anything else, so a driver can never put a file next to Asterisk's config
// or into the tree served to anyone.
type Files map[string][]byte

// DeviceKind is the resource kind behind a Device.
type DeviceKind string

// Device kinds.
const (
	// KindPhone is a v1alpha1.Phone.
	KindPhone DeviceKind = "Phone"
	// KindSoftphone is a v1alpha1.Softphone.
	KindSoftphone DeviceKind = "Softphone"
)

// Device is everything a driver is told about one device: the resource's
// intent, its memory (status as last written), and what the controller
// computed for it on this pass. The controller builds it; drivers never read
// resources.
type Device struct {
	// Kind and Name identify the resource.
	Kind DeviceKind
	Name string

	// ID is the device's ID in the compiled bundle (its PJSIP endpoint);
	// agent observations and actions use it.
	ID string

	// Model is spec.model for phones, "softphone" for softphones.
	Model string

	// MAC is the hardware address, upper case. Empty for softphones.
	MAC string

	// Number is the device's own extension.
	Number string

	// Label is spec.label, or the resource name when that is empty.
	Label string

	// Firmware is the target load: spec.firmware, or empty for the driver
	// default. A name that is not a load in the driver's manifest makes the
	// device Invalid.
	Firmware string

	// ConfigVersion is this device's compiled config version (see
	// compile.Bundle.DeviceVersions). Compared with
	// Memory.LastConfigFetch.Version to tell ConfigStale.
	ConfigVersion string

	// NumberConflict is true when an older resource holds this number.
	NumberConflict bool

	// MACConflict is true when an older Phone has the same MAC. Always false
	// for softphones.
	MACConflict bool

	// Request is the value of the device kind's request annotation
	// (v1alpha1.AnnotationResyncRequested on a Phone,
	// AnnotationReissueRequested on a Softphone), or empty when it is absent
	// or too long. It is a new request when it differs from
	// Memory.HandledRequest.
	Request string

	// LastState is status.state as last written, read-only: Decide may
	// return it as Decision.State when it cannot tell (Unknown keeps the last
	// known state for display), but only Decision.State writes status.state.
	LastState State

	// Memory is the state machine's memory, from status.
	Memory Memory
}

// Memory is the part of status that a driver reads and writes: the state
// machine's only memory (design §8.1). Fields mirror the v1alpha1 status
// fields of the same names; fields a driver does not use stay zero.
type Memory struct {
	FirstSeen        *metav1.Time
	Firmware         string
	Address          string
	LastConfigFetch  *v1alpha1.ConfigFetch
	LastRegistered   *metav1.Time
	UpgradeStartedAt *metav1.Time
	LastAction       *v1alpha1.LastAction

	// HandledRequest is the request annotation value last acted on
	// (status.handledRequest).
	HandledRequest string
}

// Observations is what the four sensors (design §8.2) see of one device,
// assembled by the controller from the agent's document.
type Observations struct {
	// AgentReachable is false when the controller's latest poll of the
	// agent failed. The rest is then from the last successful poll.
	AgentReachable bool

	// PolledAt is when the controller last fetched the agent's document
	// successfully, by the controller's clock. Decide judges staleness from
	// it and now.
	PolledAt time.Time

	// Agent is the agent's health: applied version, last apply error,
	// whether Asterisk is up.
	Agent agentapi.AgentStatus

	// Device is the fetch log (sensor 1), AMI registration (sensor 2) and
	// web scrape (sensor 3) for this device. Nil when no sensor has seen it.
	Device *agentapi.DeviceObservations

	// Firmware is the firmware store (sensor 4): which files of which loads
	// are staged with a matching SHA-256.
	Firmware agentapi.FirmwareStore
}

// Decision is the result of one Decide step (design §8.1).
type Decision struct {
	// State is the display summary, and the only thing that writes
	// status.state (Memory has no state; dev.LastState is read-only).
	State State

	// Conditions to set on the resource. The controller fills in
	// ObservedGeneration and LastTransitionTime (via meta.SetStatusCondition)
	// so Decide stays pure; it sets Type, Status, Reason and Message.
	Conditions []metav1.Condition

	// Actions to take, in order. Empty when there is nothing to do.
	Actions []Action

	// StatusPatch is the device's next memory. It is a whole value, not a
	// diff: Decide starts from dev.Memory and changes what it must, and the
	// controller writes every field to status. That keeps each table-test
	// row's expected output complete.
	StatusPatch Memory

	// HumanAction is the exact instruction for a person, or empty when no
	// one needs to act. Written to status.humanAction.
	HumanAction string

	// RequeueAfter is when to decide again even if nothing changes (for
	// grace periods and slow re-checks). Zero means only on change.
	RequeueAfter time.Duration
}

// Action is one thing the controller should do to a device.
type Action struct {
	// Kind is what to do.
	Kind v1alpha1.ActionKind
	// Version is the config version the action should bring the device to,
	// when it is about one. Recorded in status.lastAction.
	Version string
}

// State is a device's display state (status.state). The constants below are
// the states design §8.3 and §8.4 name. A driver's rule table maps each row
// to one of them; the exhaustive linter keeps every switch over a driver's
// states complete.
type State string

// States of the cisco79x5 machine (design §8.3), in rule order.
const (
	StateInvalid         State = "Invalid"
	StateUnknown         State = "Unknown"
	StateUnseen          State = "Unseen"
	StateNeedsFirmware   State = "NeedsFirmware"
	StateNeedsTrustReset State = "NeedsTrustReset"
	StateUpgrading       State = "Upgrading"
	StateRegistering     State = "Registering"
	StateConfigStale     State = "ConfigStale"
	StateRegistered      State = "Registered"
	StateUnreachable     State = "Unreachable"
)

// States only the softphone machine uses (design §8.4). It also uses
// Invalid, Unknown and Registered.
const (
	StateIssued      State = "Issued"
	StateProvisioned State = "Provisioned"
	StateOffline     State = "Offline"
)

// DecideSettings is what Decide needs beyond the device and its
// observations: the install-wide settings that change which rule matches.
// Grace periods and rate limits (3 min, 5 min, 20 min, ...) are not settings;
// they are rows of each driver's table.
type DecideSettings struct {
	// Signalling is the install-wide signalling backend (design §8.5). A
	// spec asking for a feature it cannot deliver is Invalid / Unsupported.
	Signalling Signalling

	// StaleAfter is how old Observations.PolledAt may be before the
	// observations count as stale and the device is Unknown.
	StaleAfter time.Duration
}

// RenderContext is what Render needs beyond the device: install-wide
// settings, already resolved by the compiler.
type RenderContext struct {
	// PBXAddress is the address devices register to and fetch from, e.g.
	// 192.168.0.240.
	PBXAddress string

	// SIPPort is Asterisk's SIP port (5060).
	SIPPort int

	// DeviceHTTPURL is the base URL of the agent's device HTTP server, e.g.
	// http://192.168.0.240:8088 (directory, services, provisioning).
	DeviceHTTPURL string

	// NTPServer is the time server as an IP address (the 79x5 cannot
	// resolve names here).
	NTPServer string

	// Timezone is an IANA zone name, e.g. America/Los_Angeles. Drivers map
	// it to whatever their devices want.
	Timezone string

	// Signalling is the install-wide signalling backend (design §8.5).
	Signalling Signalling

	// DialPatterns are the number plan's complete-number patterns (e.g.
	// "1XX", "600"), from which a driver renders its dial template so
	// complete numbers dial at once.
	DialPatterns []string

	// SIPPassword is the placeholder for the device's SIP password
	// (@@secret:<name>/<key>@@), never the password itself.
	SIPPassword string

	// ProvisioningToken is the placeholder for a softphone's one-time
	// provisioning token. Empty for phones.
	ProvisioningToken string
}

// Signalling is the PBX's SIP signalling backend (design §8.5).
type Signalling string

// Signalling backends.
const (
	// SignallingPJSIP is stock Asterisk 22 LTS PJSIP, the only backend
	// implemented.
	SignallingPJSIP Signalling = "pjsip"
)

// Feature is a device feature whose availability depends on the driver and
// the signalling backend.
type Feature string

// Features named in design §8.5 and §10.
const (
	FeatureHold             Feature = "hold"
	FeatureTransfer         Feature = "transfer"
	FeaturePickup           Feature = "pickup"
	FeatureMeetMe           Feature = "meetMe"
	FeatureDistinctiveRing  Feature = "distinctiveRing"
	FeatureWideband         Feature = "wideband" // G.722
	FeatureRFC4733DTMF      Feature = "rfc4733DTMF"
	FeatureCallerIDUpdate   Feature = "callerIDUpdate" // Remote-Party-ID
	FeatureSpeedDial        Feature = "speedDial"
	FeatureServiceURL       Feature = "serviceURL"
	FeatureBLF              Feature = "blf"
	FeaturePark             Feature = "park"
	FeatureConference       Feature = "conference"
	FeatureDNDSync          Feature = "dndSync"
	FeatureCallForwardSync  Feature = "callForwardSync"
	FeatureIntercom         Feature = "intercom" // auto-answer
	FeatureSecondLine       Feature = "secondLine"
	FeatureRTPRx            Feature = "rtpRx" // audio push without a call
	FeatureWallpaper        Feature = "wallpaper"
	FeatureCustomRingtones  Feature = "customRingtones"
	FeatureXMLServices      Feature = "xmlServices"
	FeaturePushNotification Feature = "pushNotification" // CGI/Execute
)

// Support is whether a feature works on a backend.
type Support string

// Support levels.
const (
	// Supported features are rendered.
	Supported Support = "supported"
	// Unsupported features are refused with Unsupported=True, never
	// rendered as a dead key.
	Unsupported Support = "unsupported"
	// Untested features await a bench test (design §8.6). They are rendered
	// and documented as unverified.
	Untested Support = "untested"
)

// Profile is a model's capability profile: Go data in the driver, not a CRD
// (design §4). `patchbay models` prints it.
type Profile struct {
	// Model is the model name, e.g. "cisco-7945".
	Model string

	// Hardware is what the device physically has.
	Hardware Hardware

	// Provisioning is how the device is configured.
	Provisioning Provisioning

	// Features says, per signalling backend, what works. A feature absent
	// from a backend's map is Unsupported.
	Features map[Signalling]map[Feature]Support
}

// Supports reports the support level of a feature on a backend; anything not
// listed is Unsupported.
func (p Profile) Supports(s Signalling, f Feature) Support {
	if level, ok := p.Features[s][f]; ok {
		return level
	}
	return Unsupported
}

// Hardware is the physical side of a capability profile.
type Hardware struct {
	// LineKeys is the number of line/feature buttons (2 on a 7945).
	LineKeys int
	// Softkeys is the number of softkeys under the display.
	Softkeys int
	// Display is the screen size in pixels.
	Display Size
	// ServiceArea is the XML service area in pixels.
	ServiceArea Size
	// Wallpaper and WallpaperThumbnail are the background image sizes.
	Wallpaper          Size
	WallpaperThumbnail Size
	// RingtoneFormat describes the ringtone encoding, e.g. "raw u-law 8 kHz
	// mono, at most 2 s".
	RingtoneFormat string
	// Codecs are the codecs the device offers Asterisk, in preference order.
	Codecs []string
}

// Size is a width and height in pixels.
type Size struct {
	Width, Height int
}

// Provisioning is the configuration side of a capability profile.
type Provisioning struct {
	// ConfigFile is the device's config file name pattern, with {MAC} for
	// the upper-case MAC, e.g. "SEP{MAC}.cnf.xml".
	ConfigFile string
	// DefaultFirmware is the load used when spec.firmware is empty, e.g.
	// SIP45.9-4-2SR4-3S. Empty for drivers without firmware.
	DefaultFirmware string
	// ScreenLabelMax is the longest short label the device shows (the 79x5
	// phoneLabel: 12, no spaces).
	ScreenLabelMax int
	// LineLabelMax is the longest line label / caller ID name (30).
	LineLabelMax int
	// Password is the policy for the generated SIP password.
	Password PasswordPolicy
}

// PasswordPolicy is how a driver's generated SIP passwords look.
type PasswordPolicy struct {
	// Length in characters (12 for the 79x5, 24 for softphones).
	Length int
	// Alphabet the characters are drawn from.
	Alphabet string
}

// FirmwareLoad is one load in a driver's firmware manifest: everything a
// device needs on TFTP to boot that load.
//
// The firmware store keeps files per load, because loads can share a file
// name with different contents: every Cisco SIP zip carries its own
// term45.default.loads and term65.default.loads, the files a factory-reset
// phone asks for to learn which load to boot. Such files are Shared. TFTP
// serves a Shared file from the driver's default load only (its profile's
// DefaultFirmware), so a reset phone always lands on the default load;
// every other file name belongs to exactly one load and is served by name.
type FirmwareLoad struct {
	// Name is the load ID, as the device reports it and as the SEP file's
	// loadInformation names it, e.g. SIP45.9-4-2SR4-3S. It is what
	// Phone.spec.firmware selects.
	Name string
	// Models are the models that can run this load.
	Models []string
	// Files are the files of the load. FirmwareAvailable means every one of
	// them is staged with its SHA-256.
	Files []FirmwareFile
}

// FirmwareFile is one file of a firmware load.
type FirmwareFile struct {
	// Name is the file name devices request, e.g. apps45.9-4-2SR4-3.sbn.
	Name string
	// SHA256 is the lower-case hex digest the file must have.
	SHA256 string
	// Size in bytes, for progress and sanity checks.
	Size int64
	// Shared marks a file whose name several loads carry with different
	// contents (Cisco's term*.default.loads). See FirmwareLoad.
	Shared bool
}

// LoadByName returns the load called name from a manifest.
func LoadByName(loads []FirmwareLoad, name string) (FirmwareLoad, bool) {
	for _, l := range loads {
		if l.Name == name {
			return l, true
		}
	}
	return FirmwareLoad{}, false
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateFirmware checks the rules a firmware manifest keeps:
//   - every load has a unique, non-empty name, at least one model and at
//     least one file;
//   - every file has a plain file name (no slash), a lower-case hex SHA-256
//     and a positive size, and appears once per load;
//   - a file name appears in more than one load only if it is Shared in
//     every load that carries it, because TFTP serves files by name.
func ValidateFirmware(loads []FirmwareLoad) error {
	var errs []error
	loadNames := map[string]bool{}
	owners := map[string][]string{} // file name -> the loads carrying it
	unshared := map[string]bool{}   // file name -> some load carries it not Shared
	for _, l := range loads {
		switch {
		case l.Name == "":
			errs = append(errs, errors.New("firmware: a load has no name"))
		case loadNames[l.Name]:
			errs = append(errs, fmt.Errorf("firmware: load %s is listed twice", l.Name))
		}
		loadNames[l.Name] = true
		if len(l.Models) == 0 {
			errs = append(errs, fmt.Errorf("firmware: load %s names no models", l.Name))
		}
		if len(l.Files) == 0 {
			errs = append(errs, fmt.Errorf("firmware: load %s has no files", l.Name))
		}
		inLoad := map[string]bool{}
		for _, f := range l.Files {
			switch {
			case f.Name == "" || strings.ContainsAny(f.Name, `/\`) || f.Name == "." || f.Name == "..":
				errs = append(errs, fmt.Errorf("firmware: load %s: %q is not a plain file name", l.Name, f.Name))
			case inLoad[f.Name]:
				errs = append(errs, fmt.Errorf("firmware: load %s lists %s twice", l.Name, f.Name))
			}
			inLoad[f.Name] = true
			if !sha256Hex.MatchString(f.SHA256) {
				errs = append(errs, fmt.Errorf("firmware: load %s: %s: SHA-256 must be 64 lower-case hex digits", l.Name, f.Name))
			}
			if f.Size <= 0 {
				errs = append(errs, fmt.Errorf("firmware: load %s: %s: size must be positive", l.Name, f.Name))
			}
			unshared[f.Name] = unshared[f.Name] || !f.Shared
			owners[f.Name] = append(owners[f.Name], l.Name)
		}
	}
	names := make([]string, 0, len(owners))
	for n := range owners {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if len(owners[n]) > 1 && unshared[n] {
			errs = append(errs, fmt.Errorf("firmware: %s is in loads %s but not Shared in all of them; TFTP serves files by name",
				n, strings.Join(owners[n], ", ")))
		}
	}
	return errors.Join(errs...)
}

// Registry maps model names to the drivers that serve them.
type Registry struct {
	byModel map[string]Driver
	drivers []Driver
}

// NewRegistry builds a registry. It fails if two drivers claim one model, a
// driver claims none, or a driver's firmware manifest breaks
// ValidateFirmware.
func NewRegistry(ds ...Driver) (*Registry, error) {
	r := &Registry{byModel: map[string]Driver{}}
	for _, d := range ds {
		models := d.Models()
		if len(models) == 0 {
			return nil, fmt.Errorf("drivers: %s serves no models", d.Name())
		}
		if err := ValidateFirmware(d.Firmware()); err != nil {
			return nil, fmt.Errorf("drivers: %s: %w", d.Name(), err)
		}
		for _, m := range models {
			if other, dup := r.byModel[m]; dup {
				return nil, fmt.Errorf("drivers: model %q claimed by both %s and %s", m, other.Name(), d.Name())
			}
			r.byModel[m] = d
		}
		r.drivers = append(r.drivers, d)
	}
	return r, nil
}

// ErrUnknownModel is returned for a model no driver serves.
var ErrUnknownModel = errors.New("drivers: no driver for model")

// ForModel returns the driver serving model.
func (r *Registry) ForModel(model string) (Driver, error) {
	if d, ok := r.byModel[model]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownModel, model)
}

// Models lists every registered model, sorted.
func (r *Registry) Models() []string {
	out := make([]string, 0, len(r.byModel))
	for m := range r.byModel {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}
