package firmware_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
)

// plant writes a file into the store as an operator (or a past upload)
// would have: <dir>/<load>/<name>.
func plant(t *testing.T, dir, load, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, load, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// newDisk is a store over a fresh directory, counting the files it hashes.
func newDisk(t *testing.T) (d *firmware.Disk, dir string, hashed func() []string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "firmware")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	d = firmware.NewDisk(dir, testManifest(t), logr.Discard())
	var mu sync.Mutex
	var got []string
	firmware.SetHashHook(d, func(load, name string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, load+"/"+name)
	})
	return d, dir, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(got)
		got = nil
		return out
	}
}

// openBytes opens name and reads it whole, or returns Open's error.
func openBytes(t *testing.T, d *firmware.Disk, name string) (*firmware.File, []byte, error) {
	t.Helper()
	f, err := d.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return f, b, nil
}

func wantNotFound(t *testing.T, d *firmware.Disk, name, why string) {
	t.Helper()
	if f, err := d.Open(name); !errors.Is(err, firmware.ErrNotFound) {
		if f != nil {
			f.Close()
		}
		t.Errorf("%s: Open(%q) error = %v, want ErrNotFound", why, name, err)
	}
}

// TestOpenServesOnlyVerified: Open serves a stored file only when its bytes
// have the manifest's digest, and only from the load the name resolves to.
func TestOpenServesOnlyVerified(t *testing.T) {
	d, dir, _ := newDisk(t)

	// A file planted with the right digest is served, from its start.
	plant(t, dir, "A1", "a1.loads", content("A1", "a1.loads"))
	f, b, err := openBytes(t, d, "a1.loads")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(content("A1", "a1.loads")) {
		t.Error("Open served other bytes")
	}
	if f.Load != "A1" || f.Name != "a1.loads" || f.Size != int64(len(b)) || f.ModTime.IsZero() {
		t.Errorf("File = %s/%s size %d mtime %v", f.Load, f.Name, f.Size, f.ModTime)
	}

	// Not stored, or in no manifest.
	wantNotFound(t, d, "appsA1.sbn", "not stored")
	wantNotFound(t, d, "unknown.sbn", "in no manifest")
	plant(t, dir, "A1", "unknown.sbn", []byte("x"))
	wantNotFound(t, d, "unknown.sbn", "stored but in no manifest")

	// Pending: never, whatever is on disk.
	plant(t, dir, "A3", "a3.loads", content("A3", "a3.loads"))
	wantNotFound(t, d, "a3.loads", "pending")
	plant(t, dir, "B1", "pend.default.loads", content("B1", "pend.default.loads"))
	wantNotFound(t, d, "pend.default.loads", "pending and Shared")

	// A wrong digest.
	plant(t, dir, "A1", "appsA1.sbn", content("A2", "appsA1.sbn"))
	wantNotFound(t, d, "appsA1.sbn", "wrong digest")
	// Another load's right file under this load's name.
	plant(t, dir, "A2", "a2.loads", content("A1", "a1.loads"))
	wantNotFound(t, d, "a2.loads", "another file's bytes")

	// A rewrite in place after it was verified: the size and mtime change,
	// so Open hashes it again and refuses it.
	p := plant(t, dir, "A2", "appsA2.sbn", content("A2", "appsA2.sbn"))
	if _, _, err := openBytes(t, d, "appsA2.sbn"); err != nil {
		t.Fatal(err)
	}
	bad := content("A2", "appsA2.sbn")
	bad[0] ^= 1
	if err := os.WriteFile(p, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, d, "appsA2.sbn", "rewritten in place")

	// A symlink to a verified copy, inside the store or out of it, never
	// counts; nor does a directory.
	good := plant(t, dir, "A1", "copy", content("A1", "appsA1.sbn"))
	if err := os.Remove(filepath.Join(dir, "A1", "appsA1.sbn")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(good, filepath.Join(dir, "A1", "appsA1.sbn")); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, d, "appsA1.sbn", "an absolute symlink")
	if err := os.Remove(filepath.Join(dir, "A1", "appsA1.sbn")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("copy", filepath.Join(dir, "A1", "appsA1.sbn")); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, d, "appsA1.sbn", "a relative symlink")
	outside := filepath.Join(t.TempDir(), "b1.sbn")
	if err := os.WriteFile(outside, content("B1", "b1.sbn"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "B1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "B1")); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, d, "b1.sbn", "a load directory that is a symlink out of the store")
	if err := os.Remove(filepath.Join(dir, "A2", "a2.loads")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "A2", "a2.loads"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, d, "a2.loads", "a directory")

	// A Shared name: from the default load only. A2's copy is right for
	// A2, but A1 is the default load.
	plant(t, dir, "A2", "term.default.loads", content("A2", "term.default.loads"))
	wantNotFound(t, d, "term.default.loads", "Shared, only a non-default load's copy stored")
	plant(t, dir, "A1", "term.default.loads", content("A2", "term.default.loads"))
	wantNotFound(t, d, "term.default.loads", "Shared, the default load holds another load's copy")
	plant(t, dir, "A1", "term.default.loads", content("A1", "term.default.loads"))
	later = later.Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "A1", "term.default.loads"), later, later); err != nil {
		t.Fatal(err)
	}
	f, b, err = openBytes(t, d, "term.default.loads")
	if err != nil {
		t.Fatalf("Shared from the default load: %v", err)
	}
	if f.Load != "A1" || string(b) != string(content("A1", "term.default.loads")) {
		t.Errorf("Shared name served from %s", f.Load)
	}
	// Shared names no single default load carries: never.
	plant(t, dir, "A1", "both.default.loads", content("A1", "both.default.loads"))
	plant(t, dir, "A2", "orphan.default.loads", content("A2", "orphan.default.loads"))
	wantNotFound(t, d, "both.default.loads", "Shared by two default loads")
	wantNotFound(t, d, "orphan.default.loads", "Shared by no default load")

	// Names are names, never paths.
	for _, name := range []string{"A1/a1.loads", "../firmware/A1/a1.loads", "/a1.loads", "A1.LOADS", ".incoming"} {
		wantNotFound(t, d, name, "a path or a variant")
	}
}

