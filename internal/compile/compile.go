// Package compile turns resources and install settings into a Bundle: every
// file the PBX serves or loads (design §5, Compile).
//
// Compile is a pure function. It owns Asterisk's config directory outright
// (modules.conf uses autoload=no with an explicit module list; none of the
// image's stock configs are used), and it renders each device's files through
// its driver, the directory XML and each softphone's provisioning XML. Every
// file goes into one of four trees (asterisk/, tftp/, http/, provisioning/;
// see layout.go), and the tree decides who may read it once applied.
//
// manager.conf rules. AMI binds 127.0.0.1, but the PBX pod is on the host
// network, so that is the node's loopback: every host-network process on the
// node can connect. The generated AMI user is therefore the only barrier, and
// the compiler must render it with: a high-entropy generated secret (a
// placeholder, like every password); deny=0.0.0.0/0.0.0.0 then
// permit=127.0.0.1/255.255.255.255; and read/write classes limited to what the
// agent uses (module reloads, PJSIPNotify, registration queries), never all,
// command or originate. The classes do not contain a client that has the
// secret: the agent needs the system class (PJSIPNotify, PJSIPShowContacts,
// ModuleLoad), and system also allows ModuleLoad of any module in Asterisk's
// module directory (the image ships app_system, func_shell, res_agi) and
// DialplanExtensionAdd, so whoever holds the AMI secret can run commands as
// Asterisk's user in the PBX pod. The secret is the barrier: only the agent
// may hold it (docs/plans/release-a.md §5 keeps it out of every other
// container), and in the PBX pod Asterisk can see no module but the listed
// ones (AsteriskModuleDir).
//
// Secret values never appear in a bundle's files: a password appears only as
// a placeholder, @@secret:<name>/<key>@@ (see SecretRef), so the ConfigMap the
// controller writes is safe to read in full. The agent resolves placeholders
// when it applies a bundle and refuses the whole version if any placeholder
// cannot be resolved.
//
// Compile is tested with golden files.
package compile

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// Bundle is the compiled configuration of one install.
type Bundle struct {
	// Files maps a slash-separated path, relative to one applied version's
	// directory, to its contents (with placeholders, never secret values).
	// The first path element is the file's tree (TreeAsterisk, TreeTFTP,
	// TreeHTTP or TreeProvisioning); Validate checks the shape. ConfigMapData
	// encodes the bundle for the pbx-config ConfigMap.
	Files map[string][]byte

	// Version is the lower-case hex SHA-256 over Files, the device index and
	// AMI account, and the values of every secret they reference (provisioning
	// tokens included). A regenerated password therefore produces a new
	// version even though no file text changed.
	Version string

	// Devices is the device index, keyed by device ID (drivers.DeviceID):
	// one entry for every Phone and Softphone, rendered or not, so the agent
	// can attribute every fetch and registration to a device. Its Version is
	// what a device's status.observed.lastConfigFetch.version is compared
	// with (ConfigStale, design §8.3), so a change to one device, or to
	// Asterisk-only config, does not resync every phone.
	Devices map[string]BundleDevice

	// AMI is the account the compiled manager.conf defines, which the agent
	// logs in with.
	AMI AMIAccount

	// Streams is streams.json (release B): every Stream the dialplan sends
	// to the stream bridge, by UUID, with the tuner pools, sealed
	// (streams.Document.Sealed). It is not one of Files and not part of
	// Version: the PBX never reads it, so a source URL change rewrites only
	// the <fullname>-streams ConfigMap the bridge mounts
	// (install.StreamsConfigMap), never pbx-config.
	Streams streams.Document
}

