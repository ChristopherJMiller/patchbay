package fileserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/pin/tftp/v3"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// Two bundle versions.
var (
	vA = strings.Repeat("a", 64)
	vB = strings.Repeat("b", 64)
)

// secret is what the asterisk/ tree of every test version holds: it must
// never be served.
const secret = "password=not-a-real-secret-but-never-served"

// live is a live config directory as apply leaves it.
type live struct {
	t   *testing.T
	dir string
}

func newLive(t *testing.T) *live {
	t.Helper()
	return &live{t: t, dir: t.TempDir()}
}

// version writes v<version> with the given tftp/ files, plus files in the
// other trees that the file server must never serve.
func (l *live) version(version string, tftpFiles map[string][]byte) {
	l.t.Helper()
	root := filepath.Join(l.dir, "v"+version)
	write := func(rel string, b []byte, dirMode, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), dirMode); err != nil {
			l.t.Fatal(err)
		}
		if err := os.WriteFile(p, b, mode); err != nil {
			l.t.Fatal(err)
		}
	}
	for name, b := range tftpFiles {
		write(filepath.Join(compile.TreeTFTP, name), b, 0o755, 0o644)
	}
	write("asterisk/pjsip.conf", []byte(secret), 0o750, 0o640)
	write("http/directory.xml", []byte("<directory/>"), 0o750, 0o640)
	write("provisioning/softphone-den.xml", []byte(secret), 0o700, 0o600)
	write("bundle.json", []byte("{}"), 0o755, 0o640)
}

// swap makes version current, as apply does: a new symlink renamed over
// the old.
func (l *live) swap(version string) {
	l.t.Helper()
	tmp := filepath.Join(l.dir, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink("v"+version, tmp); err != nil {
		l.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(l.dir, compile.CurrentLink)); err != nil {
		l.t.Fatal(err)
	}
}

// tftpPath is a file in a version's tftp/ tree.
func (l *live) tftpPath(version, name string) string {
	return filepath.Join(l.dir, "v"+version, compile.TreeTFTP, name)
}

// fakeStore is a firmware.Store holding some files by name.
type fakeStore struct {
	t     *testing.T
	dir   string
	mu    sync.Mutex
	files map[string][]byte
	errs  map[string]error
	opens []string
	stats int
}

func newFakeStore(t *testing.T, files map[string][]byte) *fakeStore {
	t.Helper()
	return &fakeStore{t: t, dir: t.TempDir(), files: files, errs: map[string]error{}}
}

func (f *fakeStore) Open(name string) (*firmware.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, name)
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	b, ok := f.files[name]
	if !ok {
		return nil, firmware.ErrNotFound
	}
	p := filepath.Join(f.dir, fmt.Sprintf("%d", len(f.opens)))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return nil, err
	}
	fd, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &firmware.File{File: fd, Load: "LOAD", Name: name, Size: int64(len(b))}, nil
}

func (f *fakeStore) Status() api.FirmwareStore {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats++
	return api.FirmwareStore{Files: []api.FirmwareFileStatus{}}
}

func (f *fakeStore) used() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opens) + f.stats
}

// recorder collects fetch events and fails the test on an invalid one.
type recorder struct {
	t      *testing.T
	mu     sync.Mutex
	events []sensor.FetchEvent
	added  chan struct{}
}

func newRecorder(t *testing.T) *recorder {
	return &recorder{t: t, added: make(chan struct{}, 1)}
}

