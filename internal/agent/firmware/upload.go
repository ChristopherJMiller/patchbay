package firmware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// incomingMode is the upload area's mode: the agent's alone.
const incomingMode fs.FileMode = 0o700

// NewUploadHandler serves PUT api.FirmwarePath(load, name) for the agent's
// control API (agent.Options.Firmware), which authenticates the request
// and routes it with the path values "load" and "name". Every answer is an
// api.FirmwareUploadResponse, with the status codes api.PathFirmware
// lists; anything but PUT is 405.
//
// The refusals that need no body (404, 412, and 413 by Content-Length)
// come first, so a body that could never be stored is never read.
func NewUploadHandler(d *Disk, log logr.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		load, name := r.PathValue("load"), r.PathValue("name")
		code, resp := d.upload(r, load, name)
		if code == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", http.MethodPut)
		}
		if code >= http.StatusInternalServerError {
			log.Error(errors.New(resp.Error), "firmware upload failed", "load", load, "name", name, "status", code)
		} else {
			log.Info("firmware upload", "load", load, "name", name, "status", code,
				"size", resp.Size, "sha256", resp.SHA256, "stored", resp.Stored)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func (d *Disk) upload(r *http.Request, load, name string) (int, api.FirmwareUploadResponse) {
	resp := api.FirmwareUploadResponse{Load: load, Name: name}
	if r.Method != http.MethodPut {
		resp.Error = "firmware is uploaded with PUT"
		return http.StatusMethodNotAllowed, resp
	}
	l, f, code, msg := d.precheck(load, name)
	if code != 0 {
		resp.Error = msg
		return code, resp
	}
	if r.ContentLength > d.maxSize {
		resp.Error = d.tooLarge()
		resp.Stored = d.storedNow(l, f)
		return http.StatusRequestEntityTooLarge, resp
	}
	return d.Put(r.Context(), load, name, r.Body)
}

// precheck is what refuses an upload before its body: no such file (404),
// or one whose digest is pending (412). code is 0 when neither applies.
func (d *Disk) precheck(load, name string) (drivers.FirmwareLoad, drivers.FirmwareFile, int, string) {
	l, f, ok := d.m.Lookup(load, name)
	switch {
	case !ok:
		return l, f, http.StatusNotFound, fmt.Sprintf("no firmware manifest has a file %q in a load %q", name, load)
	case f.Pending():
		return l, f, http.StatusPreconditionFailed,
			"the manifest's SHA-256 of this file is pending, so nothing can verify it and nothing is stored"
	}
	return l, f, 0, ""
}

func (d *Disk) tooLarge() string {
	return fmt.Sprintf("a firmware file is at most %d bytes", d.maxSize)
}

// Put stores body as file name of load, if its SHA-256 is the manifest's,
// and returns the HTTP status and the response NewUploadHandler answers
// with (api.PathFirmware): 404 no such file; 412 a pending digest (body not
// read); 413 over the size limit; 422 another digest; 200 already stored
// and verified; 201 stored now; 400 the body failed or ctx ended; 500 the
// store failed.
//
// The body streams into a temp file in IncomingDir (0600) through SHA-256;
// only a match is synced, made FileMode and renamed to <load>/<name>, and
// the directory synced, so the store never holds a partial or unverified
// file under a served name. Put creates the store directory (DirMode),
// IncomingDir (0700) and the load directory (DirMode) when missing, and
// the first Put of a process removes what an earlier one left in
// IncomingDir. Concurrent uploads of one file are safe: each has its own
// temp file, and the check-and-rename is serialized, so exactly one of
// them is 201.
func (d *Disk) Put(ctx context.Context, load, name string, body io.Reader) (int, api.FirmwareUploadResponse) {
	resp := api.FirmwareUploadResponse{Load: load, Name: name}
	l, f, code, msg := d.precheck(load, name)
	if code != 0 {
		resp.Error = msg
		return code, resp
	}
	fail := func(code int, format string, args ...any) (int, api.FirmwareUploadResponse) {
		resp.Error = fmt.Sprintf(format, args...)
		return code, resp
	}

	root, err := d.prepare()
	if err != nil {
		return fail(http.StatusInternalServerError, "preparing the firmware store: %v", err)
	}
	tmpName, tmp, err := createTemp(root)
	if err != nil {
		return fail(http.StatusInternalServerError, "creating a temp file: %v", err)
	}
	renamed := false
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		if !renamed {
			_ = root.Remove(tmpName)
		}
	}()

	h := sha256.New()
	src := &bodyReader{ctx: ctx, r: body}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, d.maxSize+1))
	resp.Size = n
	switch {
	case n > d.maxSize:
		resp.Stored = d.storedNow(l, f)
		return fail(http.StatusRequestEntityTooLarge, "%s", d.tooLarge())
	case err != nil && src.err != nil:
		return fail(http.StatusBadRequest, "reading the upload: %v", src.err)
	case err != nil:
		return fail(http.StatusInternalServerError, "writing the upload: %v", err)
	}
	resp.SHA256 = hex.EncodeToString(h.Sum(nil))
	if resp.SHA256 != f.SHA256 {
		resp.Stored = d.storedNow(l, f)
		return fail(http.StatusUnprocessableEntity, "the SHA-256 of the upload is %s; the manifest's is %s", resp.SHA256, f.SHA256)
	}

	if err := tmp.Sync(); err != nil {
		return fail(http.StatusInternalServerError, "syncing the upload: %v", err)
	}
	if err := tmp.Chmod(FileMode); err != nil {
		return fail(http.StatusInternalServerError, "setting the upload's mode: %v", err)
	}
	fi, err := tmp.Stat()
	if err != nil {
		return fail(http.StatusInternalServerError, "reading the upload's stat: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fail(http.StatusInternalServerError, "closing the upload: %v", err)
	}
	tmp = nil

	d.commit.Lock()
	defer d.commit.Unlock()
	if d.storedNow(l, f) {
		resp.Stored = true
		return http.StatusOK, resp
	}
	created, err := mkdirIn(root, l.Name, DirMode)
	if err != nil {
		return fail(http.StatusInternalServerError, "creating the load directory: %v", err)
	}
	if created {
		if err := syncDir(root, "."); err != nil {
			return fail(http.StatusInternalServerError, "syncing the store directory: %v", err)
		}
	}
	if err := root.Rename(tmpName, l.Name+"/"+f.Name); err != nil {
		return fail(http.StatusInternalServerError, "moving the upload into place: %v", err)
	}
	renamed = true
	d.remember(fileRef{l.Name, f.Name}, keyOf(fi), resp.SHA256)
	if err := syncDir(root, l.Name); err != nil {
		return fail(http.StatusInternalServerError, "syncing the load directory: %v", err)
	}
	resp.Stored = true
	return http.StatusCreated, resp
}

