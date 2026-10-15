package firmware

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Disk is the firmware store on a directory (layout.FirmwareDir): the
// Store the file server serves from, the store sensor, and the target of
// uploads (Put, NewUploadHandler).
//
// Every access goes through an os.Root on the directory, opened on first
// use: while the directory does not exist the store is empty, and the next
// call tries again. Only regular files count: a symlink, even to a
// verified copy, never does. A file is hashed once per change of its
// device, inode, size or modification time (one cache, shared by Open,
// Status and Put); a file over api.MaxFirmwareFileSize, which no upload
// can have stored, is never read and never present.
type Disk struct {
	dir     string
	m       *Manifest
	log     logr.Logger
	maxSize int64                   // uploads; tests lower it
	onHash  func(load, name string) // tests

	mu      sync.Mutex
	root    *os.Root
	digests map[fileRef]digest

	prepMu  sync.Mutex // Put's store preparation
	cleaned bool       // IncomingDir emptied of an earlier process's leftovers

	commit sync.Mutex // Put's check-and-rename
}

type fileRef struct{ load, name string }

// digest is a cached SHA-256 of one stored file, valid while its stat key
// is unchanged. sum is empty for a file too big to have been uploaded.
type digest struct {
	key statKey
	sum string
}

// statKey identifies one content of one file: the same device, inode, size
// and modification time are taken to mean the same bytes.
type statKey struct {
	dev, ino uint64
	size     int64
	mtime    int64
}

// NewDisk is the store on dir with manifest m. It does no I/O; it logs,
// once, each Shared name m will never serve.
func NewDisk(dir string, m *Manifest, log logr.Logger) *Disk {
	if m == nil {
		m = &Manifest{byLoad: map[string]int{}, byName: map[string]ref{}}
	}
	for _, name := range m.unserved {
		log.Info("firmware: a Shared file is never served: no default load carries it, or several do", "name", name)
	}
	return &Disk{
		dir: dir, m: m, log: log, maxSize: api.MaxFirmwareFileSize,
		digests: map[fileRef]digest{},
	}
}

// Open implements Store.
func (d *Disk) Open(name string) (*File, error) {
	load, file, ok := d.m.Resolve(name)
	if !ok || file.Pending() {
		return nil, ErrNotFound
	}
	root, err := d.getRoot()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("firmware: opening the store: %w", err)
	}
	f, fi, err := openRegular(root, load.Name+"/"+file.Name)
	if err != nil {
		if ioFailure(err) {
			return nil, fmt.Errorf("firmware: %w", err)
		}
		return nil, ErrNotFound
	}
	good, err := d.verify(load, file, f, fi)
	if err != nil || !good {
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("firmware: %w", err)
		}
		return nil, ErrNotFound
	}
	return &File{File: f, Load: load.Name, Name: file.Name, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

// Status implements sensor.FirmwareSensor.
func (d *Disk) Status() api.FirmwareStore {
	out := make([]api.FirmwareFileStatus, 0, d.m.size())
	root, rootErr := d.getRoot()
	if rootErr != nil && !errors.Is(rootErr, fs.ErrNotExist) {
		d.log.V(1).Info("firmware: the store cannot be opened", "error", rootErr.Error())
	}
	d.m.each(func(load drivers.FirmwareLoad, file drivers.FirmwareFile) {
		present := rootErr == nil && d.verified(root, load, file)
		out = append(out, api.FirmwareFileStatus{Load: load.Name, Name: file.Name, Present: present})
	})
	return api.FirmwareStore{Files: out}
}

// verified reports whether the store holds file of load with its digest.
// A cached digest whose stat key still holds needs no open.
func (d *Disk) verified(root *os.Root, load drivers.FirmwareLoad, file drivers.FirmwareFile) bool {
	if file.Pending() {
		return false
	}
	rel := load.Name + "/" + file.Name
	li, err := root.Lstat(rel)
	if err != nil || !li.Mode().IsRegular() {
		return false
	}
	if sum, ok := d.cached(fileRef{load.Name, file.Name}, keyOf(li)); ok {
		return sum != "" && sum == file.SHA256
	}
	f, fi, err := openRegular(root, rel)
	if err != nil {
		return false
	}
	defer f.Close()
	good, err := d.verify(load, file, f, fi)
	if err != nil {
		d.log.V(1).Info("firmware: reading a stored file failed", "load", load.Name, "name", file.Name, "error", err.Error())
	}
	return good
}

// verify reports whether the open file f (fi its fstat) has file's digest,
// hashing it unless the cache knows it. f is left at its start.
func (d *Disk) verify(load drivers.FirmwareLoad, file drivers.FirmwareFile, f *os.File, fi fs.FileInfo) (bool, error) {
	ref, key := fileRef{load.Name, file.Name}, keyOf(fi)
	if sum, ok := d.cached(ref, key); ok {
		return sum != "" && sum == file.SHA256, nil
	}
	var sum string
	if fi.Size() <= api.MaxFirmwareFileSize {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return false, fmt.Errorf("hashing %s/%s: %w", load.Name, file.Name, err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return false, fmt.Errorf("rewinding %s/%s: %w", load.Name, file.Name, err)
		}
		if d.onHash != nil {
			d.onHash(load.Name, file.Name)
		}
		sum = hex.EncodeToString(h.Sum(nil))
	}
	d.remember(ref, key, sum)
	return sum != "" && sum == file.SHA256, nil
}

func (d *Disk) cached(ref fileRef, key statKey) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.digests[ref]
	if !ok || c.key != key {
		return "", false
	}
	return c.sum, true
}

func (d *Disk) remember(ref fileRef, key statKey, sum string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.digests[ref] = digest{key: key, sum: sum}
}

// getRoot opens the store directory once it exists.
func (d *Disk) getRoot() (*os.Root, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.root != nil {
		return d.root, nil
	}
	r, err := os.OpenRoot(d.dir)
	if err != nil {
		return nil, err
	}
	d.root = r
	return r, nil
}

// errNotRegular is a name that holds something other than a regular file.
var errNotRegular = errors.New("not a regular file")

// openRegular opens rel in root only if it is a regular file, not a
// symlink: it checks with Lstat, then that the descriptor it opened is
// that same file, so a swap in between cannot slip a symlink through.
func openRegular(root *os.Root, rel string) (*os.File, fs.FileInfo, error) {
	li, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, nil, &fs.PathError{Op: "open", Path: rel, Err: errNotRegular}
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		_ = f.Close()
		return nil, nil, &fs.PathError{Op: "open", Path: rel, Err: errNotRegular}
	}
	return f, fi, nil
}

// ioFailure tells an error of the store itself (no permission, no
// descriptors, a failing disk) from a name that holds nothing servable
// (missing, a symlink, a path that escapes, not a directory): only the
// first is worth more than ErrNotFound.
func ioFailure(err error) bool {
	for _, e := range []error{fs.ErrPermission, syscall.EIO, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM, syscall.ENOSPC} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
