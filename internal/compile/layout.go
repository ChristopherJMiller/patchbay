package compile

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// A bundle's files live in four trees, named by the first element of each
// path. The tree decides who can read a file once it is applied, so the
// compiler puts each file in the narrowest tree that works, and the agent
// serves each tree only from that tree's own root (an os.Root opened on
// LiveDir/current/<tree>), never from the whole live directory.
const (
	// TreeAsterisk holds every file Asterisk loads: its astetcdir. Nothing
	// serves it to any device. Once resolved, pjsip.conf and manager.conf
	// carry every SIP password and the AMI password.
	TreeAsterisk = "asterisk"

	// TreeTFTP holds the files phones fetch by name, unauthenticated, over
	// TFTP :69 and HTTP :6970 from the voice VLAN and the LAN:
	// SEP<MAC>.cnf.xml, the dial template, and later ringtones and
	// wallpapers. A phone's own SIP password is in its SEP file in clear;
	// design §5 accepts exactly that, and nothing else secret belongs here.
	// Firmware is not in the bundle: the agent serves it from the firmware
	// volume next to this tree.
	TreeTFTP = "tftp"

	// TreeHTTP holds what device HTTP :8088 serves to anyone who asks: the
	// directory and the XML services, http/<p> at the URL path /<p>. It
	// never carries a placeholder.
	TreeHTTP = "http"

	// TreeProvisioning holds each softphone's provisioning document (with its
	// SIP password). Device HTTP :8088 serves one only to the holder of that
	// softphone's one-time token (at agentapi.ProvisioningPathPrefix +
	// token), never by path.
	TreeProvisioning = "provisioning"
)

// Trees lists the four trees in a fixed order.
var Trees = []string{TreeAsterisk, TreeTFTP, TreeHTTP, TreeProvisioning}

// The phones' directory and Services menu, in http/ (directory.go). Device
// HTTP serves http/<p> at /<p>.
const (
	// DirectoryFile is the Directories menu (a CiscoIPPhoneMenu), served at
	// /directory.xml: the 79x5's directoryURL. Phones hold that URL, so
	// the path never moves.
	DirectoryFile = TreeHTTP + "/directory.xml"

	// ServicesFile is the Services menu, the same CiscoIPPhoneMenu, served
	// at /services.xml: the 79x5's servicesURL.
	ServicesFile = TreeHTTP + "/services.xml"

	// ExtensionsFile is the CiscoIPPhoneDirectory the menus' Extensions
	// item opens: every routed Phone and Softphone, and every Person who
	// rings at least one of them.
	ExtensionsFile = TreeHTTP + "/directory/extensions.xml"

	// StreamsFile is the CiscoIPPhoneDirectory the menus' Streams item
	// opens: every Stream whose number dials the stream bridge. Neither
	// the file nor the item exists while no Stream does.
	StreamsFile = TreeHTTP + "/directory/streams.xml"
)

// The live directory (design §5, Apply). The PBX pod mounts one volume at
// LiveDir in both containers. The agent writes each version into a directory
// of its own and then repoints the CurrentLink symlink at it in one rename,
// so the paths below never change from one version to the next and a reload
// always reads a complete version:
//
//	/var/lib/patchbay/config/
//	  current -> v<version>        swapped atomically on apply and rollback
//	  v<version>/asterisk/...      Asterisk (asterisk -C AsteriskConfigDir/asterisk.conf)
//	  v<version>/tftp/...          TFTP :69 and HTTP :6970
//	  v<version>/http/...          device HTTP :8088
//	  v<version>/provisioning/...  device HTTP :8088, per one-time token
//
// The chart hard-codes the same paths (charts/patchbay/templates/pbx.yaml);
// test/chart's TestChartUsesTheLiveLayout renders it and keeps them in step.
const (
	// LiveDir is where the PBX pod mounts the live-config volume, in the
	// agent and in Asterisk.
	LiveDir = "/var/lib/patchbay/config"

	// CurrentLink is the symlink in LiveDir that names the applied version's
	// directory.
	CurrentLink = "current"

	// AsteriskConfigDir is Asterisk's astetcdir through the symlink. The
	// compiled asterisk.conf names it, and the chart starts Asterisk with
	// -C AsteriskConfigDir/asterisk.conf, so a reload after a swap reads
	// the new version.
	AsteriskConfigDir = LiveDir + "/" + CurrentLink + "/" + TreeAsterisk
)

// Sound prompts. The Asterisk image ships none, and recordings cannot be
// committed, so the prompts are generated when the patchbay image is built
// (text-to-speech from text in the repo; nothing binary in git) and the
// agent installs them into a volume the PBX pod mounts at SoundsDir in both
// containers. The compiled dialplan plays them by absolute path,
// Playback(<SoundsDir>/<name>), so Asterisk's own data directory stays the
// image's. Each prompt exists as <name>.ulaw (8 kHz) and <name>.g722
// (16 kHz), so neither a G.711 nor a G.722 call transcodes.
const (
	// SoundsDir is where the prompts are mounted.
	SoundsDir = "/var/lib/patchbay/sounds"

	// SoundNoEmergency is the 911 announcement: "This phone cannot call
	// emergency services. Hang up and use a mobile phone."
	SoundNoEmergency = "no-emergency"

	// SoundNotAvailable is what reserved and unassigned numbers play:
	// "That number is not available."
	SoundNotAvailable = "not-available"
)