func (r *recorder) RecordFetch(e sensor.FetchEvent) {
	if err := e.Validate(); err != nil {
		r.t.Errorf("an invalid event %+v: %v", e, err)
	}
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	select {
	case r.added <- struct{}{}:
	default:
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// wait returns the events once there are n, failing after a while.
func (r *recorder) wait(n int) []sensor.FetchEvent {
	r.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		r.mu.Lock()
		if len(r.events) >= n {
			out := slices.Clone(r.events)
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		select {
		case <-r.added:
		case <-deadline:
			r.t.Fatalf("%d events, want %d: %+v", r.count(), n, r.events)
		}
	}
}

// take returns the events so far and forgets them.
func (r *recorder) take() []sensor.FetchEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// next waits for exactly one more event (after those taken).
func (r *recorder) next() sensor.FetchEvent {
	r.t.Helper()
	ev := r.wait(1)
	r.take()
	if len(ev) != 1 {
		r.t.Fatalf("%d events, want one: %+v", len(ev), ev)
	}
	return ev[0]
}

// quiet checks no event arrives for a while.
func (r *recorder) quiet(d time.Duration) {
	r.t.Helper()
	time.Sleep(d)
	if got := r.take(); len(got) != 0 {
		r.t.Errorf("unexpected events: %+v", got)
	}
}

// env is a running file server on loopback, without the privilege drop
// (serve, as Run calls it).
type env struct {
	live  *live
	store *fakeStore
	rec   *recorder
	tftp  *net.UDPAddr
	http  string // host:port
}

func fastTFTP() tftpOptions {
	return tftpOptions{timeout: 200 * time.Millisecond, retries: 2, backoff: func(int) time.Duration { return 0 }}
}

func startEnv(t *testing.T, store firmware.Store, o tftpOptions) *env {
	t.Helper()
	e := &env{live: newLive(t), rec: newRecorder(t)}
	if fs, ok := store.(*fakeStore); ok {
		e.store = fs
	}
	if store == nil {
		e.store = newFakeStore(t, nil)
		store = e.store
	}
	e.tftp, e.http = serveOn(t, "127.0.0.1", Config{
		LiveDir: e.live.dir, Firmware: store, Recorder: e.rec,
		Now: func() time.Time { return pinned }, Log: logr.Discard(),
	}, o)
	return e
}

// serveOn runs serve on fresh sockets bound to host, until the test ends.
func serveOn(t *testing.T, host string, cfg Config, o tftpOptions) (*net.UDPAddr, string) {
	t.Helper()
	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(context.Background(), "udp4", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := lc.Listen(context.Background(), "tcp4", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, pc.(*net.UDPConn), ln, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve = %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("serve did not return")
		}
	})
	return pc.LocalAddr().(*net.UDPAddr), ln.Addr().String()
}

// tftpGet fetches name with pin/tftp's client, as a phone would.
func tftpGet(t *testing.T, server *net.UDPAddr, name string, blksize int) ([]byte, error) {
	t.Helper()
	c, err := tftp.NewClient(server.String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetTimeout(2 * time.Second)
	c.SetRetries(2)
	if blksize > 0 {
		c.SetBlockSize(blksize)
	}
	wt, err := c.Receive(name, "octet")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if _, err := wt.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Raw TFTP, for what a well-behaved client never sends.

const (
	opRRQ   = 1
	opWRQ   = 2
	opDATA  = 3
	opACK   = 4
	opERROR = 5
	opOACK  = 6
)

func rrq(name string, opts ...string) []byte {
	b := []byte{0, opRRQ}
	b = append(b, name...)
	b = append(b, 0)
	b = append(b, "octet"...)
	b = append(b, 0)
	for _, o := range opts {
		b = append(b, o...)
		b = append(b, 0)
	}
	return b
}

func ackPacket(block uint16) []byte {
	return binary.BigEndian.AppendUint16([]byte{0, opACK}, block)
}

func udpClient(t *testing.T) *net.UDPConn {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc.(*net.UDPConn)
}

func send(t *testing.T, c *net.UDPConn, to *net.UDPAddr, b []byte) {
	t.Helper()
	if _, err := c.WriteToUDP(b, to); err != nil {
		t.Fatal(err)
	}
}

// recv reads one packet, or returns nil after d.
func recv(t *testing.T, c *net.UDPConn, d time.Duration) ([]byte, *net.UDPAddr) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 70000)
	n, from, err := c.ReadFromUDP(buf)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return nil, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n], from
}

func opcode(p []byte) int {
	if len(p) < 2 {
		return -1
	}
	return int(binary.BigEndian.Uint16(p))
}

// rawFetch reads a whole file the plain way (512-byte blocks), acking each
// block, and calls between after the first block.
func rawFetch(t *testing.T, server *net.UDPAddr, name string, between func()) []byte {
	t.Helper()
	c := udpClient(t)
	send(t, c, server, rrq(name))
	var out []byte
	for block := uint16(1); ; block++ {
		p, from := recv(t, c, 5*time.Second)
		if p == nil || opcode(p) != opDATA || binary.BigEndian.Uint16(p[2:]) != block {
			t.Fatalf("block %d: got %v", block, p)
		}
		out = append(out, p[4:]...)
		if block == 1 && between != nil {
			between()
		}
		send(t, c, from, ackPacket(block))
		if len(p)-4 < 512 {
			return out
		}
	}
}

// httpRaw sends one request line as given (no client cleaning of the
// path) and returns the response.
// httpResult is what the tests read of a response, its body already read.
type httpResult struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
}

func httpRaw(t *testing.T, addr, method, target string, header ...string) (httpResult, []byte) {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := method + " " + target + " HTTP/1.1\r\nHost: pbx\r\nConnection: close\r\n"
	for _, h := range header {
		req += h + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{StatusCode: resp.StatusCode, Header: resp.Header, ContentLength: resp.ContentLength}, body
}

func httpGet(t *testing.T, addr, target string) (httpResult, []byte) {
	t.Helper()
	return httpRaw(t, addr, http.MethodGet, target)
}

// generated is n bytes of a pattern named by seed: never anything real.
func generated(seed string, n int) []byte {
	line := []byte("patchbay fileserver test " + seed + "\n")
	return bytes.Repeat(line, n/len(line)+1)[:n]
}
