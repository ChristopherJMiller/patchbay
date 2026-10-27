// Package devicehttp is the agent's device HTTP server on :8088 (design §5;
// docs/plans/release-a.md W2.4). It serves two things and nothing else:
//
//   - the live bundle's http/ tree, http/<p> at /<p> (the 79x5 directory and
//     XML services), to anyone on the voice VLAN or the LAN, from the
//     version current names when the request arrives;
//   - each softphone's provisioning document at
//     api.ProvisioningPathPrefix + <one-time token>, once (design §8.4), from
//     the grant's own version (provision.Grant), so token and document
//     always belong together.
//
// The one-time token is the only secret that reaches this server, and it
// never leaves it: the token is compared as a SHA-256 digest with every
// grant's in constant time; a request that matches no grant, a spent one,
// a token with a "/" and the bare prefix all get the one 404 every unknown
// path gets, byte for byte; fetch events and log lines name the document's
// bundle path (provisioning/<device ID>.xml) or sensor.ProvisioningUnknownFile,
// never the request path, so not even a token sent to the wrong path is
// recorded (each path element is checked against the grants first).
//
// A token is spent by its first successful match, and the spend is written
// to the state directory before the document is sent (state.go): an agent
// restart, before or after the controller saw the fetch, re-records the
// served fetch and keeps the token spent until the live bundle no longer
// grants it and no rollback can bring back one that does (Config.Settled).
// HEAD never spends a token, and is answered with the one 404
// under the provisioning prefix, so it cannot tell a good token from a bad
// one either.
package devicehttp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/netlimit"
	"github.com/ChristopherJMiller/patchbay/internal/agent/provision"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// The server's limits.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 10 * time.Second
	WriteTimeout      = 30 * time.Second
	IdleTimeout       = 15 * time.Second // a phone asks once per connection
	MaxHeaderBytes    = 8 << 10

	// MaxConns and MaxConnsPerSource cap the connections open at once
	// (package netlimit): anyone on the voice VLAN or the LAN can connect,
	// and the agent's memory limit must hold them all.
	MaxConns          = 256
	MaxConnsPerSource = 32

	// maxDocumentSize bounds a file read into memory: a whole bundle fits
	// in one ConfigMap (1 MiB).
	maxDocumentSize = 1 << 20

	// pruneEvery is how often Run checks whether the grants moved on.
	pruneEvery = 15 * time.Second

	// quotedNameLimit cuts a recorded name the bundle cannot hold.
	quotedNameLimit = 200
)

// The answers.
const (
	notFoundBody     = "404 page not found\n"
	unavailableBody  = "503 service unavailable\n"
	notAllowedBody   = "405 method not allowed\n"
	xmlContentType   = "text/xml; charset=utf-8"
	vCardContentType = "text/vcard; charset=utf-8" // RFC 6350 §10.1
	binContentType   = "application/octet-stream"
	textContentType  = "text/plain; charset=utf-8"
)

// compareHook, when set (tests only), is called with each grant's index as
// its token is compared.
var compareHook atomic.Pointer[func(int)]

// Config configures a Server.
type Config struct {
	// LiveDir is the live directory (compile.LiveDir).
	LiveDir string
	// Grants are the live bundle's provisioning grants (apply). Required.
	Grants provision.Source
	// Settled are the grants spent tokens are pruned against: the live
	// version's once no failure can roll it back
	// (provision.SourceFunc(apply.Applier.SettledGrants)). Pruning against
	// Grants would forget the spends of an older version's tokens while a
	// rollback can still make that version, and its grants, live again.
	// Nil: spends are never pruned.
	Settled provision.Source
	// Recorder receives one event per GET (the observation store). Nil
	// discards them.
	Recorder sensor.FetchRecorder
	// StateDir is where spent tokens are kept (layout.StateDir). It is
	// created, mode layout.StateDirMode, if missing.
	StateDir string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Credentials is the push account the phone asks about at /authenticate
	// (apply.Applier.PushCredentials); ok false (or nil) answers every ask
	// UN-AUTHORIZED.
	Credentials func() (user, password string, ok bool)
	// Log receives one line per request (V(1)) and every failure.
	Log logr.Logger
}

// Server is device HTTP.
type Server struct {
	liveDir     string
	stateDir    string
	grants      provision.Source
	settled     provision.Source
	rec         sensor.FetchRecorder
	now         func() time.Time
	log         logr.Logger
	credentials func() (string, string, bool)
	pruneEvery  time.Duration

	// mu serialises spending and pruning, and guards what follows.
	mu       sync.Mutex
	spends   []spendRecord
	spent    map[tokenDigest]bool
	prunedAt string // the grants' bundle version of the last complete prune
}