// pathElement is one element of a bundle path below its tree: ASCII letters,
// digits, ".", "-" and "_", neither starting nor ending with "_" and never
// holding "__". That keeps every path a portable TFTP/HTTP file name and
// lets ConfigMapKey encode "/" as "__" reversibly.
var pathElement = regexp.MustCompile(`^[A-Za-z0-9.-]([A-Za-z0-9._-]*[A-Za-z0-9.-])?$`)

// Validate checks the shape every bundle must have, whoever built it:
//   - every path is relative, slash-separated and clean (path.Clean leaves it
//     unchanged, so it has no "." or ".." element), names a file inside one
//     of the four trees, and uses only portable names (see TreeOf);
//   - asterisk/asterisk.conf exists (Asterisk cannot start without it);
//   - no file in http/ carries a placeholder (it is served to anyone);
//   - the AMI account is set;
//   - the device index is consistent: each key is DeviceID(kind, name);
//     a rendered device (Version set) has a ConfigFile in its own tree
//     (tftp/ for a Phone, provisioning/ for a Softphone) and sorted, unique
//     Files that all exist, ConfigFile among them; an unrendered one has
//     neither; only a rendered Softphone, and every one, names its
//     provisioning token; no two devices share a MAC or a number.
//
// The agent validates again before it applies, so a bundle that breaks the
// layout is refused whole, never half served.
func (b Bundle) Validate() error {
	var errs []error
	paths := make([]string, 0, len(b.Files))
	for p := range b.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		tree, err := TreeOf(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if tree == TreeHTTP && PlaceholderPattern.Match(b.Files[p]) {
			errs = append(errs, fmt.Errorf("bundle: %s: a file in %s/ is served to anyone and must not carry a secret placeholder", p, TreeHTTP))
		}
	}
	if _, ok := b.Files[TreeAsterisk+"/asterisk.conf"]; !ok {
		errs = append(errs, fmt.Errorf("bundle: %s/asterisk.conf is missing", TreeAsterisk))
	}
	if b.AMI.Username == "" || b.AMI.Password.Name == "" || b.AMI.Password.Key == "" {
		errs = append(errs, errors.New("bundle: the AMI account is not set"))
	}
	errs = append(errs, b.validateDevices()...)
	return errors.Join(errs...)
}

func (b Bundle) validateDevices() []error {
	var errs []error
	ids := make([]string, 0, len(b.Devices))
	for id := range b.Devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	macs, numbers := map[string]string{}, map[string]string{}
	for _, id := range ids {
		d := b.Devices[id]
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("bundle: device %s: "+format, append([]any{id}, args...)...))
		}
		var tree string
		switch d.Kind {
		case drivers.KindPhone:
			tree = TreeTFTP
		case drivers.KindSoftphone:
			tree = TreeProvisioning
		default:
			bad("unknown kind %q", d.Kind)
			continue
		}
		if want := drivers.DeviceID(d.Kind, d.Name); id != want {
			bad("ID should be %q", want)
		}
		rendered := d.Version != ""
		switch {
		case rendered && !strings.HasPrefix(d.ConfigFile, tree+"/"):
			bad("config file %q is not in %s/", d.ConfigFile, tree)
		case rendered && !slices.Contains(d.Files, d.ConfigFile):
			bad("config file %q is not among its files", d.ConfigFile)
		case !rendered && (d.ConfigFile != "" || len(d.Files) > 0):
			bad("has files but no version")
		}
		if !slices.IsSorted(d.Files) || len(slices.Compact(slices.Clone(d.Files))) != len(d.Files) {
			bad("files are not sorted and unique")
		}
		for _, f := range d.Files {
			if _, ok := b.Files[f]; !ok {
				bad("file %s is not in the bundle", f)
			}
		}
		if wantToken := rendered && d.Kind == drivers.KindSoftphone; (d.ProvisioningToken != nil) != wantToken {
			bad("provisioning token set=%t, want %t", d.ProvisioningToken != nil, wantToken)
		}
		if d.MAC != "" {
			if other, dup := macs[d.MAC]; dup {
				bad("MAC %s is also device %s's", d.MAC, other)
			}
			macs[d.MAC] = id
		}
		if d.Number != "" {
			if other, dup := numbers[d.Number]; dup {
				bad("number %s is also device %s's", d.Number, other)
			}
			numbers[d.Number] = id
		}
	}
	return errs
}

// TreeOf returns the tree a bundle path belongs to, or an error when the
// path is not a clean relative path to a file inside one of the trees, with
// every element below the tree a portable name (ASCII letters, digits, ".",
// "-" and "_", not starting or ending with "_", never holding "__").
func TreeOf(p string) (string, error) {
	switch {
	case p == "":
		return "", errors.New("bundle: empty path")
	case strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("bundle: %q: path must be relative", p)
	case strings.Contains(p, `\`):
		return "", fmt.Errorf("bundle: %q: path must use forward slashes", p)
	case path.Clean(p) != p:
		return "", fmt.Errorf("bundle: %q: path is not clean (want %q)", p, path.Clean(p))
	}
	tree, rest, ok := strings.Cut(p, "/")
	if !ok || rest == "" {
		return "", fmt.Errorf("bundle: %q: not a file inside a tree", p)
	}
	if !slices.Contains(Trees, tree) {
		return "", fmt.Errorf("bundle: %q: %q is not a tree (want one of %s)", p, tree, strings.Join(Trees, ", "))
	}
	for _, el := range strings.Split(rest, "/") {
		if !pathElement.MatchString(el) || strings.Contains(el, "__") {
			return "", fmt.Errorf("bundle: %q: %q is not a portable file name", p, el)
		}
	}
	return tree, nil
}
