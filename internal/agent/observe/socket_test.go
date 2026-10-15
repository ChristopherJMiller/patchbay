package observe_test

import (
	"bytes"
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/observe"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

// recorder collects what the socket hands on.
type recorder struct {
	mu     sync.Mutex
	events []sensor.FetchEvent
}

func (r *recorder) RecordFetch(e sensor.FetchEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) files() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.File)
	}
	return out
}

func event(file string) sensor.FetchEvent {
	return sensor.FetchEvent{Transport: api.TransportTFTP, File: file, Result: api.FetchServed, At: t0, Source: "192.168.20.5"}
}

// socketPath is a fresh path for the fetch socket. A unix socket path must
// stay under about 108 bytes, and t.TempDir() embeds the test's name: under
// a deep TMPDIR (CI's nix develop) the bind fails. So use a short directory
// of our own, as the fileserver's tests do.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "obs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "fetches.sock")
}

// serve runs the fetch socket until ctx ends; the channel gets its error.
func serve(ctx context.Context, path string, rec *recorder) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- observe.ServeFetchSocket(ctx, path, rec, logr.Discard()) }()
	return errc
}

// noSocket fails the test, with the server's error if it has stopped.
func noSocket(t *testing.T, errc <-chan error) {
	t.Helper()
	select {
	case err := <-errc:
		t.Fatalf("no socket: ServeFetchSocket returned %v", err)
	default:
		t.Fatal("no socket (ServeFetchSocket still running)")
	}
}

func dial(t *testing.T, path string) net.Conn {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func send(t *testing.T, c net.Conn, e sensor.FetchEvent) {
	t.Helper()
	if err := sensor.WriteFetchEvent(c, e); err != nil {
		t.Fatal(err)
	}
}

func waitFiles(t *testing.T, r *recorder, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(r.files(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("recorded %q, want %q", r.files(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFetchSocket(t *testing.T) {
	path := socketPath(t)
	// A stale socket from a previous agent.
	stale, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("the stale socket is gone already")
	}

	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- observe.ServeFetchSocket(ctx, path, rec, logr.Discard()) }()
	var fi os.FileInfo
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := (&net.Dialer{}).DialContext(context.Background(), "unix", path)
		if err == nil {
			_ = c.Close()
			if fi, err = os.Lstat(path); err == nil && fi.Mode().Perm() == layout.FetchSocketMode {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the socket never came up (%v, %v)", err, fi)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		t.Errorf("%s is %v, not a socket", path, fi.Mode())
	}

	a := dial(t, path)
	send(t, a, event("SEPD0C78915E8C8.cnf.xml"))
	// Only device HTTP may report these: dropped, the connection stays.
	http8088 := event("directory.xml")
	http8088.Transport = api.TransportHTTPDevice
	send(t, a, http8088)
	withDevice := event("provisioning/softphone-x.xml")
	withDevice.Device = "softphone-x"
	send(t, a, withDevice)
	withVersion := event("dialplan.xml")
	withVersion.Device, withVersion.Version = "phone-desk", intaketestVersion
	send(t, a, withVersion)
	send(t, a, event("dialplan.xml"))
	waitFiles(t, rec, "SEPD0C78915E8C8.cnf.xml", "dialplan.xml")

	// A bad line ends its connection only; what came before stands.
	b := dial(t, path)
	send(t, b, event("b1"))
	_, _ = b.Write([]byte("{\"transport\":\"tftp\",\"surprise\":1}\n"))
	waitFiles(t, rec, "SEPD0C78915E8C8.cnf.xml", "dialplan.xml", "b1")
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := b.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Error("the connection with a bad line was not closed")
	}
	send(t, a, event("a-after"))
	// Connections are served concurrently: only one connection's events
	// keep their order, so c dials once a-after is in.
	waitFiles(t, rec, "SEPD0C78915E8C8.cnf.xml", "dialplan.xml", "b1", "a-after")
	c := dial(t, path)
	send(t, c, event("c1"))
	// A line over MaxWireLine also ends its connection.
	_, _ = c.Write(append(bytes.Repeat([]byte("x"), sensor.MaxWireLine+1), '\n'))
	waitFiles(t, rec, "SEPD0C78915E8C8.cnf.xml", "dialplan.xml", "b1", "a-after", "c1")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeFetchSocket = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeFetchSocket did not return")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Error("the socket was not removed")
	}
}

// intaketestVersion is a valid version for an event (any will do).
var intaketestVersion = strings.Repeat("a", 64)

func TestFetchSocketRefusesAFile(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := observe.ServeFetchSocket(context.Background(), path, &recorder{}, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("= %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "not a socket" {
		t.Error("the file was touched")
	}
	if err := observe.ServeFetchSocket(context.Background(), filepath.Join(t.TempDir(), "missing", "x.sock"), &recorder{}, logr.Discard()); err == nil {
		t.Error("a socket in a missing directory: no error")
	}
}

// TestFetchSocketLimitsConnections: connections beyond the limit are
// closed at once; the others keep working.
func TestFetchSocketLimitsConnections(t *testing.T) {
	path := socketPath(t)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := serve(ctx, path, rec)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			noSocket(t, errc)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var conns []net.Conn
	for range 20 {
		conns = append(conns, dial(t, path))
	}
	closed := 0
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil && !os.IsTimeout(err) {
			closed++
		}
	}
	if closed != 12 {
		t.Errorf("%d of 20 connections closed, want 12 over the limit of 8", closed)
	}
}

// TestFetchSocketDropsWhatTheFileServerCannotServe: the file server reads
// only the firmware store and the tftp/ tree, and reports a name that may
// carry a provisioning token as p/ only. An event that says otherwise (a
// compromised file server claiming a softphone's provisioning fetch, or
// one carrying a token) is dropped; the connection stays.
func TestFetchSocketDropsWhatTheFileServerCannotServe(t *testing.T) {
	path := socketPath(t)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := serve(ctx, path, rec)
	var c net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		if c, err = (&net.Dialer{}).DialContext(context.Background(), "unix", path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			noSocket(t, errc)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _ = c.Close() }()

	claim := event("provisioning/softphone-chris-laptop.xml")
	claim.BundleVersion, claim.BundlePath = intaketestVersion, "provisioning/softphone-chris-laptop.xml"
	send(t, c, claim)
	inTFTP := event("provisioning/softphone-chris-laptop.xml")
	inTFTP.BundleVersion, inTFTP.BundlePath = intaketestVersion, "tftp/provisioning/softphone-chris-laptop.xml"
	send(t, c, inTFTP)
	httpTree := event("directory.xml")
	httpTree.BundleVersion, httpTree.BundlePath = intaketestVersion, "http/directory.xml"
	send(t, c, httpTree)
	send(t, c, event("p/not-a-secret-provisioning-token-0123456789"))
	send(t, c, event("x/P/not-a-secret-provisioning-token-0123456789"))

	unknown := event(sensor.ProvisioningUnknownFile)
	unknown.Result = api.FetchNotFound
	send(t, c, unknown)
	sep := event("SEPD0C78915E8C8.cnf.xml")
	sep.BundleVersion, sep.BundlePath = intaketestVersion, "tftp/SEPD0C78915E8C8.cnf.xml"
	send(t, c, sep)
	waitFiles(t, rec, sensor.ProvisioningUnknownFile, "SEPD0C78915E8C8.cnf.xml")
}
