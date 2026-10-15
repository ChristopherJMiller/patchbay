// Package netlimit caps the connections an unauthenticated listener holds:
// in all, and per source address. Device HTTP :8088 and the file server's
// HTTP :6970 answer anyone on the voice VLAN and the LAN, and each idle or
// slow connection costs a goroutine and its buffers (about 21 KB). Without
// a cap, one host holding a few thousand connections open would run the
// agent or the file server out of its memory limit, again and again. TFTP
// has its own cap (fileserver's maxTransfers).
//
// A connection over either cap is closed as soon as it is accepted, so a
// flood from one host cannot keep the others out, and a flood from many
// cannot exhaust memory.
package netlimit

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Listener is a net.Listener with connection caps.
type Listener struct {
	net.Listener
	total, perSource int
	log              logr.Logger
	now              func() time.Time

	mu       sync.Mutex
	open     int
	bySource map[netip.Addr]int
	refused  uint64
	loggedAt time.Time
}

// logEvery is how often refusals are logged at most.
const logEvery = time.Minute

// New caps ln at total connections open at once, and perSource from any
// one source address. Refusals are logged at most once a minute.
func New(ln net.Listener, total, perSource int, log logr.Logger) *Listener {
	return &Listener{
		Listener: ln, total: total, perSource: perSource, log: log, now: time.Now,
		bySource: map[netip.Addr]int{},
	}
}

// Accept returns the next connection within the caps, closing those over
// them.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		src := sourceOf(c.RemoteAddr())
		if !l.take(src) {
			_ = c.Close()
			continue
		}
		return &conn{Conn: c, l: l, src: src}, nil
	}
}

// Refused is how many connections were closed over a cap.
func (l *Listener) Refused() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refused
}

// Open is how many connections are open.
func (l *Listener) Open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open
}

func (l *Listener) take(src netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open >= l.total || l.bySource[src] >= l.perSource {
		l.refused++
		if now := l.now(); now.Sub(l.loggedAt) >= logEvery {
			l.loggedAt = now
			l.log.Info("connections over the cap are refused", "addr", l.Addr().String(),
				"open", l.open, "fromSource", l.bySource[src], "limit", l.total, "limitPerSource", l.perSource,
				"refused", l.refused)
		}
		return false
	}
	l.open++
	l.bySource[src]++
	return true
}

func (l *Listener) release(src netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open--
	if l.bySource[src]--; l.bySource[src] <= 0 {
		delete(l.bySource, src)
	}
}

// conn releases its place once, on its first Close.
type conn struct {
	net.Conn
	l    *Listener
	src  netip.Addr
	once sync.Once
}

func (c *conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.release(c.src) })
	return err
}

// sourceOf is a remote address's IP, unmapped; the zero Addr (one shared
// slot) for anything else.
func sourceOf(a net.Addr) netip.Addr {
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	return netip.Addr{}
}
