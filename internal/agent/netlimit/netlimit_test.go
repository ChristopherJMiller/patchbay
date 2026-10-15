package netlimit_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/netlimit"
)

// serve accepts on l until it is closed, keeping every connection.
func serve(t *testing.T, l *netlimit.Listener) (conns func() []net.Conn) {
	t.Helper()
	var mu sync.Mutex
	var accepted []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
	return func() []net.Conn {
		mu.Lock()
		defer mu.Unlock()
		return append([]net.Conn(nil), accepted...)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// dial connects from the given loopback address.
func dial(t *testing.T, addr net.Addr, from string) net.Conn {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
	c, err := d.DialContext(context.Background(), "tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// closedByServer reports whether the server closed c.
func closedByServer(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	return errors.Is(err, io.EOF) || (err != nil && !os.IsTimeout(err))
}

func waitOpen(t *testing.T, l *netlimit.Listener, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.Open() != n {
		if time.Now().After(deadline) {
			t.Fatalf("open = %d, want %d", l.Open(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestPerSourceCap(t *testing.T) {
	var logged strings.Builder
	var mu sync.Mutex
	log := funcr.New(func(_, args string) { mu.Lock(); logged.WriteString(args + "\n"); mu.Unlock() }, funcr.Options{})
	l := netlimit.New(listen(t), 10, 2, log)
	conns := serve(t, l)

	a1, a2 := dial(t, l.Addr(), "127.0.0.1"), dial(t, l.Addr(), "127.0.0.1")
	waitOpen(t, l, 2)
	a3 := dial(t, l.Addr(), "127.0.0.1")
	if !closedByServer(a3) {
		t.Error("a third connection from one source was kept")
	}
	// Another source still gets in.
	b1 := dial(t, l.Addr(), "127.0.0.2")
	waitOpen(t, l, 3)
	if closedByServer(b1) || closedByServer(a1) || closedByServer(a2) {
		t.Error("a connection within the caps was closed")
	}
	if l.Refused() != 1 {
		t.Errorf("refused = %d, want 1", l.Refused())
	}
	// Closing one frees its source's place; closing twice frees it once.
	for _, c := range conns() {
		if c.RemoteAddr().(*net.TCPAddr).IP.String() == "127.0.0.1" {
			_ = c.Close()
			_ = c.Close()
			break
		}
	}
	waitOpen(t, l, 2)
	dial(t, l.Addr(), "127.0.0.1")
	waitOpen(t, l, 3)
	a5 := dial(t, l.Addr(), "127.0.0.1")
	if !closedByServer(a5) {
		t.Error("the cap was not restored")
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(logged.String(), "connections over the cap"); n != 1 {
		t.Errorf("refusals logged %d times, want once a minute:\n%s", n, logged.String())
	}
}

func TestTotalCap(t *testing.T) {
	l := netlimit.New(listen(t), 3, 3, funcr.New(func(_, _ string) {}, funcr.Options{}))
	serve(t, l)
	for _, from := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"} {
		dial(t, l.Addr(), from)
	}
	waitOpen(t, l, 3)
	if c := dial(t, l.Addr(), "127.0.0.4"); !closedByServer(c) {
		t.Error("a connection over the total was kept")
	}
	if l.Refused() != 1 || l.Open() != 3 {
		t.Errorf("refused %d, open %d", l.Refused(), l.Open())
	}
}