// TestOpenNeverBlocksOnAFIFO: a FIFO where a file should be is not a file,
// and is refused without being opened: opening it would wait for a writer
// forever, and with it a TFTP transfer or the observations request.
func TestOpenNeverBlocksOnAFIFO(t *testing.T) {
	d, dir, _ := newDisk(t)
	if err := os.MkdirAll(filepath.Join(dir, "A1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "A1", "a1.loads"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := d.Open("a1.loads")
		if f != nil {
			f.Close()
		}
		d.Status()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, firmware.ErrNotFound) {
			t.Errorf("Open on a FIFO = %v, want ErrNotFound", err)
		}
	case <-time.After(5 * time.Second):
		// Unblock the opener so the test binary can exit.
		w, err := os.OpenFile(filepath.Join(dir, "A1", "a1.loads"), os.O_WRONLY, 0)
		if err == nil {
			w.Close()
		}
		t.Fatal("Open blocked on a FIFO")
	}
}

// TestOpenOnMissingStore: a store whose directory does not exist is empty,
// and notices the directory once it appears.
func TestOpenOnMissingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "firmware")
	d := firmware.NewDisk(dir, testManifest(t), logr.Discard())
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewDisk touched the directory: %v", err)
	}
	wantNotFound(t, d, "a1.loads", "no store directory")
	st := d.Status()
	if len(st.Files) == 0 {
		t.Fatal("Status of a missing store lists nothing; want every manifest file, not present")
	}
	for _, f := range st.Files {
		if f.Present {
			t.Errorf("%s/%s present in a missing store", f.Load, f.Name)
		}
	}

	plant(t, dir, "A1", "a1.loads", content("A1", "a1.loads"))
	if _, _, err := openBytes(t, d, "a1.loads"); err != nil {
		t.Errorf("after the directory appeared: %v", err)
	}
	if !present(d.Status(), "A1", "a1.loads") {
		t.Error("Status: a1.loads not present after the directory appeared")
	}
}

func present(st api.FirmwareStore, load, name string) bool {
	for _, f := range st.Files {
		if f.Load == load && f.Name == name {
			return f.Present
		}
	}
	return false
}

// TestStatus: every manifest file, in registry, load and file order,
// present only when stored and verified.
func TestStatus(t *testing.T) {
	d, dir, _ := newDisk(t)
	plant(t, dir, "A1", "a1.loads", content("A1", "a1.loads"))
	plant(t, dir, "A1", "appsA1.sbn", []byte("wrong"))
	plant(t, dir, "A2", "term.default.loads", content("A2", "term.default.loads"))
	plant(t, dir, "A3", "a3.loads", content("A3", "a3.loads")) // pending
	plant(t, dir, "B1", "both.default.loads", content("B1", "both.default.loads"))

	var got []string
	for _, f := range d.Status().Files {
		s := f.Load + "/" + f.Name
		if f.Present {
			s += "+"
		}
		got = append(got, s)
	}
	want := []string{
		"A1/a1.loads+", "A1/appsA1.sbn", "A1/term.default.loads", "A1/both.default.loads",
		"A2/a2.loads", "A2/appsA2.sbn", "A2/term.default.loads+", "A2/orphan.default.loads",
		"A3/a3.loads", "A3/orphan.default.loads",
		"B1/b1.sbn", "B1/both.default.loads+", "B1/pend.default.loads",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Status =\n %v\nwant\n %v", got, want)
	}
}

