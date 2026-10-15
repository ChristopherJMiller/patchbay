package compile

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
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
	// directory and the XML services. It never carries a placeholder.
	TreeHTTP = "http"

	// TreeProvisioning holds each softphone's provisioning document (with its
	// SIP password). Device HTTP :8088 serves one only to the holder of that
	// softphone's one-time token, never by path.
	TreeProvisioning = "provisioning"
)

// Trees lists the four trees in a fixed order.
var Trees = []string{TreeAsterisk, TreeTFTP, TreeHTTP, TreeProvisioning}

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
// TestChartUsesTheLiveLayout keeps them in step.
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

// Validate checks the shape every bundle must have, whoever built it:
//   - every path is relative, slash-separated and clean (path.Clean leaves it
//     unchanged, so it has no "." or ".." element), and names a file inside
//     one of the four trees;
//   - asterisk/asterisk.conf exists (Asterisk cannot start without it);
//   - no file in http/ carries a placeholder (it is served to anyone).
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
	return errors.Join(errs...)
}

// TreeOf returns the tree a bundle path belongs to, or an error when the
// path is not a clean relative path to a file inside one of the trees.
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
	for _, t := range Trees {
		if tree == t {
			return tree, nil
		}
	}
	return "", fmt.Errorf("bundle: %q: %q is not a tree (want one of %s)", p, tree, strings.Join(Trees, ", "))
}
