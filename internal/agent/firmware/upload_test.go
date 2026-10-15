package firmware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
)

// watchedBody is a request body that records whether anyone read it.
type watchedBody struct {
	r    io.Reader
	read bool
}

func (b *watchedBody) Read(p []byte) (int, error) {
	b.read = true
	return b.r.Read(p)
}

// upload PUTs body to the handler as the agent's mux would route it.
func upload(t *testing.T, h http.Handler, method, load, name string, body io.Reader, length int64) (int, api.FirmwareUploadResponse) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, api.FirmwarePath(load, name), body)
	req.ContentLength = length
	req.SetPathValue("load", load)
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var resp api.FirmwareUploadResponse
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("%s %s/%s: the answer is not a FirmwareUploadResponse: %v", method, load, name, err)
	}
	if resp.Load != load || resp.Name != name {
		t.Errorf("answer names %s/%s, want %s/%s", resp.Load, resp.Name, load, name)
	}
	return rec.Code, resp
}

func put(t *testing.T, h http.Handler, load, name string, b []byte) (int, api.FirmwareUploadResponse) {
	t.Helper()
	return upload(t, h, http.MethodPut, load, name, bytes.NewReader(b), int64(len(b)))
}

// incoming lists what is left in the upload area.
func incoming(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(dir, firmware.IncomingDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func mode(t *testing.T, p string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

// TestUpload: every answer of PUT FirmwarePath(load, name).
func TestUpload(t *testing.T) {
	d, dir, hashed := newDisk(t)
	if err := os.Remove(dir); err != nil { // Put creates the store
		t.Fatal(err)
	}
	h := firmware.NewUploadHandler(d, logr.Discard())
	good := content("A1", "a1.loads")

	// 201: verified and stored.
	code, resp := put(t, h, "A1", "a1.loads", good)
	if code != http.StatusCreated || !resp.Stored || resp.Error != "" || resp.SHA256 != digest(good) || resp.Size != int64(len(good)) {
		t.Fatalf("first upload = %d %+v, want 201 stored", code, resp)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "A1", "a1.loads")); err != nil || !bytes.Equal(got, good) {
		t.Fatalf("stored bytes: %v", err)
	}
	if _, b, err := openBytes(t, d, "a1.loads"); err != nil || !bytes.Equal(b, good) {
		t.Errorf("Open after upload: %v", err)
	}
	// The upload hashed it; neither Open nor Status hashes it again.
	if !present(d.Status(), "A1", "a1.loads") {
		t.Error("Status: not present after a 201")
	}
	if got := hashed(); len(got) != 0 {
		t.Errorf("after the upload, Open and Status hashed %v", got)
	}

	// 200: the same file again.
	code, resp = put(t, h, "A1", "a1.loads", good)
	if code != http.StatusOK || !resp.Stored || resp.SHA256 != digest(good) {
		t.Errorf("repeat upload = %d %+v, want 200 stored", code, resp)
	}

	// 404: no such load, no such file in it.
	for _, tc := range [][2]string{{"A9", "a1.loads"}, {"A1", "a2.loads"}, {"A1", "nope"}, {".incoming", "x"}} {
		body := &watchedBody{r: bytes.NewReader(good)}
		code, resp := upload(t, h, http.MethodPut, tc[0], tc[1], body, int64(len(good)))
		if code != http.StatusNotFound || resp.Stored || resp.Error == "" || body.read {
			t.Errorf("%s/%s = %d %+v (body read %v), want 404 unread", tc[0], tc[1], code, resp, body.read)
		}
	}

	// 412: a pending digest; the body is not read, nothing is stored.
	body := &watchedBody{r: bytes.NewReader(content("A3", "a3.loads"))}
	code, resp = upload(t, h, http.MethodPut, "A3", "a3.loads", body, 100)
	if code != http.StatusPreconditionFailed || resp.Stored || resp.Error == "" || body.read || resp.Size != 0 {
		t.Errorf("pending = %d %+v (body read %v), want 412 unread", code, resp, body.read)
	}
	if _, err := os.Stat(filepath.Join(dir, "A3")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a pending upload left %v", err)
	}
	// Even with a huge Content-Length: the digest is the first refusal.
	body = &watchedBody{r: strings.NewReader("x")}
	if code, _ := upload(t, h, http.MethodPut, "B1", "pend.default.loads", body, api.MaxFirmwareFileSize+1); code != http.StatusPreconditionFailed || body.read {
		t.Errorf("pending and too large = %d, want 412", code)
	}

	// 413 by Content-Length: not read, nothing stored.
	body = &watchedBody{r: bytes.NewReader(content("A1", "appsA1.sbn"))}
	code, resp = upload(t, h, http.MethodPut, "A1", "appsA1.sbn", body, api.MaxFirmwareFileSize+1)
	if code != http.StatusRequestEntityTooLarge || resp.Stored || body.read || resp.Error == "" {
		t.Errorf("Content-Length over the limit = %d %+v (body read %v), want 413 unread", code, resp, body.read)
	}

	// 413 by stream: a body without a length that runs over.
	firmware.SetMaxSize(d, 100)
	code, resp = upload(t, h, http.MethodPut, "A1", "appsA1.sbn", bytes.NewReader(content("A1", "appsA1.sbn")), -1)
	if code != http.StatusRequestEntityTooLarge || resp.Stored || resp.Error == "" || resp.SHA256 != "" {
		t.Errorf("a stream over the limit = %d %+v, want 413", code, resp)
	}
	if _, err := os.Stat(filepath.Join(dir, "A1", "appsA1.sbn")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("413 stored something: %v", err)
	}
	if left := incoming(t, dir); len(left) != 0 {
		t.Errorf("413 left %v in %s", left, firmware.IncomingDir)
	}
	firmware.SetMaxSize(d, api.MaxFirmwareFileSize)

	// The manifest size is advisory: b1.sbn's is one byte off, and the
	// right digest is stored and served all the same.
	code, resp = put(t, h, "B1", "b1.sbn", content("B1", "b1.sbn"))
	if code != http.StatusCreated {
		t.Errorf("b1.sbn (manifest size wrong) = %d %+v, want 201", code, resp)
	}
	if _, _, err := openBytes(t, d, "b1.sbn"); err != nil {
		t.Errorf("b1.sbn (manifest size wrong): Open: %v", err)
	}

	// 422: another file's bytes; nothing stored, no temp file left.
	wrong := content("A2", "appsA1.sbn")
	code, resp = put(t, h, "A1", "appsA1.sbn", wrong)
	if code != http.StatusUnprocessableEntity || resp.Stored || resp.SHA256 != digest(wrong) || resp.Size != int64(len(wrong)) || resp.Error == "" {
		t.Errorf("wrong bytes = %d %+v, want 422", code, resp)
	}
	if _, err := os.Stat(filepath.Join(dir, "A1", "appsA1.sbn")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("422 stored something: %v", err)
	}
	if left := incoming(t, dir); len(left) != 0 {
		t.Errorf("422 left %v in %s", left, firmware.IncomingDir)
	}
	// 422 over a stored file leaves it, and says it is still stored.
	code, resp = put(t, h, "A1", "a1.loads", wrong)
	if code != http.StatusUnprocessableEntity || !resp.Stored {
		t.Errorf("wrong bytes over a stored file = %d %+v, want 422, still stored", code, resp)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "A1", "a1.loads")); !bytes.Equal(got, good) {
		t.Error("a 422 changed the stored file")
	}

	// A Shared file is uploaded per load: A2's copy is stored under A2 and
	// is still not what a device gets (A1 is the default load).
	if code, _ := put(t, h, "A2", "term.default.loads", content("A2", "term.default.loads")); code != http.StatusCreated {
		t.Errorf("A2's Shared copy = %d", code)
	}
	wantNotFound(t, d, "term.default.loads", "Shared, only A2's copy uploaded")
	if code, _ := put(t, h, "A1", "term.default.loads", content("A1", "term.default.loads")); code != http.StatusCreated {
		t.Errorf("A1's Shared copy = %d", code)
	}
	if f, b, err := openBytes(t, d, "term.default.loads"); err != nil || f.Load != "A1" || !bytes.Equal(b, content("A1", "term.default.loads")) {
		t.Errorf("Shared after the default load's upload: %v", err)
	}

	// Only PUT.
	if code, _ := upload(t, h, http.MethodGet, "A1", "a1.loads", http.NoBody, 0); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
}

// errReader fails after some bytes, like a client that went away.
type errReader struct{ n int }

func (r *errReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := min(len(p), r.n)
	for i := range n {
		p[i] = 'x'
	}
	r.n -= n
	return n, nil
}

// TestUploadInterrupted: a body that fails, or a request whose context
// ends, stores nothing and leaves no temp file.
func TestUploadInterrupted(t *testing.T) {
	d, dir, _ := newDisk(t)
	h := firmware.NewUploadHandler(d, logr.Discard())
	code, resp := upload(t, h, http.MethodPut, "A1", "appsA1.sbn", &errReader{n: 50}, -1)
	if code != http.StatusBadRequest || resp.Stored || resp.Error == "" {
		t.Errorf("failing body = %d %+v, want 400", code, resp)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, resp = d.Put(ctx, "A1", "appsA1.sbn", bytes.NewReader(content("A1", "appsA1.sbn")))
	if code != http.StatusBadRequest || resp.Stored {
		t.Errorf("cancelled = %d %+v, want 400", code, resp)
	}
	if _, err := os.Stat(filepath.Join(dir, "A1", "appsA1.sbn")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an interrupted upload stored something: %v", err)
	}
	if left := incoming(t, dir); len(left) != 0 {
		t.Errorf("left %v in %s", left, firmware.IncomingDir)
	}
}

// TestUploadModes: the modes are set, not left to the umask.
func TestUploadModes(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	d, dir, _ := newDisk(t)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if code, resp := d.Put(context.Background(), "A1", "a1.loads", bytes.NewReader(content("A1", "a1.loads"))); code != http.StatusCreated {
		t.Fatalf("Put = %d %+v", code, resp)
	}
	for p, want := range map[string]fs.FileMode{
		dir:                                      fs.ModeDir | firmware.DirMode,
		filepath.Join(dir, firmware.IncomingDir): fs.ModeDir | 0o700,
		filepath.Join(dir, "A1"):                 fs.ModeDir | firmware.DirMode,
		filepath.Join(dir, "A1", "a1.loads"):     firmware.FileMode,
	} {
		if got := mode(t, p); got != want {
			t.Errorf("%s: mode %v, want %v", p, got, want)
		}
	}
}

// TestUploadNeedsTheParent: Put creates the store directory, not the
// volume it lives on.
func TestUploadNeedsTheParent(t *testing.T) {
	d := firmware.NewDisk(filepath.Join(t.TempDir(), "no-volume", "firmware"), testManifest(t), logr.Discard())
	code, resp := d.Put(context.Background(), "A1", "a1.loads", bytes.NewReader(content("A1", "a1.loads")))
	if code != http.StatusInternalServerError || resp.Stored || resp.Error == "" {
		t.Errorf("Put without the volume = %d %+v, want 500", code, resp)
	}
}

// TestUploadRemovesLeftovers: what an earlier process left in the upload
// area (a crash mid-upload) is removed by the first upload.
func TestUploadRemovesLeftovers(t *testing.T) {
	d, dir, _ := newDisk(t)
	inc := filepath.Join(dir, firmware.IncomingDir)
	if err := os.MkdirAll(filepath.Join(inc, "stale-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inc, "stale"), []byte("half an upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := d.Put(context.Background(), "A1", "a1.loads", bytes.NewReader(content("A1", "a1.loads"))); code != http.StatusCreated {
		t.Fatalf("Put = %d", code)
	}
	if left := incoming(t, dir); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// TestConcurrentUploads: many uploads of one file at once, right and wrong,
// end with the right file stored and nothing left behind.
func TestConcurrentUploads(t *testing.T) {
	d, dir, _ := newDisk(t)
	good, wrong := content("A1", "appsA1.sbn"), content("A2", "appsA2.sbn")
	var wg sync.WaitGroup
	codes := make([]int, 16)
	for i := range codes {
		wg.Go(func() {
			b := good
			if i%4 == 3 {
				b = wrong
			}
			codes[i], _ = d.Put(context.Background(), "A1", "appsA1.sbn", bytes.NewReader(b))
		})
	}
	wg.Wait()
	created := 0
	for i, c := range codes {
		switch {
		case i%4 == 3 && c != http.StatusUnprocessableEntity:
			t.Errorf("wrong upload %d = %d, want 422", i, c)
		case i%4 != 3 && c != http.StatusCreated && c != http.StatusOK:
			t.Errorf("good upload %d = %d, want 201 or 200", i, c)
		case c == http.StatusCreated:
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d uploads were 201, want exactly one (the rest found it stored)", created)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "A1", "appsA1.sbn")); !bytes.Equal(got, good) {
		t.Error("the stored file is not the right one")
	}
	if left := incoming(t, dir); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}
