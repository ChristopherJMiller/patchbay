// Package firmware is the firmware store (W2.8, design §12): phone firmware
// on the firmware volume (layout.FirmwareDir), verified against the
// manifests compiled into the binary (each driver's Firmware(), via
// builtin.Registry()), and the upload endpoint behind
// PUT agentapi.FirmwarePath(load, name).
//
// This file is the contract the file server, the upload handler and the
// observation store build on. The implementation lives beside it.
//
// The agent and the separate file server each open their own store over
// the same directory: the agent read-write (uploads, the store sensor), the
// file server read-only (serving). Neither needs the other's state: what is
// on disk, checked against the manifest, is the whole truth. A store whose
// directory does not exist yet is empty (the agent's first upload creates
// it, DirMode), and a store notices the directory appear.
package firmware

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

// The store's layout under its directory.
//
//	<dir>/<load>/<name>     a stored file: complete, and matching its
//	                        manifest digest when it was stored
//	<dir>/.incoming/        uploads in progress (mode 0700), renamed into
//	                        place only once their digest matched
//
// Load directories are DirMode and files FileMode: the file server reads
// them as "other" (layout's identities). Load and file names are manifest
// names, which never hold a slash and never start with ".", so IncomingDir
// is never a load.
const (
	IncomingDir             = ".incoming"
	DirMode     fs.FileMode = 0o755
	FileMode    fs.FileMode = 0o644
)

// ErrNotFound is what Open returns for every name it will not serve.
var ErrNotFound = errors.New("firmware: not found")

// Store is the firmware store as the file servers and the store sensor see
// it.
type Store interface {
	// Status is sensor 4 (sensor.FirmwareSensor): one entry per (load,
	// file) of every load in every manifest, in manifest order, Present
	// only when the file is stored and verified (never for a pending
	// digest). It is called on every observations request, so it hashes a
	// file only when its size or modification time changed since it was
	// last hashed.
	sensor.FirmwareSensor

	// Open opens the file a device asked for by name (over TFTP or HTTP
	// :6970: a bare file name, no directory), positioned at its start; the
	// caller closes it. The name resolves to one load:
	//   - a name that is not Shared belongs to exactly one load;
	//   - a Shared name (term45.default.loads, carried by every load) is
	//     served from the default load only: the one load carrying it that
	//     is the profile DefaultFirmware of a model it lists, so a
	//     factory-reset phone always lands on the default load. No such
	//     load, or several, and the name is not served.
	// The file is verified on the descriptor Open returns (its SHA-256
	// equals the manifest's, cached by device, inode, size and modification
	// time, so it is hashed once per change), so what is served is exactly
	// what was verified. Open returns ErrNotFound for a name in no
	// manifest, a pending digest, a file not stored, or one whose digest no
	// longer matches; any other error is an I/O failure.
	Open(name string) (*File, error)
}

// File is an opened, verified firmware file.
type File struct {
	// File is the open file, at offset 0.
	*os.File
	// Load and Name are the manifest load and file it is.
	Load string
	Name string
	// Size and ModTime are from the descriptor's stat.
	Size    int64
	ModTime time.Time
}