// New prepares the state directory, loads the spent tokens and re-records
// each spend's served event, so the controller sees every served fetch
// even when the agent restarted before it polled. A state file it cannot
// fully read is an error.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.LiveDir == "":
		return nil, errors.New("devicehttp: no live directory")
	case cfg.StateDir == "":
		return nil, errors.New("devicehttp: no state directory")
	case cfg.Grants == nil:
		return nil, errors.New("devicehttp: no grants")
	}
	s := &Server{
		liveDir: cfg.LiveDir, stateDir: cfg.StateDir, grants: cfg.Grants, settled: cfg.Settled, rec: cfg.Recorder,
		now: cfg.Now, log: cfg.Log, credentials: cfg.Credentials, pruneEvery: pruneEvery, spent: map[tokenDigest]bool{},
	}
	if s.rec == nil {
		s.rec = sensor.FetchRecorderFunc(func(sensor.FetchEvent) {})
	}
	if s.now == nil {
		s.now = time.Now
	}
	if err := ensureStateDir(cfg.StateDir); err != nil {
		return nil, err
	}
	recs, err := loadState(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	s.setSpends(recs)
	for _, r := range recs {
		s.rec.RecordFetch(r.Event)
	}
	if len(recs) > 0 {
		s.log.Info("re-recorded spent provisioning tokens", "count", len(recs))
	}
	return s, nil
}

// setSpends replaces the spends; s.mu held (or not yet shared).
func (s *Server) setSpends(recs []spendRecord) {
	s.spends = recs
	s.spent = make(map[tokenDigest]bool, len(recs))
	for _, r := range recs {
		if d, ok := parseDigest(r.TokenSHA256); ok {
			s.spent[d] = true
		}
	}
}

// Run serves device HTTP on addr ("<pbx address>:8088") until ctx is done,
// and prunes spent tokens the live bundle no longer grants.
func (s *Server) Run(ctx context.Context, addr string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("devicehttp: listen on %s: %w", addr, err)
	}
	return s.serve(ctx, ln)
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	srv := s.httpServer()
	capped := netlimit.New(ln, MaxConns, MaxConnsPerSource, s.log)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(capped) }()
	s.log.Info("device HTTP listening", "addr", ln.Addr().String())
	tick := time.NewTicker(s.pruneEvery)
	defer tick.Stop()
	for {
		select {
		case err := <-errc:
			return fmt.Errorf("devicehttp: %w", err)
		case <-tick.C:
			s.prune()
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdown); err != nil {
				return fmt.Errorf("devicehttp: shutdown: %w", err)
			}
			if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("devicehttp: %w", err)
			}
			return nil
		}
	}
}

func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		ErrorLog:          log.New(logWriter{s.log}, "", 0),
	}
}

// logWriter sends net/http's own messages (connection errors) to the log.
type logWriter struct{ log logr.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.log.V(1).Info("device HTTP server", "message", strings.TrimSpace(string(p)))
	return len(p), nil
}

// ServeHTTP answers one request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == compile.AuthenticatePath && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		s.authenticate(w, r)
		return
	}
	head := r.Method == http.MethodHead
	if r.Method != http.MethodGet && !head {
		w.Header().Set("Allow", "GET, HEAD")
		writeText(w, http.StatusMethodNotAllowed, notAllowedBody, false)
		return
	}
	src := sourceOf(r.RemoteAddr)
	if token, ok := strings.CutPrefix(r.URL.Path, api.ProvisioningPathPrefix); ok {
		if head {
			// HEAD never spends a token, and never tells whether one is good.
			writeText(w, http.StatusNotFound, notFoundBody, true)
			return
		}
		s.provision(w, token, src)
		return
	}
	s.serveTree(w, r.URL.Path, head, src)
}

