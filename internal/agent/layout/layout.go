// Package layout is the PBX pod's on-disk contract: where each volume is
// mounted, who may read what on it, and how an applied version is laid out
// in the live directory. The agent (apply, the firmware store, device
// HTTP), the separate file server and the chart all build on it; the chart
// hard-codes the same paths and uids, and tests keep the two in step.
//
// Who runs as whom (docs/plans/release-a.md, W2 task level):
//
//	container   uid:gid        supplementary groups   reads
//	agent       65532:65532    FSGroup (pod fsGroup)  everything it writes
//	asterisk    1000:1000      FSGroup (= its gid)    asterisk/ (group read)
//	fileserver  0, then        none (it drops them)   only what is world-readable:
//	            65533:65533                           tftp/ and the firmware store
//
// The file server mounts the whole live volume read-only (a subPath would
// pin one version and miss the swap), so the modes below are what keep the
// resolved Asterisk files (every SIP password, the AMI password) and the
// softphone documents away from the process that parses untrusted TFTP
// packets: they are never world-readable.
package layout

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// Mount points in the PBX pod, besides compile.LiveDir and
// compile.SoundsDir.
const (
	// PersistentDir is where the firmware PVC is mounted, whole (an emptyDir
	// when the chart's firmware persistence is off): read-write in the
	// agent, read-only in the file server, nowhere else. Not as subPaths:
	// the kubelet creates a missing subPath directory itself, owned by
	// root, with a mode that need not let the file server (another uid, no
	// groups) in. The agent creates FirmwareDir and StateDir in it, with
	// the modes it needs.
	PersistentDir = "/var/lib/patchbay/persistent"

	// FirmwareDir is the firmware store: <FirmwareDir>/<load>/<name>,
	// directories 0755 and files 0644 (firmware.DirMode, FileMode), so the
	// file server reads them as "other". The agent's upload creates it.
	FirmwareDir = PersistentDir + "/firmware"

	// StateDir is the agent's own small persistent state, mode 0700 (the
	// file server mounts the volume but cannot enter it), so it survives
	// agent restarts and PBX pod recreation. Device HTTP creates it and
	// keeps its spent provisioning tokens here (SpentTokensFile).
	StateDir = PersistentDir + "/state"

	// StateDirMode is StateDir's mode.
	StateDirMode fs.FileMode = 0o700

	// SpentTokensFile is device HTTP's record of spent provisioning tokens,
	// in StateDir: token SHA-256s only, never a token.
	SpentTokensFile = "spent-tokens.json"

	// FetchSocketDir is an emptyDir (medium Memory) shared by the agent
	// (read-write) and the file server (read-only: connecting to a unix
	// socket needs no write access to its file system).
	FetchSocketDir = "/var/run/patchbay/fetchlog"

	// FetchSocket is the unix socket the agent listens on and the file
	// server reports each transfer to (sensor.WriteFetchEvent).
	FetchSocket = FetchSocketDir + "/fetches.sock"

	// FetchSocketMode is the socket's mode once the agent has bound it: the
	// file server runs as another uid without the pod's group, and connect
	// needs write permission on the socket. Only the agent and the file
	// server mount FetchSocketDir.
	FetchSocketMode fs.FileMode = 0o666

	// ModulesStagingDir is where the modules init container (the Asterisk
	// image) mounts the narrowed module volume, read-write, to copy
	// compile.Modules() into it from compile.AsteriskModuleDir. The Asterisk
	// container mounts the same volume read-only at
	// compile.AsteriskModuleDir, hiding the image's own modules.
	ModulesStagingDir = "/var/lib/patchbay/asterisk-modules"
)

// Identities in the PBX pod.
const (
	// AgentUID and AgentGID are the agent's (the image's nonroot user).
	AgentUID = 65532
	AgentGID = 65532

	// FileserverUID and FileserverGID are what the file server drops to
	// after binding its ports: another uid than the agent's, so it cannot
	// read the agent's files, and with no supplementary groups, so the
	// pod's fsGroup gives it nothing either.
	FileserverUID = 65533
	FileserverGID = 65533

	// AsteriskUID is the Asterisk image's asterisk user.
	AsteriskUID = 1000

	// DefaultFSGroup is the chart's pbx.fsGroup: Asterisk's group, which
	// the agent gives every live file Asterisk must read.
	DefaultFSGroup = 1000
)

