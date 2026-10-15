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
// cannot exhaust memory. A source is an IPv4 address, or an IPv6 /64: a
// host on an IPv6 LAN can pick any address in its /64.
//
// The control API also exempts the node's own loopback (ExemptLoopback):
// the kubelet probes it there, and the CLI's port-forward arrives there, so
// LAN hosts that fill the total can neither fail the agent's probes (and
// have the kubelet restart it) nor lock out the operator.
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
	exemptLoopback   bool
	log              logr.Logger
	now              func() time.Time

	mu       sync.Mutex
	open     int // every connection held
	counted  int // those the total counts (all but an exempt loopback's)
	bySource map[netip.Addr]int
	refused  uint64
	loggedAt time.Time
}

// logEvery is how often refusals are logged at most.
const logEvery = time.Minute

// New caps ln at total connections open at once, and perSource from any
// one source. Refusals are logged at most once a minute.
func New(ln net.Listener, total, perSource int, log logr.Logger) *Listener {
	return &Listener{
		Listener: ln, total: total, perSource: perSource, log: log, now: time.Now,
		bySource: map[netip.Addr]int{},
	}
}

// ExemptLoopback takes connections from 127.0.0.1 and ::1 out of the
// total: they neither count toward it nor are refused by it, and they keep
// the per-source cap (as one source). Other 127/8 addresses are ordinary
// sources. It returns l; call it before the first Accept.
func (l *Listener) ExemptLoopback() *Listener {
	l.exemptLoopback = true
	return l
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
		exempt := l.exemptLoopback && src == loopback
		if !l.take(src, exempt) {
			_ = c.Close()
			continue
		}
		return &conn{Conn: c, l: l, src: src, exempt: exempt}, nil
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

func (l *Listener) take(src netip.Addr, exempt bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if (!exempt && l.counted >= l.total) || l.bySource[src] >= l.perSource {
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
	if !exempt {
		l.counted++
	}
	l.bySource[src]++
	return true
}

func (l *Listener) release(src netip.Addr, exempt bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open--
	if !exempt {
		l.counted--
	}
	if l.bySource[src]--; l.bySource[src] <= 0 {
		delete(l.bySource, src)
	}
}

// conn releases its place once, on its first Close.
type conn struct {
	net.Conn
	l      *Listener
	src    netip.Addr
	exempt bool
	once   sync.Once
}

func (c *conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.release(c.src, c.exempt) })
	return err
}

// loopback is the one source 127.0.0.1 and ::1 both count as.
var loopback = netip.IPv6Loopback()

// sourceOf is the source a remote address counts as: its IPv4 address
// (unmapped), its IPv6 /64, loopback for 127.0.0.1 and ::1, and the zero
// Addr (one shared slot) for anything else.
func sourceOf(a net.Addr) netip.Addr {
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}
	}
	ip := ap.Addr().Unmap().WithZone("")
	switch {
	case ip == netip.AddrFrom4([4]byte{127, 0, 0, 1}) || ip == loopback:
		return loopback
	case ip.Is6():
		return netip.PrefixFrom(ip, 64).Masked().Addr()
	default:
		return ip
	}
}