// provision serves a softphone's document to the holder of its token,
// once.
func (s *Server) provision(w http.ResponseWriter, token, src string) {
	s.prune()
	version, grants := s.grants.Grants()
	if !layout.ValidVersion(version) {
		version = ""
	}
	i := -1
	if token != "" && !strings.Contains(token, "/") {
		i = match(token, grants)
	}
	if i < 0 {
		writeText(w, http.StatusNotFound, notFoundBody, false)
		s.record(sensor.FetchEvent{File: sensor.ProvisioningUnknownFile, Result: api.FetchNotFound, BundleVersion: version, Source: src})
		return
	}
	g := grants[i]
	if err := checkGrant(g); err != nil {
		s.log.Error(err, "a provisioning grant is malformed; nothing served", "grant", g)
		writeText(w, http.StatusServiceUnavailable, unavailableBody, false)
		s.record(sensor.FetchEvent{
			File: sensor.ProvisioningUnknownFile, Result: api.FetchError, BundleVersion: version, Source: src,
			Error: "the grant is malformed; nothing was served",
		})
		return
	}
	e := sensor.FetchEvent{File: g.File, BundleVersion: g.BundleVersion, BundlePath: g.File, Device: g.Device, Source: src}
	digest := digestOf(token)

	s.mu.Lock()
	if s.spent[digest] {
		s.mu.Unlock()
		writeText(w, http.StatusNotFound, notFoundBody, false)
		e.Result = api.FetchNotFound
		s.record(e)
		return
	}
	doc, err := readGrant(s.liveDir, g)
	if err != nil {
		s.mu.Unlock()
		if isNotFound(err) {
			writeText(w, http.StatusNotFound, notFoundBody, false)
			e.Result = api.FetchNotFound
		} else {
			s.log.Error(err, "reading a provisioning document; nothing served", "file", g.File, "device", g.Device)
			writeText(w, http.StatusServiceUnavailable, unavailableBody, false)
			e.Result, e.Error = api.FetchError, "the document could not be read; nothing was served"
		}
		s.record(e)
		return
	}
	served := e
	served.Transport, served.Result, served.Version, served.At = api.TransportHTTPDevice, api.FetchServed, g.Version, s.now().UTC()
	if err := s.spend(digest, served); err != nil {
		s.mu.Unlock()
		s.log.Error(err, "persisting a spent provisioning token; nothing served", "file", g.File, "device", g.Device)
		writeText(w, http.StatusServiceUnavailable, unavailableBody, false)
		e.Result, e.Error = api.FetchError, "the spend could not be persisted; nothing was served"
		s.record(e)
		return
	}
	s.mu.Unlock()

	// The token is spent whether or not the client reads the document, and
	// the served fetch is recorded either way: only a served fetch makes
	// the controller rotate the token (a fresh QR) instead of leaving the
	// softphone Issued on a dead URL.
	if err := writeDocument(w, xmlContentType, doc, false, true); err != nil {
		s.log.V(1).Info("the client did not take its provisioning document; the token is spent", "file", g.File, "device", g.Device, "error", err.Error())
	}
	s.record(served)
}

// spend persists a spend, then adds it to memory; s.mu held.
func (s *Server) spend(digest tokenDigest, served sensor.FetchEvent) error {
	recs := append(append(make([]spendRecord, 0, len(s.spends)+1), s.spends...),
		spendRecord{TokenSHA256: hexDigest(digest), Event: served})
	if err := writeState(s.stateDir, recs); err != nil {
		return err
	}
	s.spends = recs
	s.spent[digest] = true
	return nil
}

// match returns the index of the first grant whose token is token, or -1.
// It compares the SHA-256 digests (so not even the length leaks) of every
// grant, in constant time, with no early exit; a grant with no token
// never matches.
func match(token string, grants []provision.Grant) int {
	want := digestOf(token)
	hook := compareHook.Load()
	found, matched := -1, 0
	for i := range grants {
		have := digestOf(grants[i].Token)
		eq := subtle.ConstantTimeCompare(want[:], have[:]) & subtle.ConstantTimeLessOrEq(1, len(grants[i].Token))
		if hook != nil {
			(*hook)(i)
		}
		found = subtle.ConstantTimeSelect(eq&^matched, i, found)
		matched |= eq
	}
	return found
}

// checkGrant refuses a grant apply should never produce: one whose file is
// not a provisioning document, or whose served fetch could not be recorded
// (the fetch event's own rules: versions, device ID, file name).
func checkGrant(g provision.Grant) error {
	if tree, err := compile.TreeOf(g.File); err != nil || tree != compile.TreeProvisioning {
		return errors.New("devicehttp: the grant's file is not a provisioning document")
	}
	if g.Device == "" || g.Version == "" {
		return errors.New("devicehttp: the grant has no device or no version")
	}
	return checkSpendEvent(sensor.FetchEvent{
		Transport: api.TransportHTTPDevice, File: g.File, Result: api.FetchServed, At: time.Unix(1, 0),
		Source: "192.0.2.1", BundleVersion: g.BundleVersion, BundlePath: g.File, Device: g.Device, Version: g.Version,
	})
}