// BundleDevice is one device in the bundle's index: what the agent needs to
// attribute fetches, registrations and calls to a device, and to serve a
// softphone's document to its token.
type BundleDevice struct {
	// Kind and Name are the resource's.
	Kind drivers.DeviceKind `json:"kind"`
	Name string             `json:"name"`

	// Number is the device's number: its SIP user and the name of its PJSIP
	// AOR. Empty when the device is not routed (Invalid).
	Number string `json:"number,omitempty"`

	// MAC is a Phone's hardware address, upper case: a fetch of
	// SEP<MAC>.cnf.xml is this device's. Empty for softphones and for a
	// Phone that lost its MAC to an older Phone (MACConflict).
	MAC string `json:"mac,omitempty"`

	// Version is the device's own config version: the lower-case hex
	// SHA-256 over its files (as rendered with an empty
	// Device.ConfigVersion) and the values of the secrets they reference.
	// Empty when the device is not rendered.
	Version string `json:"version,omitempty"`

	// ConfigFile is the bundle path of the device's own config document
	// (tftp/SEP<MAC>.cnf.xml, provisioning/<ID>.xml): a served fetch of it is
	// the device's config fetch (status lastConfigFetch). Empty when the
	// device is not rendered.
	ConfigFile string `json:"configFile,omitempty"`

	// Files are the bundle paths of every file the device fetches,
	// ConfigFile and shared files (the 79x5's tftp/dialplan.xml) included,
	// sorted. Empty when the device is not rendered.
	Files []string `json:"files,omitempty"`

	// ProvisioningToken is the Secret key holding a softphone's current
	// one-time provisioning token. Nil for phones.
	ProvisioningToken *SecretRef `json:"provisioningToken,omitempty"`
}

// AMIAccount is the AMI user the compiler renders and the agent logs in as.
type AMIAccount struct {
	// Username is AMIUsername.
	Username string `json:"username"`
	// Password is the Secret key holding the generated AMI password
	// (Settings.AMISecret).
	Password SecretRef `json:"password"`
}

// AMIUsername is the AMI user in the compiled manager.conf.
const AMIUsername = "patchbay-agent"

// Inputs is everything Compile reads: the resources in the PBX namespace and
// the secret values the bundle may reference.
type Inputs struct {
	Phones     []v1alpha1.Phone
	Softphones []v1alpha1.Softphone
	People     []v1alpha1.Person
	Streams    []v1alpha1.Stream

	// Secrets holds the value of every secret a placeholder or the device
	// index may name. The values feed Version and the device versions only;
	// they never appear in Files.
	Secrets map[SecretRef][]byte
}

// Settings are the install-wide settings, from chart values.
type Settings struct {
	// PBXAddress is the address devices register to and fetch config from
	// (the PBX node's IP, e.g. 192.168.0.240).
	PBXAddress string `json:"pbxAddress"`

	// SIPPort is Asterisk's SIP port, udp and tcp (5060).
	SIPPort int `json:"sipPort"`

	// RTPPorts is Asterisk's RTP port range (10000-10099).
	RTPPorts PortRange `json:"rtpPorts"`

	// DeviceHTTPPort is the agent's device HTTP port (8088).
	DeviceHTTPPort int `json:"deviceHTTPPort"`

	// NTPServer is the time server phones use, as an IP address (the voice
	// VLAN gateway, 192.168.20.1).
	NTPServer string `json:"ntpServer"`

	// Timezone is an IANA zone name, e.g. America/Los_Angeles.
	Timezone string `json:"timezone"`

	// Signalling is the signalling backend (design §8.5); only pjsip exists.
	Signalling drivers.Signalling `json:"signalling"`

	// NumberPlan says which numbers mean what.
	NumberPlan NumberPlan `json:"numberPlan"`

	// AllowedNetworks are the CIDRs the PJSIP ACL accepts registrations
	// from (192.168.20.0/24 and 192.168.0.0/24).
	AllowedNetworks []string `json:"allowedNetworks"`

	// StreamBridge is the stream bridge's AudioSocket address, host:port
	// (release B, design §9): the chart renders
	// <fullname>-streams.<namespace>.svc:9092 (install.StreamBridgeAddress)
	// while streamBridge.enabled. Empty disables streams: every Stream's
	// number plays "not available".
	StreamBridge string `json:"streamBridge,omitempty"`

	// Streams are the stream settings (release B): the per-call time
	// limit and the tuner pools.
	Streams StreamSettings `json:"streams,omitzero"`

	// AMISecret is the Secret key holding the generated AMI password. The
	// controller generates it; the compiler only references it.
	AMISecret SecretRef `json:"amiSecret"`

	// PhoneDefaults are house defaults for Phone spec fields. Optional.
	PhoneDefaults PhoneDefaults `json:"phoneDefaults,omitzero"`
}

// PhoneDefaults are the house defaults for Phones: each applies to a Phone
// field by field, where the Phone leaves a field unset (the Phone's own
// value always wins).
type PhoneDefaults struct {
	// Display is the house default for Phone.spec.display, in the same
	// shape (settings.json phoneDefaults.display, e.g. {"idleTimeout":
	// "1m", "wakeOnCall": true}). Each field a Phone's display leaves unset
	// takes this one's, and a Phone without a display gets it whole;
	// onWindow is one field. It applies only to models that support the
	// screen's power saving (drivers.FeatureDisplayPowerSave), and nil means
	// none: a Phone without a display then renders no display settings.
	Display *v1alpha1.PhoneDisplay `json:"display,omitempty"`
}