// TestDigestCache: a file is hashed once per change of its size or
// modification time, whether Open or Status asks.
func TestDigestCache(t *testing.T) {
	d, dir, hashed := newDisk(t)
	p := plant(t, dir, "A1", "a1.loads", content("A1", "a1.loads"))
	plant(t, dir, "A1", "appsA1.sbn", content("A1", "appsA1.sbn"))
	plant(t, dir, "A3", "a3.loads", content("A3", "a3.loads")) // pending: never hashed

	d.Status()
	if got := hashed(); !slices.Equal(got, []string{"A1/a1.loads", "A1/appsA1.sbn"}) {
		t.Errorf("first Status hashed %v, want the two stored files", got)
	}
	d.Status()
	if got := hashed(); len(got) != 0 {
		t.Errorf("second Status hashed %v, want nothing", got)
	}
	for range 3 {
		if _, _, err := openBytes(t, d, "a1.loads"); err != nil {
			t.Fatal(err)
		}
	}
	if got := hashed(); len(got) != 0 {
		t.Errorf("Open after Status hashed %v, want nothing", got)
	}

	// A changed mtime hashes again, once.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openBytes(t, d, "a1.loads"); err != nil {
		t.Fatal(err)
	}
	d.Status()
	if got := hashed(); !slices.Equal(got, []string{"A1/a1.loads"}) {
		t.Errorf("after a new mtime hashed %v, want a1.loads once", got)
	}

	// A changed size too; and a wrong digest is cached as wrong.
	if err := os.WriteFile(p, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, d, "a1.loads", "rewritten")
	wantNotFound(t, d, "a1.loads", "rewritten, again")
	if present(d.Status(), "A1", "a1.loads") {
		t.Error("Status: a rewritten file is present")
	}
	if got := hashed(); !slices.Equal(got, []string{"A1/a1.loads"}) {
		t.Errorf("after a new size hashed %v, want a1.loads once", got)
	}

	// Replaced by another inode with the same size and mtime: hashed again.
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "A1", "tmp")
	if err := os.WriteFile(tmp, []byte("shoRt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	d.Status()
	if got := hashed(); !slices.Equal(got, []string{"A1/a1.loads"}) {
		t.Errorf("after a new inode hashed %v, want a1.loads once", got)
	}
}

// TestOversizedFileIsNotHashed: nothing over the upload limit was ever
// uploaded, so a planted one is not present and is never read.
func TestOversizedFileIsNotHashed(t *testing.T) {
	d, dir, hashed := newDisk(t)
	p := filepath.Join(dir, "A1", "appsA1.sbn")
	plant(t, dir, "A1", "appsA1.sbn", nil)
	if err := os.Truncate(p, api.MaxFirmwareFileSize+1); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, d, "appsA1.sbn", "over the size limit")
	if present(d.Status(), "A1", "appsA1.sbn") {
		t.Error("an oversized file is present")
	}
	if got := hashed(); len(got) != 0 {
		t.Errorf("hashed %v, want nothing", got)
	}
}

// TestUnservedSharedNamesAreLogged: a Shared name the store will never
// serve is said once, when the store is made.
func TestUnservedSharedNamesAreLogged(t *testing.T) {
	m := testManifest(t)
	if got := firmware.Unserved(m); !slices.Equal(got, []string{"both.default.loads", "orphan.default.loads"}) {
		t.Errorf("unserved = %v", got)
	}
	var logged strings.Builder
	log := funcr.New(func(prefix, args string) { logged.WriteString(prefix + args + "\n") }, funcr.Options{})
	firmware.NewDisk(t.TempDir(), m, log)
	for _, name := range []string{"both.default.loads", "orphan.default.loads"} {
		if strings.Count(logged.String(), name) != 1 {
			t.Errorf("log names %s %d times, want once:\n%s", name, strings.Count(logged.String(), name), logged.String())
		}
	}
}
