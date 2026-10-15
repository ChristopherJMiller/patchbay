package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// registrar is an in-process SIP registrar that challenges every REGISTER
// with digest auth (as Asterisk does) and records what it accepted.
type registrar struct {
	addr     string // 127.0.0.1:port, UDP and TCP
	password string

	mu       sync.Mutex
	contacts []string // Contact values of accepted REGISTERs
	userAgts []string
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func startRegistrar(t *testing.T, user, password string) *registrar {
	t.Helper()
	r := &registrar{addr: "127.0.0.1:" + strconv.Itoa(freePort(t)), password: password}
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("test-registrar"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}
	chal := digest.Challenge{Realm: "patchbay-test", Nonce: "n0nce", Algorithm: "MD5"}
	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		h := req.GetHeader("Authorization")
		if h == nil {
			res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
			res.AppendHeader(sip.NewHeader("WWW-Authenticate", chal.String()))
			_ = tx.Respond(res)
			return
		}
		cred, err := digest.ParseCredentials(h.Value())
		if err != nil || cred.Username != user {
			_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusForbidden, "Forbidden", nil))
			return
		}
		want, err := digest.Digest(&chal, digest.Options{
			Method: "REGISTER", URI: cred.URI, Username: user, Password: r.password,
		})
		if err != nil || want.Response != cred.Response {
			_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusForbidden, "Forbidden", nil))
			return
		}
		r.mu.Lock()
		r.contacts = append(r.contacts, req.Contact().Value())
		if ua := req.GetHeader("User-Agent"); ua != nil {
			r.userAgts = append(r.userAgts, ua.Value())
		}
		r.mu.Unlock()
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		res.AppendHeader(req.Contact().Clone())
		_ = tx.Respond(res)
	})

	// Listen before serving, so the registrar is up when this returns.
	udp, err := net.ListenPacket("udp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeUDP(udp) }()
	go func() { _ = srv.ServeTCP(tcp) }()
	t.Cleanup(func() {
		_ = udp.Close()
		_ = tcp.Close()
		_ = ua.Close()
	})
	return r
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRegister(t *testing.T) {
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			reg := startRegistrar(t, "e2e-phone", "s3cret")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			port := freePort(t)
			p, err := newPhone(ctx, sipOptions{
				Server: reg.addr, Transport: transport, User: "e2e-phone", Password: "s3cret",
				LocalIP: "127.0.0.1", SIPPort: port, UserAgent: "unit-test",
			}, quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer p.close()

			res, err := p.register(ctx, 120)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != sip.StatusOK {
				t.Fatalf("REGISTER: %d %s", res.StatusCode, res.Reason)
			}
			reg.mu.Lock()
			defer reg.mu.Unlock()
			want := fmt.Sprintf("<sip:e2e-phone@127.0.0.1:%d;transport=%s>", port, transport)
			if len(reg.contacts) != 1 || reg.contacts[0] != want {
				t.Fatalf("registrar saw contacts %q, want [%q]", reg.contacts, want)
			}
			if len(reg.userAgts) != 1 || reg.userAgts[0] != "unit-test" {
				t.Fatalf("registrar saw User-Agents %q", reg.userAgts)
			}
		})
	}
}

func TestRegisterWrongPassword(t *testing.T) {
	reg := startRegistrar(t, "e2e-phone", "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := newPhone(ctx, sipOptions{
		Server: reg.addr, Transport: "tcp", User: "e2e-phone", Password: "wrong",
		LocalIP: "127.0.0.1", SIPPort: freePort(t), UserAgent: "unit-test",
	}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	res, err := p.register(ctx, 120)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != sip.StatusForbidden {
		t.Fatalf("REGISTER with a wrong password: %d, want 403", res.StatusCode)
	}
}

func TestNewPhoneRejectsBadOptions(t *testing.T) {
	ctx := context.Background()
	for name, o := range map[string]sipOptions{
		"server without port": {Server: "127.0.0.1", Transport: "tcp", User: "u"},
		"tls transport":       {Server: "127.0.0.1:5060", Transport: "tls", User: "u"},
		"no user":             {Server: "127.0.0.1:5060", Transport: "tcp"},
		"bad local ip":        {Server: "127.0.0.1:5060", Transport: "tcp", User: "u", LocalIP: "nope"},
	} {
		if _, err := newPhone(ctx, o, quietLogger()); err == nil {
			t.Errorf("%s: newPhone accepted %+v", name, o)
		}
	}
}