// StreamSettings are the install's stream settings (settings.json
// "streams", from the chart's streams values; docs/plans/release-b.md
// §2(c), §2(e)).
type StreamSettings struct {
	// CallLimit is how long one call to a stream may last: the dialplan
	// sets TIMEOUT(absolute) to it, so Asterisk hangs up at it and a
	// phone left off the hook cannot hold a tuner for ever. Whole seconds,
	// MinCallLimit to MaxCallLimit; zero is DefaultCallLimit. It applies to
	// every stream, tuner or not.
	CallLimit metav1.Duration `json:"callLimit,omitzero"`

	// TunerPools are the tuner devices whose tuners a stream holds while
	// it plays. A stream whose source's host (its URL's host name, any
	// port) is one of a pool's Hosts counts against that pool: the bridge
	// plays at most Max of the pool's sources at once (one decoder per
	// source, shared by all its callers), and refuses a call that would
	// need one more. A source on no pool's host needs no tuner (internet
	// radio) and has no limit. Names are DNS labels, each once; a host is
	// in one pool at most; Max is 1 to streams.MaxPoolMax.
	TunerPools []TunerPool `json:"tunerPools,omitempty"`
}

// TunerPool is one tuner device, or several that share a limit.
type TunerPool struct {
	// Name names the pool in status (Stream.status.tunerPool) and
	// messages, e.g. "hdhomerun".
	Name string `json:"name"`
	// Hosts are the hosts its sources are on, as the Streams' URLs write
	// them: an IP address or a DNS name, without scheme, port or path
	// ("192.168.0.58"), compared case-insensitively. A Stream that names
	// the same device by another host (a name for the IP) is not in the
	// pool: list every form the Streams use.
	Hosts []string `json:"hosts"`
	// Max is how many of the pool's sources may play at once: for the
	// house's HDHomeRun, 1 of its 2 tuners, so the DVR always keeps one.
	Max int `json:"max"`
}

// The per-call time limit's bounds (StreamSettings.CallLimit).
const (
	DefaultCallLimit = 2 * time.Hour
	MinCallLimit     = 10 * time.Second
	MaxCallLimit     = 24 * time.Hour
)

// Limit is the call limit in effect: CallLimit, or DefaultCallLimit when it
// is zero.
func (s StreamSettings) Limit() time.Duration {
	if s.CallLimit.Duration == 0 {
		return DefaultCallLimit
	}
	return s.CallLimit.Duration
}

// PortRange is an inclusive range of ports.
type PortRange struct {
	First int `json:"first"`
	Last  int `json:"last"`
}

// NumberPlan is the number plan (design §4). Patterns use Asterisk's
// notation: X is any digit, a trailing "." is one or more digits, and "*"
// and "#" are literal.
type NumberPlan struct {
	// Devices is the pattern for phones and softphones ("1XX").
	Devices string `json:"devices"`
	// People is the pattern for people ("3XX").
	People string `json:"people"`
	// Streams is the pattern for streams ("7XX").
	Streams string `json:"streams"`
	// EchoTest is the built-in echo test number ("600").
	EchoTest string `json:"echoTest"`
	// Emergency is the number that plays "this phone cannot call emergency
	// services" ("911").
	Emergency string `json:"emergency"`
	// Reserved patterns play "not available" ("2XX", "0", "9.", "*8").
	Reserved []string `json:"reserved"`
}

// DefaultNumberPlan is the number plan from design §4.
func DefaultNumberPlan() NumberPlan {
	return NumberPlan{
		Devices:   "1XX",
		People:    "3XX",
		Streams:   "7XX",
		EchoTest:  "600",
		Emergency: "911",
		Reserved:  []string{"2XX", "0", "9.", "*8"},
	}
}

// SecretRef names one key of one Secret in the PBX namespace.
type SecretRef struct {
	// Name is the Secret's name.
	Name string `json:"name"`
	// Key is the key within the Secret's data.
	Key string `json:"key"`
}

// Placeholder is the text that stands for the secret in a bundle file.
func (r SecretRef) Placeholder() string {
	return "@@secret:" + r.Name + "/" + r.Key + "@@"
}

