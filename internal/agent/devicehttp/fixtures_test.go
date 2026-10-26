package devicehttp_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/devicehttp"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/provision"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Bundle versions, device versions and tokens. The tokens are obviously
// not secrets; they are long and distinctive so a test can search every
// output for them.
var (
	v1, v2, v3, v4, v5 = ver('1'), ver('2'), ver('3'), ver('4'), ver('5')
	denV1, denV2       = ver('a'), ver('b')
	labV1              = ver('c')
)

const (
	tokDen  = "not-a-secret-den-token-0123456789ABCDEF"
	tokDen2 = "not-a-secret-den-token-rotated-0123456789"
	tokLab  = "not-a-secret-lab-token-0123456789ABCDEF"
	tokNone = "not-a-secret-unknown-token-0123456789AB"

	denFile = "provisioning/softphone-den.xml"
	labFile = "provisioning/softphone-lab.xml"
)

var allTokens = []string{tokDen, tokDen2, tokLab}

func ver(c byte) string { return strings.Repeat(string(c), 64) }

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func softphone(name, version string) compile.BundleDevice {
	id := drivers.DeviceID(drivers.KindSoftphone, name)
	tok := compile.ProvisioningTokenRef(id)
	file := compile.TreeProvisioning + "/" + id + ".xml"
	return compile.BundleDevice{
		Kind: drivers.KindSoftphone, Name: name, Number: "150", Version: version,
		ConfigFile: file, Files: []string{file}, ProvisioningToken: &tok,
	}
}

func grant(device, token, file, bundle, version string) provision.Grant {
	return provision.Grant{Device: device, Token: token, File: file, BundleVersion: bundle, Version: version}
}

// writeVersion writes one applied version the way apply does: every tree,
// per-tree modes, the index as bundle.json.
func writeVersion(t *testing.T, live, version string, files map[string]string, devices map[string]compile.BundleDevice) {
	t.Helper()
	dir := filepath.Join(live, layout.VersionDirPrefix+version)
	must(t, os.MkdirAll(dir, layout.VersionDirMode))
	for _, tree := range compile.Trees {
		dm, _, _ := layout.TreeModes(tree)
		must(t, os.Mkdir(filepath.Join(dir, tree), dm))
	}
	for p, content := range files {
		tree, err := compile.TreeOf(p)
		must(t, err)
		dm, fm, _ := layout.TreeModes(tree)
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), dm))
		must(t, os.WriteFile(filepath.Join(dir, p), []byte(content), fm))
	}
	if devices != nil {
		idx, err := json.Marshal(compile.Index{Version: version, Devices: devices})
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dir, layout.IndexFile), idx, layout.IndexFileMode))
	}
}