// storedNow reports whether the store holds file of load, verified.
func (d *Disk) storedNow(load drivers.FirmwareLoad, file drivers.FirmwareFile) bool {
	root, err := d.getRoot()
	return err == nil && d.verified(root, load, file)
}

// prepare creates the store directory and IncomingDir when missing, and
// once per process empties IncomingDir: only this process uploads, and
// none of its uploads has started yet.
func (d *Disk) prepare() (*os.Root, error) {
	d.prepMu.Lock()
	defer d.prepMu.Unlock()
	if err := os.Mkdir(d.dir, DirMode); err == nil {
		if err := os.Chmod(d.dir, DirMode); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	root, err := d.getRoot()
	if err != nil {
		return nil, err
	}
	if _, err := mkdirIn(root, IncomingDir, incomingMode); err != nil {
		return nil, err
	}
	if !d.cleaned {
		dir, err := root.Open(IncomingDir)
		if err != nil {
			return nil, err
		}
		entries, err := dir.ReadDir(-1)
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if err := root.RemoveAll(IncomingDir + "/" + e.Name()); err != nil {
				return nil, err
			}
		}
		if len(entries) > 0 {
			d.log.Info("firmware: removed uploads an earlier agent left unfinished", "count", len(entries))
		}
		d.cleaned = true
	}
	return root, nil
}

// mkdirIn makes the directory name in root with exactly mode (not left to
// the umask), unless it exists. created says whether it did.
func mkdirIn(root *os.Root, name string, mode fs.FileMode) (created bool, err error) {
	err = root.Mkdir(name, mode)
	switch {
	case errors.Is(err, fs.ErrExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, root.Chmod(name, mode)
}

func createTemp(root *os.Root) (string, *os.File, error) {
	name := IncomingDir + "/" + rand.Text()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = root.Remove(name)
		return "", nil, err
	}
	return name, f, nil
}

func syncDir(root *os.Root, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// bodyReader reads an upload's body, ending with ctx, and keeps the error
// reading failed with (as opposed to writing).
type bodyReader struct {
	ctx context.Context
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		b.err = err
		return 0, err
	}
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}
