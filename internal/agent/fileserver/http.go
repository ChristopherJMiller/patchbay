package fileserver

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// The fixed bodies HTTP :6970 answers with: every not-found is the same.
const (
	notFoundBody         = "404 page not found\n"
	serverErrorBody      = "500 internal server error\n"
	methodNotAllowedBody = "405 method not allowed\n"
)

// httpServer is HTTP :6970, which 9.x Cisco firmware may use instead of
// TFTP: the same names and the same files.
func (s *server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Minute, // a 4.6 MB firmware file on a slow phone
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    8 << 10,
	}
}

// ServeHTTP answers GET and HEAD with exactly what TFTP would send for the
// same name, and reports each GET once. A HEAD is answered and not
// reported; any other method is 405 and not reported (as TFTP does not
// report a write request).
//
// Range and conditional requests are answered with the whole file: a
// served fetch is always the whole file, and a phone can never keep an
// older version's file on a 304 (two versions written within one second
// carry the same Last-Modified).
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		plain(w, http.StatusMethodNotAllowed, methodNotAllowedBody)
		return
	}
	source := ""
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		source = ap.Addr().Unmap().WithZone("").String()
	}
	head := r.Method == http.MethodHead

	f, ev := s.open(r.URL.Path)
	if f == nil {
		if ev.Result == api.FetchNotFound {
			plain(w, http.StatusNotFound, notFoundBody)
		} else {
			plain(w, http.StatusInternalServerError, serverErrorBody)
		}
		if !head {
			s.report(ev, api.TransportHTTP6970, source)
		}
		return
	}
	defer f.Close()

	for _, h := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		r.Header.Del(h)
	}
	w.Header().Set("Content-Type", contentType(ev.File))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rw := &recordingWriter{ResponseWriter: w}
	// A zero modification time: no Last-Modified, no If-Modified-Since.
	http.ServeContent(rw, r, "", time.Time{}, f)
	if head {
		return
	}
	if err := http.NewResponseController(rw).Flush(); err != nil && rw.err == nil {
		rw.err = err
	}
	switch {
	case rw.err != nil:
		ev = failed(ev, "http", rw.err)
	case rw.status != http.StatusOK:
		ev = failed(ev, "http", fmt.Errorf("status %d", rw.status))
	default:
		ev.Result = api.FetchServed
	}
	s.report(ev, api.TransportHTTP6970, source)
}

// contentType is what a phone is told it gets: XML for .xml, else bytes.
func contentType(name string) string {
	if strings.HasSuffix(name, ".xml") {
		return "text/xml; charset=utf-8"
	}
	return "application/octet-stream"
}

func plain(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// recordingWriter keeps the status and the first error of a response, so
// the handler can tell a whole transfer from a failed one.
type recordingWriter struct {
	http.ResponseWriter
	status int
	err    error
}

func (w *recordingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

// ReadFrom keeps the server's sendfile path for firmware.
func (w *recordingWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	var n int64
	var err error
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(struct{ io.Writer }{w.ResponseWriter}, src)
	}
	if err != nil && !errors.Is(err, io.EOF) && w.err == nil {
		w.err = err
	}
	return n, err
}

// Unwrap lets http.ResponseController reach the server's writer.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
