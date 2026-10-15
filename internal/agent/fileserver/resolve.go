package fileserver

import (
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// maxQuotedFile is how much of a quoted invalid name an event carries.
const maxQuotedFile = 200

// maxErrorText is how much of an error an event carries.
const maxErrorText = 200

// server is what both transports share: name resolution, the stores it
// reads, and the fetch log it reports to.
type server struct {
	liveDir  string
	firmware firmware.Store
	rec      sensor.FetchRecorder
	now      func() time.Time
	log      logr.Logger
	slots    chan struct{} // TFTP transfers in flight
}

// checkName is the name a request asks for: one leading "/" stripped. ok
// is false for a name that can only be an attack or garbage: a "\", a NUL
// or any other byte that is not printable ASCII (a control character, a
// space, a bidi override, invalid UTF-8: every bundle path is portable
// ASCII and every manifest name printable ASCII), an empty, "." or ".."
// element, or more than sensor.MaxFileLength bytes. Such a name is
// reported quoted (quoteName), never looked up.
func checkName(raw string) (name string, ok bool) {
	name = strings.TrimPrefix(raw, "/")
	if len(name) > sensor.MaxFileLength || strings.Contains(name, `\`) {
		return name, false
	}
	for i := range len(name) {
		if c := name[i]; c <= ' ' || c > '~' {
			return name, false
		}
	}
	for el := range strings.SplitSeq(name, "/") {
		if el == "" || el == "." || el == ".." {
			return name, false
		}
	}
	return name, true
}

// quoteName is how an invalid name is reported: Go-quoted in ASCII, cut to
// maxQuotedFile bytes, so it is always a valid FetchEvent.File and never
// starts with "/".
func quoteName(name string) string {
	q := strconv.QuoteToASCII(name)
	if len(q) > maxQuotedFile {
		q = q[:maxQuotedFile]
	}
	return q
}

// fileOf is FetchEvent.File for a request: the name, or quoted.
func fileOf(raw string) string {
	name, ok := checkName(raw)
	if !ok {
		return quoteName(name)
	}
	return name
}

// open resolves a request, the same for both transports: the firmware
// store first, then the live tftp/ tree of the version current now. It
// returns the open file and the event to report, filled but for
// Transport, At, Source and (when the file opened) Result. f is nil when
// the event's Result is notFound or error.
func (s *server) open(raw string) (*os.File, sensor.FetchEvent) {
	name, ok := checkName(raw)
	if !ok {
		return nil, sensor.FetchEvent{File: quoteName(name), Result: api.FetchNotFound}
	}
	ev := sensor.FetchEvent{File: name}

	fw, err := s.firmware.Open(name)
	switch {
	case err == nil:
		return fw.File, ev
	case !errors.Is(err, firmware.ErrNotFound):
		return nil, failed(ev, "firmware store", err)
	}

	bundlePath := compile.TreeTFTP + "/" + name
	if _, err := compile.TreeOf(bundlePath); err != nil {
		ev.Result = api.FetchNotFound
		return nil, ev
	}
	version, err := layout.Current(s.liveDir)
	switch {
	case errors.Is(err, layout.ErrNoCurrent):
		ev.Result = api.FetchNotFound
		return nil, ev
	case err != nil:
		return nil, failed(ev, "live config", err)
	}
	ev.BundleVersion, ev.BundlePath = version, bundlePath
	root, err := layout.OpenTree(s.liveDir, version, compile.TreeTFTP)
	if err != nil {
		return nil, missing(ev, err)
	}
	defer root.Close()
	f, err := openRegular(root, name)
	if err != nil {
		return nil, missing(ev, err)
	}
	return f, ev
}

// missing is the event for a name that did not open: notFound, unless the
// failure is the server's own.
func missing(ev sensor.FetchEvent, err error) sensor.FetchEvent {
	if ioFailure(err) {
		return failed(ev, "tftp tree", err)
	}
	ev.Result = api.FetchNotFound
	return ev
}

// failed is an error event: what failed, and the innermost error's text,
// which names no path (a path outside the tree must never reach the fetch
// log).
func failed(ev sensor.FetchEvent, what string, err error) sensor.FetchEvent {
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			break
		}
		err = inner
	}
	ev.Result = api.FetchError
	ev.Error = errorText(what + ": " + err.Error())
	return ev
}

// errorText makes text safe for FetchEvent.Error: printable ASCII only (a
// TFTP client chooses the text of its own error packets), at most
// maxErrorText bytes.
func errorText(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	if len(b) > maxErrorText {
		b = b[:maxErrorText]
	}
	return string(b)
}

// report completes an event and hands it to the fetch log.
func (s *server) report(ev sensor.FetchEvent, t api.Transport, source string) {
	ev.Transport, ev.Source, ev.At = t, source, s.now()
	if err := ev.Validate(); err != nil {
		s.log.Error(err, "fileserver: an invalid fetch event (a bug)", "file", ev.File)
		return
	}
	s.rec.RecordFetch(ev)
}

// canonicalIP is FetchEvent.Source for an address: unmapped, no zone.
func canonicalIP(ip net.IP) string {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return ""
	}
	return a.Unmap().WithZone("").String()
}

// errNotRegular is a name that holds something other than a regular file.
var errNotRegular = errors.New("not a regular file")

// openRegular opens rel in root only if it is a regular file, not a
// symlink or a FIFO (opening a FIFO would wait for a writer): it checks
// with Lstat, then that the descriptor it opened is that same file.
func openRegular(root *os.Root, rel string) (*os.File, error) {
	li, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "open", Path: rel, Err: errNotRegular}
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		_ = f.Close()
		return nil, &fs.PathError{Op: "open", Path: rel, Err: errNotRegular}
	}
	return f, nil
}

// ioFailure tells a failure of the server itself (no permission, no
// descriptors, a failing disk) from a name that holds nothing servable
// (missing, a symlink, a path that escapes the tree, not a directory).
func ioFailure(err error) bool {
	for _, e := range []error{fs.ErrPermission, syscall.EIO, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