// PlaceholderPattern matches one placeholder. Submatch 1 is the Secret name
// (a DNS subdomain), 2 the key (a valid Secret data key). The agent resolves
// exactly what this matches.
var PlaceholderPattern = regexp.MustCompile(`@@secret:([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?)/([-._a-zA-Z0-9]+)@@`)

// Keys of a device's generated Secret.
const (
	// SecretKeySIPPassword holds the device's SIP password, generated by
	// the controller with the driver's PasswordPolicy.
	SecretKeySIPPassword = "sip-password"
	// SecretKeyProvisioningToken holds a softphone's current one-time
	// provisioning token.
	SecretKeyProvisioningToken = "provisioning-token"
)

// DeviceSecretName is the name of the Secret the controller generates for a
// device, owned by the device's resource: "patchbay-" + the device ID, e.g.
// patchbay-phone-desk. Compile references it; the controller creates it.
func DeviceSecretName(deviceID string) string {
	return "patchbay-" + deviceID
}

// PasswordRef is where a device's SIP password lives.
func PasswordRef(deviceID string) SecretRef {
	return SecretRef{Name: DeviceSecretName(deviceID), Key: SecretKeySIPPassword}
}

// ProvisioningTokenRef is where a softphone's provisioning token lives.
func ProvisioningTokenRef(deviceID string) SecretRef {
	return SecretRef{Name: DeviceSecretName(deviceID), Key: SecretKeyProvisioningToken}
}

// Compile builds the bundle for an install, rendering each device through
// the registry's driver for its model. It is pure: the same registry,
// inputs and settings always give the same bundle, byte for byte, whatever
// the order of the resources in Inputs.
//
// The steps:
//  1. Settings.Validate, and every resource name is a DNS subdomain (names
//     reach config files and IDs).
//  2. ResolveClaims: who holds which number and MAC.
//  3. Each Phone, then each Softphone, in name order, becomes a
//     drivers.Device with its claim fields applied. A device that holds its
//     number and has a driver is rendered twice: first with an empty
//     ConfigVersion, which is hashed (with the secrets it references) into
//     the device's version, then with that version set. A Render error
//     wrapping drivers.ErrInvalidSpec leaves the device out (Decide says
//     why); any other error fails Compile. A device's files must lie in its
//     kind's tree and reference no secret but its own SIP password; a path
//     two devices render must be byte-identical (the 79x5's shared
//     dialplan.xml).
//  4. Asterisk's files, the dialplan and the directory, from the routed
//     devices (rendered, holding their number), people and streams.
//  5. The device index (every Phone and Softphone, rendered or not), the
//     AMI account, the bundle version, and Validate.
//
// Labels and person names are free text: Compile collapses every run of
// "@", with any white space between them, to one "@", so no text a person
// writes can form a secret placeholder that the agent would resolve into a
// file served to devices, even after a driver drops its spaces.
func Compile(reg *drivers.Registry, in Inputs, s Settings) (Bundle, error) {
	if reg == nil {
		return Bundle{}, errors.New("compile: no driver registry")
	}
	if err := s.Validate(); err != nil {
		return Bundle{}, err
	}
	if err := checkNames(in); err != nil {
		return Bundle{}, err
	}
	claims := ResolveClaims(in, s.NumberPlan)
	devs, err := buildDevices(reg, in, claims)
	if err != nil {
		return Bundle{}, err
	}

	b := Bundle{
		Files:   map[string][]byte{},
		Devices: map[string]BundleDevice{},
		AMI:     AMIAccount{Username: AMIUsername, Password: s.AMISecret},
	}
	ctx := renderContext(s)
	owners := map[string]string{} // bundle path -> the first device that rendered it
	for i := range devs {
		d := &devs[i]
		if err := d.render(ctx, in.Secrets); err != nil {
			return Bundle{}, err
		}
		for _, p := range slices.Sorted(maps.Keys(d.files)) {
			if prev, shared := b.Files[p]; shared {
				if !bytes.Equal(prev, d.files[p]) {
					return Bundle{}, fmt.Errorf("compile: %s is rendered differently for devices %s and %s", p, owners[p], d.dev.ID)
				}
				continue
			}
			owners[p] = d.dev.ID
			b.Files[p] = d.files[p]
		}
		b.Devices[d.dev.ID] = d.index()
	}

	routedID := map[ResourceRef]string{}
	var endpoints []endpoint
	var deviceTargets []dialTarget
	var directory []directoryEntry
	for _, d := range devs {
		if !d.routed() {
			continue
		}
		routedID[d.ref] = d.dev.ID
		endpoints = append(endpoints, endpoint{dev: d.dev, profile: d.profile})
		deviceTargets = append(deviceTargets, dialTarget{number: d.dev.Number, comment: d.ref.String(), ring: []string{d.dev.ID}})
		directory = append(directory, directoryEntry{Name: d.dev.Label, Number: d.dev.Number})
	}
	slices.SortFunc(endpoints, func(a, b endpoint) int { return cmp.Compare(a.dev.ID, b.dev.ID) })
	people, peopleDirectory := personTargets(in.People, claims, routedID)
	directory = append(directory, peopleDirectory...)

	maps.Copy(b.Files, asteriskFiles(s, endpoints))
	b.Files[TreeAsterisk+"/extensions.conf"] = extensionsConf(s.NumberPlan,
		byNumber(deviceTargets), people, streamTargets(in.Streams, claims))
	if b.Files[DirectoryFile], err = directoryXML(directory); err != nil {
		return Bundle{}, err
	}

	if b.Version, err = b.VersionWith(in.Secrets); err != nil {
		return Bundle{}, err
	}
	if err := b.Validate(); err != nil {
		return Bundle{}, fmt.Errorf("compile: the compiled bundle is invalid: %w", err)
	}
	return b, nil
}