// readGrant reads a grant's document from its own version's directory.
func readGrant(liveDir string, g provision.Grant) ([]byte, error) {
	root, err := layout.OpenTree(liveDir, g.BundleVersion, compile.TreeProvisioning)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readRegular(root, strings.TrimPrefix(g.File, compile.TreeProvisioning+"/"))
}

var (
	errNotRegular = errors.New("not a regular file")
	errTooLarge   = fmt.Errorf("larger than %d bytes", maxDocumentSize)
)

// readRegular reads a regular file inside root into memory. It never
// blocks on a FIFO and never follows a link out of root.
func readRegular(root *os.Root, name string) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	if fi.Size() > maxDocumentSize {
		return nil, errTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDocumentSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDocumentSize {
		return nil, errTooLarge
	}
	return b, nil
}

// isNotFound reports whether a read failed because the file is not there
// (or is not a file), as opposed to failing.
func isNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, errNotRegular)
}

// serveTree serves http/<name> from the current version.
func (s *Server) serveTree(w http.ResponseWriter, urlPath string, head bool, src string) {
	name := strings.TrimPrefix(urlPath, "/")
	bundlePath := compile.TreeHTTP + "/" + name
	tree, err := compile.TreeOf(bundlePath)
	valid := err == nil && tree == compile.TreeHTTP && len(name) <= sensor.MaxFileLength
	e := sensor.FetchEvent{Source: src}
	finish := func(code int, result api.FetchResult, errText string) {
		switch code {
		case http.StatusNotFound:
			writeText(w, code, notFoundBody, head)
		case http.StatusServiceUnavailable:
			writeText(w, code, unavailableBody, head)
		}
		if head {
			return // a HEAD is not a fetch
		}
		e.File = s.recordedName(name, valid)
		if e.File == sensor.ProvisioningUnknownFile {
			e.BundlePath = "" // it would carry the token too
		}
		e.Result, e.Error = result, errText
		s.record(e)
	}
	if !valid {
		finish(http.StatusNotFound, api.FetchNotFound, "")
		return
	}
	version, err := layout.Current(s.liveDir)
	if errors.Is(err, layout.ErrNoCurrent) {
		finish(http.StatusNotFound, api.FetchNotFound, "")
		return
	}
	if err != nil {
		s.log.Error(err, "resolving the live version")
		finish(http.StatusServiceUnavailable, api.FetchError, "no live version could be resolved")
		return
	}
	e.BundleVersion, e.BundlePath = version, bundlePath
	root, err := layout.OpenTree(s.liveDir, version, compile.TreeHTTP)
	var body []byte
	if err == nil {
		body, err = readRegular(root, name)
		_ = root.Close()
	}
	switch {
	case err != nil && isNotFound(err):
		finish(http.StatusNotFound, api.FetchNotFound, "")
		return
	case err != nil:
		s.log.Error(err, "reading a device HTTP file", "file", bundlePath)
		finish(http.StatusServiceUnavailable, api.FetchError, "the file could not be read")
		return
	}
	ctype := binContentType
	switch path.Ext(name) {
	case ".xml":
		ctype = xmlContentType
	case ".vcf":
		ctype = vCardContentType
	}
	if err := writeDocument(w, ctype, body, head, false); err != nil {
		finish(http.StatusOK, api.FetchError, "the response could not be sent")
		return
	}
	finish(http.StatusOK, api.FetchServed, "")
}

// recordedName is what a fetch of the tree records as its File: the name
// when the bundle could hold it, else its quoted form, and
// sensor.ProvisioningUnknownFile when any path element is a granted token
// (a token sent to the wrong path is still never recorded; the caller then
// drops the bundle path too).
func (s *Server) recordedName(name string, valid bool) string {
	if s.mentionsToken(name) {
		return sensor.ProvisioningUnknownFile
	}
	if valid {
		return name
	}
	q := strconv.QuoteToASCII(name)
	if len(q) > quotedNameLimit {
		q = q[:quotedNameLimit]
	}
	return q
}

