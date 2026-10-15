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
	// TreeHTTP or TreeProvisioning); Validate checks the shape. The
	// controller decides how paths are encoded as ConfigMap keys.
	Files map[string][]byte

	// Version is the lower-case hex SHA-256 over Files plus the values of the
	// secrets they reference. A regenerated password therefore produces a
	// new version even though no file text changed.
	Version string

	// DeviceVersions maps each device's bundle ID to the version of that
	// device's own config: a SHA-256 over the files the device fetches and
	// the secret values they reference. It is what a device's
	// status.observed.lastConfigFetch.version is compared with (ConfigStale,
	// design §8.3), so a change to one device, or to Asterisk-only config,
	// does not resync every phone.
	DeviceVersions map[string]string
}

// Inputs is everything Compile reads: the resources in the PBX namespace and
// the secret values the bundle may reference.
type Inputs struct {
	Phones     []v1alpha1.Phone
	Softphones []v1alpha1.Softphone
	People     []v1alpha1.Person
	Streams    []v1alpha1.Stream

	// Secrets holds the value of every secret a placeholder may name. The
	// values feed Version and DeviceVersions only; they never appear in
	// Files.
	Secrets map[SecretRef][]byte
}

// Settings are the install-wide settings, from chart values.
type Settings struct {
	// PBXAddress is the address devices register to and fetch config from
	// (the PBX node's IP, e.g. 192.168.0.240).
	PBXAddress string

	// SIPPort is Asterisk's SIP port, udp and tcp (5060).
	SIPPort int

	// RTPPorts is Asterisk's RTP port range (10000-10099).
	RTPPorts PortRange

	// DeviceHTTPPort is the agent's device HTTP port (8088).
	DeviceHTTPPort int

	// NTPServer is the time server phones use, as an IP address (the voice
	// VLAN gateway, 192.168.20.1).
	NTPServer string

	// Timezone is an IANA zone name, e.g. America/Los_Angeles.
	Timezone string

	// Signalling is the signalling backend (design §8.5); only pjsip exists.
	Signalling drivers.Signalling

	// NumberPlan says which numbers mean what.
	NumberPlan NumberPlan

	// AllowedNetworks are the CIDRs the PJSIP ACL accepts registrations
	// from (192.168.20.0/24 and 192.168.0.0/24).
	AllowedNetworks []string

	// StreamBridge is the stream bridge's AudioSocket address, host:port
	// (release B, design §9). Empty disables streams.
	StreamBridge string
}

// PortRange is an inclusive range of ports.
type PortRange struct {
	First, Last int
}

// NumberPlan is the number plan (design §4). Patterns use Asterisk's
// notation: X is any digit, "." is one or more of anything.
type NumberPlan struct {
	// Devices is the pattern for phones and softphones ("1XX").
	Devices string
	// People is the pattern for people ("3XX").
	People string
	// Streams is the pattern for streams ("7XX").
	Streams string
	// EchoTest is the built-in echo test number ("600").
	EchoTest string
	// Emergency is the number that plays "this phone cannot call emergency
	// services" ("911").
	Emergency string
	// Reserved patterns play "not available" ("2XX", "0", "9.", "*8").
	Reserved []string
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
	Name string
	// Key is the key within the Secret's data.
	Key string
}

// Placeholder is the text that stands for the secret in a bundle file.
func (r SecretRef) Placeholder() string {
	return "@@secret:" + r.Name + "/" + r.Key + "@@"
}

// PlaceholderPattern matches one placeholder. Submatch 1 is the Secret name
// (a DNS subdomain), 2 the key (a valid Secret data key). The agent resolves
// exactly what this matches.
var PlaceholderPattern = regexp.MustCompile(`@@secret:([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?)/([-._a-zA-Z0-9]+)@@`)

// ErrNotImplemented is returned by Compile until the compiler is written.
var ErrNotImplemented = errors.New("compile: not implemented yet")

// Compile builds the bundle for an install. It is pure: the same inputs and
// settings always give the same bundle, byte for byte.
func Compile(in Inputs, s Settings) (Bundle, error) {
	return Bundle{}, ErrNotImplemented
}
