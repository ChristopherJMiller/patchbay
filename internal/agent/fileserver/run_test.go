package fileserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// freePorts finds a free UDP and a free TCP port on 127.0.0.1. Run takes
// literal addresses and does not say which port 0 became, so the tests
// pick them first.
func freePorts(t *testing.T) (udp, tcp int) {
	t.Helper()
	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	ln, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port, ln.Addr().(*net.TCPAddr).Port
}

// udpBound reports whether something holds the UDP port.
func udpBound(t *testing.T, port int) bool {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", "127.0.0.1:"+strconv.Itoa(port))
	if err == nil {
		pc.Close()
		return false
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("binding UDP %d: %v", port, err)
	}
	return true
}

// tcpListening reports whether something accepts connections on the port.
func tcpListening(t *testing.T, port int) bool {
	t.Helper()
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func runConfig(t *testing.T, store *fakeStore, socket string) Config {
	t.Helper()
	udp, tcp := freePorts(t)
	l := newLive(t)
	l.version(vA, map[string][]byte{sepName: generated("sep", 100)})
	l.swap(vA)
	return Config{
		LiveDir: l.dir, Firmware: store,
		TFTPListen:   "127.0.0.1:" + strconv.Itoa(udp),
		HTTPListen:   "127.0.0.1:" + strconv.Itoa(tcp),
		ReportSocket: socket,
		UID:          65533, GID: 65533,
		Now: func() time.Time { return pinned }, Log: logr.Discard(),
	}
}

func port(addr string) int {
	_, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return n
}

// TestRunDropsBeforeServing: when Drop runs, both ports are bound, and
// nothing else has happened: no file read, no store asked, no event, no
// connection to the agent. Afterwards it serves, and its own Reporter
// delivers the events.
func TestRunDropsBeforeServing(t *testing.T) {
	store := newFakeStore(t, nil)
	path := socketPath(t)
	agent := listenAgent(t, path)
	cfg := runConfig(t, store, path)
	udp, tcp := port(cfg.TFTPListen), port(cfg.HTTPListen)

	dropped := make(chan [2]int, 1)
	cfg.Drop = func(uid, gid int) error {
		if !udpBound(t, udp) {
			t.Error("Drop: the TFTP port is not bound yet")
		}
		if !tcpListening(t, tcp) {
			t.Error("Drop: the HTTP port is not listening yet")
		}
		if n := store.used(); n != 0 {
			t.Errorf("Drop: the firmware store was used %d times before the drop", n)
		}
		time.Sleep(50 * time.Millisecond) // a Reporter started early would connect now
		if n := agent.connections(); n != 0 {
			t.Errorf("Drop: %d connections to the agent before the drop", n)
		}
		dropped <- [2]int{uid, gid}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()

	select {
	case ids := <-dropped:
		if ids != [2]int{65533, 65533} {
			t.Errorf("Drop(%v), want 65533, 65533", ids)
		}
	case err := <-done:
		t.Fatalf("Run returned before dropping: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Drop was never called")
	}

	server := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udp}
	if _, err := tftpGet(t, server, sepName, 0); err != nil {
		t.Errorf("serving after the drop: %v", err)
	}
	if ev := agent.next(); ev.File != sepName || ev.Result != api.FetchServed || ev.BundleVersion != vA {
		t.Errorf("the agent got %+v", ev)
	}
	resp, _ := httpGet(t, cfg.HTTPListen, "/"+sepName)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTP after the drop: %d", resp.StatusCode)
	}
	if ev := agent.next(); ev.Transport != api.TransportHTTP6970 {
		t.Errorf("the agent got %+v", ev)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil once cancelled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return once cancelled")
	}
	if udpBound(t, udp) || tcpListening(t, tcp) {
		t.Error("the ports are still held after Run returned")
	}
}

// TestRunExitsWhenDropFails: a failed drop closes both ports and is
// returned; nothing is served or reported.
func TestRunExitsWhenDropFails(t *testing.T) {
	store := newFakeStore(t, nil)
	path := socketPath(t)
	agent := listenAgent(t, path)
	cfg := runConfig(t, store, path)
	boom := errors.New("CapEff is 0000000000000400")
	cfg.Drop = func(int, int) error { return boom }
	err := Run(context.Background(), cfg)
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the drop's error", err)
	}
	if udpBound(t, port(cfg.TFTPListen)) || tcpListening(t, port(cfg.HTTPListen)) {
		t.Error("a port is still held after a failed drop")
	}
	if store.used() != 0 {
		t.Error("the store was used")
	}
	time.Sleep(50 * time.Millisecond)
	if agent.connections() != 0 {
		t.Error("the agent was connected to")
	}
}

// TestRunRefusesBadConfig: a config that would serve as root, resolve a
// host name or lose its events is refused before anything is bound.
func TestRunRefusesBadConfig(t *testing.T) {
	good := runConfig(t, newFakeStore(t, nil), socketPath(t))
	good.Drop = func(int, int) error {
		t.Error("Drop called for a refused config")
		return nil
	}
	for what, mutate := range map[string]func(*Config){
		"uid 0":              func(c *Config) { c.UID = 0 },
		"gid 0":              func(c *Config) { c.GID = 0 },
		"no live dir":        func(c *Config) { c.LiveDir = "" },
		"no firmware store":  func(c *Config) { c.Firmware = nil },
		"no way to report":   func(c *Config) { c.ReportSocket = "" },
		"a TFTP host name":   func(c *Config) { c.TFTPListen = "localhost:69" },
		"an HTTP host name":  func(c *Config) { c.HTTPListen = "pbx.example:6970" },
		"no port":            func(c *Config) { c.TFTPListen = "127.0.0.1" },
		"an empty TFTP addr": func(c *Config) { c.TFTPListen = "" },
	} {
		cfg := good
		mutate(&cfg)
		err := Run(context.Background(), cfg)
		if err == nil {
			t.Errorf("%s: Run = nil, want a refusal", what)
			continue
		}
		if !strings.HasPrefix(err.Error(), "fileserver: ") {
			t.Errorf("%s: %v", what, err)
		}
	}
	if udpBound(t, port(good.TFTPListen)) || tcpListening(t, port(good.HTTPListen)) {
		t.Error("a refused config bound a port")
	}
}

// TestRunFailsOnABusyPort: a port it cannot bind is an error before any
// drop.
func TestRunFailsOnABusyPort(t *testing.T) {
	cfg := runConfig(t, newFakeStore(t, nil), socketPath(t))
	cfg.Drop = func(int, int) error {
		t.Error("Drop called although a bind failed")
		return nil
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", cfg.HTTPListen)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "HTTP") {
		t.Errorf("Run with the HTTP port taken = %v", err)
	}
	if udpBound(t, port(cfg.TFTPListen)) {
		t.Error("the TFTP port is still held")
	}
	pc, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", cfg.TFTPListen)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	ln.Close()
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "TFTP") {
		t.Errorf("Run with the TFTP port taken = %v", err)
	}
}