// setCurrent swaps the current link the way apply does.
func setCurrent(t *testing.T, live, version string) {
	t.Helper()
	tmp := filepath.Join(live, ".current.tmp")
	_ = os.Remove(tmp)
	must(t, os.Symlink(layout.VersionDirPrefix+version, tmp))
	must(t, os.Rename(tmp, filepath.Join(live, compile.CurrentLink)))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// grants is a settable provision.Source.
type grants struct {
	mu      sync.Mutex
	version string
	gs      []provision.Grant
}

func (g *grants) Grants() (string, []provision.Grant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.version, slices.Clone(g.gs)
}

func (g *grants) set(version string, gs ...provision.Grant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.version, g.gs = version, gs
}

// recorder keeps every event, failing the test on an invalid one (the
// observation store would drop it).
type recorder struct {
	t      *testing.T
	mu     sync.Mutex
	events []sensor.FetchEvent
	added  chan struct{}
}

func newRecorder(t *testing.T) *recorder {
	return &recorder{t: t, added: make(chan struct{}, 1024)}
}

func (r *recorder) RecordFetch(e sensor.FetchEvent) {
	if err := e.Validate(); err != nil {
		r.t.Errorf("invalid fetch event %+v: %v", e, err)
	}
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	r.added <- struct{}{}
}

// next waits for the next event (a server may record after the client has
// its answer).
func (r *recorder) next(t *testing.T) sensor.FetchEvent {
	t.Helper()
	select {
	case <-r.added:
	case <-time.After(5 * time.Second):
		t.Fatal("no fetch event was recorded")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.events[0]
	r.events = r.events[1:]
	return e
}

// none checks nothing more was recorded, by recording a sentinel request
// and checking it is the next event.
func (r *recorder) none(t *testing.T, f *fixture) {
	t.Helper()
	f.get(t, "/sentinel.xml")
	if e := r.next(t); e.File != "sentinel.xml" {
		t.Errorf("an unexpected event was recorded: %+v", e)
		for len(r.added) > 0 {
			r.next(t)
		}
	}
}

func (r *recorder) all() []sensor.FetchEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// logs is everything the server logged, at every verbosity.
type logs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type fixture struct {
	live, state string
	grants      *grants
	// settled is what spends are pruned against: the serving grants unless
	// a test sets another source (nil: never prune) before start.
	settled provision.Source
	rec     *recorder
	logs    *logs
	now     time.Time
	s       *devicehttp.Server
	ts      *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		live:   filepath.Join(root, "config"),
		state:  filepath.Join(root, "persistent", "state"),
		grants: &grants{},
		logs:   &logs{},
		now:    t0,
	}
	f.settled = f.grants
	must(t, os.MkdirAll(f.live, 0o755))
	must(t, os.MkdirAll(filepath.Dir(f.state), 0o755))
	f.start(t)
	return f
}

// start (re)creates the server on the fixture's directories, as an agent
// restart does.
func (f *fixture) start(t *testing.T) {
	t.Helper()
	if f.ts != nil {
		f.ts.Close()
	}
	f.rec = newRecorder(t)
	s, err := devicehttp.New(devicehttp.Config{
		LiveDir: f.live, StateDir: f.state, Grants: f.grants, Settled: f.settled, Recorder: f.rec,
		Now: func() time.Time { return f.now },
		Log: funcr.New(func(prefix, args string) {
			f.logs.mu.Lock()
			defer f.logs.mu.Unlock()
			f.logs.b.WriteString(prefix + " " + args + "\n")
		}, funcr.Options{Verbosity: 10}),
	})
	must(t, err)
	f.s = s
	f.ts = httptest.NewServer(s)
	t.Cleanup(f.ts.Close)
}

type response struct {
	code   int
	header http.Header
	body   string
}

func (f *fixture) do(t *testing.T, method, p string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, f.ts.URL+p, nil)
	must(t, err)
	resp, err := f.ts.Client().Do(req)
	must(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	must(t, err)
	return response{resp.StatusCode, resp.Header, string(b)}
}

func (f *fixture) get(t *testing.T, p string) response {
	t.Helper()
	return f.do(t, http.MethodGet, p)
}

// raw sends one request line as given (no client-side cleaning) and
// returns the raw response without its Date header.
func (f *fixture) raw(t *testing.T, method, target string) string {
	t.Helper()
	conn, err := net.Dial("tcp", f.ts.Listener.Addr().String())
	must(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: pbx\r\nConnection: close\r\n\r\n", method, target)
	must(t, err)
	var out strings.Builder
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if !strings.HasPrefix(line, "Date: ") {
			out.WriteString(line)
		}
		if err != nil {
			break
		}
	}
	return out.String()
}

// stateFile is the spent-token file's content ("" when there is none).
func (f *fixture) stateFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, layout.SpentTokensFile))
	if os.IsNotExist(err) {
		return ""
	}
	must(t, err)
	return string(b)
}

func digest(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

// v1Files is a bundle with a directory, an XML app, a binary, a phone's
// SEP file, Asterisk config and two softphone documents.
func v1Files(tag string) map[string]string {
	return map[string]string{
		"http/directory.xml":           "<CiscoIPPhoneDirectory>" + tag + "</CiscoIPPhoneDirectory>",
		"http/apps/menu.xml":           "<CiscoIPPhoneMenu>" + tag + "</CiscoIPPhoneMenu>",
		"http/logo.bin":                "\x00\x01binary" + tag,
		"http/directory/contacts.vcf":  "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:" + tag + "\r\nEND:VCARD\r\n",
		"tftp/SEPD0C78915E8C8.cnf.xml": "<device>tftp secret " + tag + "</device>",
		"asterisk/pjsip.conf":          "[asterisk secret " + tag + "]",
		denFile:                        "<config>den document " + tag + "</config>",
		labFile:                        "<config>lab document " + tag + "</config>",
	}
}

func v1Devices(den, lab string) map[string]compile.BundleDevice {
	return map[string]compile.BundleDevice{"softphone-den": softphone("den", den), "softphone-lab": softphone("lab", lab)}
}