// The live directory (compile.LiveDir). Each applied version lives in its
// own directory, named VersionDir(version), holding the bundle's four trees
// and the version's IndexFile; compile.CurrentLink points at the applied
// one. Apply writes a version completely (into a staging name, then one
// rename to VersionDir) before it repoints the link, again by renaming, so
// a reader that resolves the link once and opens files under that version's
// directory always reads one complete version.
//
// Every directory and file apply writes gets exactly these modes (never
// left to the umask) and the group FSGroup (set explicitly, never left to
// setgid inheritance):
//
//	v<version>/              0755  the file server must traverse it
//	v<version>/bundle.json   0640  the version's compile.Index
//	v<version>/tftp/...      0755 / 0644  world-readable: the file server serves it
//	v<version>/asterisk/...  0750 / 0640  Asterisk reads it through the group
//	v<version>/http/...      0750 / 0640  the agent serves it (device HTTP)
//	v<version>/provisioning/ 0700 / 0600  the agent alone (one-time tokens)
const (
	// VersionDirPrefix starts every version directory's name.
	VersionDirPrefix = "v"

	// IndexFile is the version's compile.Index as JSON (exactly the
	// pbx-config ConfigMap's compile.IndexKey value), beside its trees.
	// Observations attribute a fetch of an older version through it.
	IndexFile = "bundle.json"

	// VersionDirMode and IndexFileMode are the modes of a version's
	// directory and its IndexFile.
	VersionDirMode fs.FileMode = 0o755
	IndexFileMode  fs.FileMode = 0o640
)

// TreeModes returns the modes apply gives a tree's directories and files.
// ok is false for a name that is not a tree.
func TreeModes(tree string) (dir, file fs.FileMode, ok bool) {
	switch tree {
	case compile.TreeTFTP:
		return 0o755, 0o644, true
	case compile.TreeAsterisk, compile.TreeHTTP:
		return 0o750, 0o640, true
	case compile.TreeProvisioning:
		return 0o700, 0o600, true
	}
	return 0, 0, false
}

// versionPattern is a bundle version: a lower-case hex SHA-256. The agent
// accepts no other version, because a version names a directory.
var versionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidVersion reports whether v is a bundle version (64 lower-case hex
// digits). The agent refuses a ConfigMap whose version is anything else.
func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

// VersionDir is the name of a version's directory in the live directory:
// VersionDirPrefix + version. It fails for anything but a valid version,
// so no version can name a path outside the live directory.
func VersionDir(version string) (string, error) {
	if !ValidVersion(version) {
		return "", fmt.Errorf("layout: %q is not a bundle version", version)
	}
	return VersionDirPrefix + version, nil
}

// VersionFromDir is the inverse of VersionDir.
func VersionFromDir(name string) (string, bool) {
	v, ok := strings.CutPrefix(name, VersionDirPrefix)
	if !ok || !ValidVersion(v) {
		return "", false
	}
	return v, true
}

// ErrNoCurrent is returned by Current before the first apply.
var ErrNoCurrent = errors.New("layout: no version is live yet")

// Current returns the version the live directory's CurrentLink names. A
// server resolves it once per request and then reads only under that
// version (OpenTree), so a swap during a transfer never mixes versions, and
// it reports the version with the fetch (sensor.FetchEvent.BundleVersion).
func Current(liveDir string) (string, error) {
	target, err := os.Readlink(filepath.Join(liveDir, compile.CurrentLink))
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoCurrent
	}
	if err != nil {
		return "", fmt.Errorf("layout: reading the %s link: %w", compile.CurrentLink, err)
	}
	v, ok := VersionFromDir(target)
	if !ok {
		return "", fmt.Errorf("layout: the %s link names %q, not a version directory", compile.CurrentLink, target)
	}
	return v, nil
}

// OpenTree opens one tree of one version as an os.Root, so nothing a
// device asks for can name a file outside that tree. The caller closes it.
func OpenTree(liveDir, version, tree string) (*os.Root, error) {
	if _, _, ok := TreeModes(tree); !ok {
		return nil, fmt.Errorf("layout: %q is not a tree", tree)
	}
	dir, err := VersionDir(version)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(filepath.Join(liveDir, dir, tree))
}
