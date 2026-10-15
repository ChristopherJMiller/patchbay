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
// with digest auth (as Asterisk does) and records what it accepted. With an
// empty password it is open instead: it accepts any REGISTER unchallenged,
// like an endpoint that lost its auth.
type registrar struct {
	addr     string // 127.0.0.1:port, UDP and TCP
	password string

	mu       sync.Mutex
	contacts []string // Contact values of accepted REGISTERs
	userAgts []string
}

// freePort is a loopback port free in both UDP and TCP at the time of the
// call: a phone binds SIP on both, and a port free in TCP may be held in
// UDP by another test binary (TFTP, RTP and AMI fakes run in parallel
// under make test), which failed runs with EADDRINUSE. It binds UDP on
// :0, then TCP on the same port, and tries again while that is taken.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	for range 50 {
		udp, err := lc.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := udp.LocalAddr().(*net.UDPAddr).Port
		tcp, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		_ = udp.Close()
		if err == nil {
			_ = tcp.Close()
			return port
		}
	}
	t.Fatal("no loopback port free in both UDP and TCP in 50 tries")
	return 0
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
		if r.password == "" {
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			res.AppendHeader(req.Contact().Clone())
			_ = tx.Respond(res)
			return
		}
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

			res, challenged, err := p.register(ctx, 120)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != sip.StatusOK || !challenged {
				t.Fatalf("REGISTER: %d %s, challenged=%t; want 200 after a challenge", res.StatusCode, res.Reason, challenged)
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
	res, _, err := p.register(ctx, 120)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != sip.StatusForbidden {
		t.Fatalf("REGISTER with a wrong password: %d, want 403", res.StatusCode)
	}
}

// TestRegisterReportsNoChallenge: against a PBX that accepts REGISTER
// without asking for credentials, register succeeds but says so, which is
// what lets the e2e suite catch an endpoint that lost its auth.
func TestRegisterReportsNoChallenge(t *testing.T) {
	reg := startRegistrar(t, "e2e-phone", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := newPhone(ctx, sipOptions{
		Server: reg.addr, Transport: "tcp", User: "e2e-phone", Password: "unused",
		LocalIP: "127.0.0.1", SIPPort: freePort(t), UserAgent: "unit-test",
	}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	res, challenged, err := p.register(ctx, 120)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != sip.StatusOK || challenged {
		t.Fatalf("REGISTER to an open registrar: %d, challenged=%t; want 200, false", res.StatusCode, challenged)
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

// TestRegisterWithAuthUser: on compiled config the SIP user is the number
// and the digest username the device ID; --contact-host replaces only the
// Contact's host.
func TestRegisterWithAuthUser(t *testing.T) {
	reg := startRegistrar(t, "phone-desk", "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	port := freePort(t)
	p, err := newPhone(ctx, sipOptions{
		Server: reg.addr, Transport: "tcp", User: "100", AuthUser: "phone-desk", Password: "s3cret",
		LocalIP: "127.0.0.1", ContactHost: "192.0.2.7", SIPPort: port, UserAgent: "unit-test",
	}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	res, challenged, err := p.register(ctx, 120)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != sip.StatusOK || !challenged {
		t.Fatalf("REGISTER: %d, challenged=%t", res.StatusCode, challenged)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if want := fmt.Sprintf("<sip:100@192.0.2.7:%d;transport=tcp>", port); len(reg.contacts) != 1 || reg.contacts[0] != want {
		t.Fatalf("registrar saw contacts %q, want [%q]", reg.contacts, want)
	}
}

// sendNotify sends one NOTIFY with event and body to a phone listening on
// 127.0.0.1:port over UDP, and returns the response code.
func sendNotify(t *testing.T, port int, event, body string) int {
	t.Helper()
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("test-pbx"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ua.Close() }()
	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	req := sip.NewRequest(sip.NOTIFY, sip.Uri{Scheme: "sip", User: "100", Host: "127.0.0.1", Port: port})
	req.AppendHeader(sip.NewHeader("Event", event))
	req.AppendHeader(sip.NewHeader("Subscription-State", "active"))
	ct := sip.ContentTypeHeader("text/plain")
	req.AppendHeader(&ct)
	req.SetBody([]byte(body))
	req.SetTransport("UDP")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.Do(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

// TestWaitNotify: a phone answers every NOTIFY with 200, skips other
// events, and hands the service-control one to the waiting command.
func TestWaitNotify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	port := freePort(t)
	p, err := newPhone(ctx, sipOptions{
		Server: "127.0.0.1:5060", Transport: "udp", User: "100", LocalIP: "127.0.0.1", SIPPort: port,
	}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if code := sendNotify(t, port, "check-sync", "x"); code != sip.StatusOK {
		t.Fatalf("NOTIFY answered %d", code)
	}
	body := "action=restart\r\nRegisterCallId={abc}"
	if code := sendNotify(t, port, "service-control", body); code != sip.StatusOK {
		t.Fatalf("NOTIFY answered %d", code)
	}
	req, err := p.waitNotify(ctx, "service-control", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(req.Body()) != body || fmt.Sprint(subscriptionState(req)) != "[active]" {
		t.Fatalf("NOTIFY body %q, Subscription-State %v", req.Body(), subscriptionState(req))
	}
	if _, err := p.waitNotify(ctx, "service-control", 100*time.Millisecond); err == nil {
		t.Fatal("a second NOTIFY appeared from nowhere")
	}
}

// TestSetAccount: after setAccount the phone registers, and names itself in
// its Contact, as the new account (run mode, after a refetch).
func TestSetAccount(t *testing.T) {
	reg := startRegistrar(t, "phone-new", "not-a-secret-new")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	port := freePort(t)
	p, err := newPhone(ctx, sipOptions{
		Server: reg.addr, Transport: "tcp", User: "100", AuthUser: "phone-old", Password: "not-a-secret-old",
		LocalIP: "127.0.0.1", SIPPort: port, UserAgent: "unit-test",
	}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if res, _, err := p.register(ctx, 120); err != nil || res.StatusCode != sip.StatusForbidden {
		t.Fatalf("the old account: %v %v", res, err)
	}
	acc := p.account()
	acc.User, acc.AuthUser, acc.Password = "101", "phone-new", "not-a-secret-new"
	p.setAccount(acc)
	if res, _, err := p.register(ctx, 120); err != nil || res.StatusCode != sip.StatusOK {
		t.Fatalf("the new account: %v %v", res, err)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if want := fmt.Sprintf("<sip:101@127.0.0.1:%d;transport=tcp>", port); len(reg.contacts) != 1 || reg.contacts[0] != want {
		t.Fatalf("registrar saw contacts %q, want [%q]", reg.contacts, want)
	}
}
