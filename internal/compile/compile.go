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
// command or originate. The agent phase confirms the exact classes against
// Asterisk 22 and the e2e suite asserts them.
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
	"errors"
	"regexp"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
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
	// (release B, design §9). Empty disables streams.
	StreamBridge string `json:"streamBridge,omitempty"`

	// AMISecret is the Secret key holding the generated AMI password. The
	// controller generates it; the compiler only references it.
	AMISecret SecretRef `json:"amiSecret"`
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

// ErrNotImplemented is returned by Compile until the compiler is written.
var ErrNotImplemented = errors.New("compile: not implemented yet")

// Compile builds the bundle for an install, rendering each device through
// the registry's driver for its model. It is pure: the same registry,
// inputs and settings always give the same bundle, byte for byte, whatever
// the order of the resources in Inputs.
func Compile(reg *drivers.Registry, in Inputs, s Settings) (Bundle, error) {
	return Bundle{}, ErrNotImplemented
}