// CheckDrivers checks the settings against every registered driver that
// refuses some install-wide settings (drivers.SettingsChecker): the
// controller runs it at start, with Validate, so a value no device could
// be rendered with (a time zone the 79x5 has no name for) stops it, rather
// than fail every compile once the first such device exists.
func (s Settings) CheckDrivers(reg *drivers.Registry) error {
	if reg == nil {
		return errors.New("settings: no driver registry")
	}
	ctx := renderContext(s)
	var errs []error
	for _, d := range reg.Drivers() {
		if c, ok := d.(drivers.SettingsChecker); ok {
			if err := c.CheckSettings(ctx); err != nil {
				errs = append(errs, fmt.Errorf("settings: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// renderContext is what every device's Render gets but its password.
func renderContext(s Settings) drivers.RenderContext {
	base := fmt.Sprintf("http://%s:%d", s.PBXAddress, s.DeviceHTTPPort)
	return drivers.RenderContext{
		PBXAddress:    s.PBXAddress,
		SIPPort:       s.SIPPort,
		DeviceHTTPURL: base,
		DirectoryURL:  base + strings.TrimPrefix(DirectoryFile, TreeHTTP),
		NTPServer:     s.NTPServer,
		Timezone:      s.Timezone,
		Signalling:    s.Signalling,
		DialPatterns:  s.NumberPlan.DialPatterns(),
		// A copy, so no driver can change the settings (drivers only read
		// it: DisplayWithDefaults copies).
		DisplayDefaults: s.PhoneDefaults.Display.DeepCopy(),
	}
}

// checkNames refuses resource names that are not DNS subdomains, or that a
// kind lists twice: the API server guarantees both, and the compiler writes
// names into IDs, section names and comments.
func checkNames(in Inputs) error {
	var errs []error
	check := func(kind ResourceKind, names []string) {
		seen := map[string]bool{}
		for _, n := range names {
			if problems := validation.IsDNS1123Subdomain(n); len(problems) > 0 {
				errs = append(errs, fmt.Errorf("compile: %s %q: %s", kind, n, strings.Join(problems, "; ")))
			}
			if seen[n] {
				errs = append(errs, fmt.Errorf("compile: %s %q is listed twice", kind, n))
			}
			seen[n] = true
		}
	}
	names := func(n int, name func(int) string) []string {
		out := make([]string, n)
		for i := range n {
			out[i] = name(i)
		}
		return out
	}
	check(ResourcePhone, names(len(in.Phones), func(i int) string { return in.Phones[i].Name }))
	check(ResourceSoftphone, names(len(in.Softphones), func(i int) string { return in.Softphones[i].Name }))
	check(ResourcePerson, names(len(in.People), func(i int) string { return in.People[i].Name }))
	check(ResourceStream, names(len(in.Streams), func(i int) string { return in.Streams[i].Name }))
	return errors.Join(errs...)
}

// device is one Phone or Softphone on its way into the bundle.
type device struct {
	ref     ResourceRef
	dev     drivers.Device
	driver  drivers.Driver // nil when no driver serves the model
	profile drivers.Profile
	// holds is whether the device holds its number (no claim problem).
	holds bool
	// files are the device's files as rendered the second time, and
	// configFile the bundle path of its config document; nil and empty when
	// it is not rendered.
	files      drivers.Files
	configFile string
}

// routed is whether the device is rendered, so in the bundle: in
// pjsip.conf, the dialplan and the directory. Only a device that holds its
// number is rendered, so every rendered device is routed.
func (d *device) routed() bool { return d.files != nil }

// buildDevices builds every Phone, then every Softphone, in name order.
func buildDevices(reg *drivers.Registry, in Inputs, claims Claims) ([]device, error) {
	var out []device
	phones := slices.Clone(in.Phones)
	slices.SortFunc(phones, func(a, b v1alpha1.Phone) int { return cmp.Compare(a.Name, b.Name) })
	for i := range phones {
		out = append(out, newDevice(reg, drivers.PhoneDevice(&phones[i]), claims))
	}
	softphones := slices.Clone(in.Softphones)
	slices.SortFunc(softphones, func(a, b v1alpha1.Softphone) int { return cmp.Compare(a.Name, b.Name) })
	for i := range softphones {
		out = append(out, newDevice(reg, drivers.SoftphoneDevice(&softphones[i]), claims))
	}
	ids := map[string]ResourceRef{}
	for _, d := range out {
		if other, dup := ids[d.dev.ID]; dup {
			return nil, fmt.Errorf("compile: %s and %s have the same device ID %s; rename one", other, d.ref, d.dev.ID)
		}
		ids[d.dev.ID] = d.ref
	}
	return out, nil
}

func newDevice(reg *drivers.Registry, dev drivers.Device, claims Claims) device {
	claims.Apply(&dev)
	dev.Label = drivers.UserText(dev.Label)
	d := device{ref: deviceRef(dev.Kind, dev.Name), dev: dev}
	d.holds = claims.Holds(d.ref)
	if drv, err := reg.ForModel(dev.Model); err == nil {
		d.driver = drv
		d.profile, _ = drv.Profile(dev.Model)
	}
	return d
}

// treeOf is the tree a device kind's files go in.
func treeOf(kind drivers.DeviceKind) string {
	switch kind {
	case drivers.KindPhone:
		return TreeTFTP
	case drivers.KindSoftphone:
		return TreeProvisioning
	}
	return ""
}

// render renders the device twice (see Compile), or leaves it out.
func (d *device) render(base drivers.RenderContext, secrets map[SecretRef][]byte) error {
	if d.driver == nil || !d.holds {
		return nil
	}
	ctx := base
	ctx.SIPPassword = PasswordRef(d.dev.ID).Placeholder()
	first, err := d.driver.Render(d.dev, ctx)
	switch {
	case errors.Is(err, drivers.ErrInvalidSpec):
		return nil
	case err != nil:
		return fmt.Errorf("compile: %s: %w", d.ref, err)
	}
	refs, err := d.checkFiles(first)
	if err != nil {
		return err
	}
	v, err := version(first, nil, refs, secrets)
	if err != nil {
		return fmt.Errorf("compile: %s: %w", d.ref, err)
	}

	stamped := d.dev
	stamped.ConfigVersion = v
	second, err := d.driver.Render(stamped, ctx)
	if err != nil {
		return fmt.Errorf("compile: %s: the second render failed: %w", d.ref, err)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(first)), slices.Sorted(maps.Keys(second))) {
		return fmt.Errorf("compile: %s: the second render gave other files (%v, then %v)",
			d.ref, slices.Sorted(maps.Keys(first)), slices.Sorted(maps.Keys(second)))
	}
	if _, err := d.checkFiles(second); err != nil {
		return err
	}
	cfg, err := d.configPath()
	if err != nil {
		return err
	}
	if _, ok := second[cfg]; !ok {
		return fmt.Errorf("compile: %s: its config document %s is not among its files", d.ref, cfg)
	}
	d.dev.ConfigVersion = v
	d.files = second
	d.configFile = cfg
	return nil
}

// checkFiles checks a device's rendered files: at least one, each a portable
// path in the kind's tree, referencing no secret but the device's own SIP
// password. It returns the secrets they reference.
func (d *device) checkFiles(files drivers.Files) ([]SecretRef, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("compile: %s: its driver rendered no files", d.ref)
	}
	want := treeOf(d.dev.Kind)
	for _, p := range slices.Sorted(maps.Keys(files)) {
		tree, err := TreeOf(p)
		if err != nil {
			return nil, fmt.Errorf("compile: %s: %w", d.ref, err)
		}
		if tree != want {
			return nil, fmt.Errorf("compile: %s: %s is outside %s/, the only tree a %s's files may go in", d.ref, p, want, d.dev.Kind)
		}
	}
	refs, err := placeholders(files)
	if err != nil {
		return nil, fmt.Errorf("compile: %s: %w", d.ref, err)
	}
	own := PasswordRef(d.dev.ID)
	for _, r := range refs {
		if r != own {
			return nil, fmt.Errorf("compile: %s: its files reference secret %s key %s, which is not its own", d.ref, r.Name, r.Key)
		}
	}
	return refs, nil
}

// configPath is the bundle path of the device's config document, from its
// profile's ConfigFile pattern ({MAC} and {ID} expanded; a bare file name
// is in the kind's tree).
func (d *device) configPath() (string, error) {
	pattern := d.profile.Provisioning.ConfigFile
	if pattern == "" {
		return "", fmt.Errorf("compile: %s: model %s's profile names no config file", d.ref, d.dev.Model)
	}
	p := strings.NewReplacer("{MAC}", d.dev.MAC, "{ID}", d.dev.ID).Replace(pattern)
	if tree := treeOf(d.dev.Kind); !strings.HasPrefix(p, tree+"/") {
		p = tree + "/" + p
	}
	return p, nil
}

// index is the device's entry in the bundle's device index.
func (d *device) index() BundleDevice {
	bd := BundleDevice{Kind: d.dev.Kind, Name: d.dev.Name}
	if !d.dev.MACConflict {
		bd.MAC = d.dev.MAC
	}
	if !d.routed() {
		return bd
	}
	bd.Number = d.dev.Number
	bd.Version = d.dev.ConfigVersion
	bd.ConfigFile = d.configFile
	bd.Files = slices.Sorted(maps.Keys(d.files))
	if d.dev.Kind == drivers.KindSoftphone {
		tok := ProvisioningTokenRef(d.dev.ID)
		bd.ProvisioningToken = &tok
	}
	return bd
}

// personTargets is each Person holding its number, sorted by number, with
// the routed devices it rings (in ring order, each once), and a directory
// entry for each that rings at least one.
func personTargets(people []v1alpha1.Person, claims Claims, routedID map[ResourceRef]string) ([]dialTarget, []directoryEntry) {
	var targets []dialTarget
	var directory []directoryEntry
	for i := range people {
		p := &people[i]
		ref := ResourceRef{ResourcePerson, p.Name}
		if !claims.Holds(ref) {
			continue
		}
		t := dialTarget{number: string(p.Spec.Number), comment: ref.String()}
		for _, r := range p.Spec.Rings {
			target := ResourceRef{ResourcePhone, string(r.Phone)}
			if r.Softphone != "" {
				target = ResourceRef{ResourceSoftphone, string(r.Softphone)}
			}
			if id, ok := routedID[target]; ok && !slices.Contains(t.ring, id) {
				t.ring = append(t.ring, id)
			}
		}
		if len(t.ring) == 0 {
			t.comment += ": no routed device to ring"
		} else {
			directory = append(directory, directoryEntry{Name: drivers.UserText(p.Spec.Name), Number: t.number})
		}
		targets = append(targets, t)
	}
	return byNumber(targets), directory
}

// streamTargets is each Stream holding its number, sorted by number. In
// release A there is no stream bridge, so each plays "not available".
func streamTargets(streams []v1alpha1.Stream, claims Claims) []dialTarget {
	var targets []dialTarget
	for i := range streams {
		s := &streams[i]
		ref := ResourceRef{ResourceStream, s.Name}
		if claims.Holds(ref) {
			targets = append(targets, dialTarget{number: string(s.Spec.Number), comment: ref.String() + ": no stream bridge in this release"})
		}
	}
	return byNumber(targets)
}

// byNumber sorts targets by number (shorter first, so 99 before 100).
func byNumber(targets []dialTarget) []dialTarget {
	slices.SortFunc(targets, func(a, b dialTarget) int {
		return cmp.Or(cmp.Compare(len(a.number), len(b.number)), cmp.Compare(a.number, b.number))
	})
	return targets
}