// mentionsToken reports whether an element of name (split at "/" and
// "\") is a granted token, comparing digests in constant time.
func (s *Server) mentionsToken(name string) bool {
	_, grants := s.grants.Grants()
	hit := 0
	for _, el := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		d := digestOf(el)
		for _, g := range grants {
			gd := digestOf(g.Token)
			hit |= subtle.ConstantTimeCompare(d[:], gd[:]) & subtle.ConstantTimeLessOrEq(1, len(g.Token))
		}
	}
	return hit == 1
}

// record completes and reports one fetch, and logs it (never a token: File
// is a bundle path, a name the bundle could hold, a quoted name that holds
// no token, or sensor.ProvisioningUnknownFile).
func (s *Server) record(e sensor.FetchEvent) {
	e.Transport = api.TransportHTTPDevice
	if e.At.IsZero() {
		e.At = s.now().UTC()
	}
	s.log.V(1).Info("device HTTP", "file", e.File, "result", e.Result, "source", e.Source, "device", e.Device)
	if e.Source == "" {
		return // no canonical source address: the store would drop it
	}
	s.rec.RecordFetch(e)
}

// sourceOf is a request's canonical source address, or "".
func sourceOf(remote string) string {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return ""
	}
	return ap.Addr().Unmap().WithZone("").String()
}

// writeText answers with a short text body. Every 404 goes through here
// with the same arguments, so they are identical byte for byte.
func writeText(w http.ResponseWriter, code int, body string, head bool) {
	h := w.Header()
	h.Set("Content-Type", textContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if code == http.StatusServiceUnavailable {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(code)
	if !head {
		_, _ = io.WriteString(w, body)
	}
}

// writeDocument answers 200 with body.
func writeDocument(w http.ResponseWriter, contentType string, body []byte, head, noStore bool) error {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if noStore {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	if head {
		return nil
	}
	_, err := w.Write(body) //nolint:gosec // G705: a bundle file, served as text/xml or octet-stream with nosniff; never HTML.
	return err
}

// pruneOnChange drops the spends of tokens the settled grants no longer
// hold, once per settled version. It prunes only on a complete grant
// list: every softphone the version's index says holds a token
// (layout.IndexFile) must have its grant, so a list apply has not filled
// yet never makes a spent token usable again. Without the index it waits.
func (s *Server) pruneOnChange(version string, grants []provision.Grant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version == "" || version == s.prunedAt {
		return
	}
	want, err := grantedDevices(s.liveDir, version)
	if err != nil {
		s.log.V(1).Info("not pruning spent tokens yet", "bundleVersion", version, "reason", err.Error())
		return
	}
	granted := map[tokenDigest]bool{}
	have := map[string]bool{}
	for _, g := range grants {
		if g.Token != "" && g.BundleVersion == version {
			granted[digestOf(g.Token)] = true
			have[g.Device] = true
		}
	}
	for _, id := range want {
		if !have[id] {
			s.log.V(1).Info("not pruning spent tokens yet: the grants are incomplete", "bundleVersion", version, "missing", id)
			return
		}
	}
	keep := make([]spendRecord, 0, len(s.spends))
	var dropped []string
	for _, r := range s.spends {
		if d, ok := parseDigest(r.TokenSHA256); ok && granted[d] {
			keep = append(keep, r)
		} else {
			dropped = append(dropped, r.Event.Device)
		}
	}
	if len(dropped) > 0 {
		if err := writeState(s.stateDir, keep); err != nil {
			s.log.Error(err, "pruning spent provisioning tokens")
			return
		}
		s.setSpends(keep)
		s.log.Info("pruned spent provisioning tokens the bundle no longer grants", "bundleVersion", version, "devices", dropped)
	}
	s.prunedAt = version
}

// prune runs pruneOnChange on the settled grants as they are now; without
// a settled source it never prunes.
func (s *Server) prune() {
	if s.settled == nil {
		return
	}
	version, grants := s.settled.Grants()
	s.pruneOnChange(version, grants)
}

// grantedDevices lists the devices a version's index gives a provisioning
// token: its rendered softphones.
func grantedDevices(liveDir, version string) ([]string, error) {
	dir, err := layout.VersionDir(version)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(liveDir, dir, layout.IndexFile)) //nolint:gosec // G304: the agent's own live directory; dir is a checked version.
	if err != nil {
		return nil, err
	}
	var idx compile.Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("the index: %w", err)
	}
	if idx.Version != version {
		return nil, fmt.Errorf("the index is version %q", idx.Version)
	}
	var ids []string
	for id, d := range idx.Devices {
		if d.ProvisioningToken != nil && d.Version != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
