package ami

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

// fakeAMI is an AMI server on a loopback port: it greets, answers
// Challenge, Login (one password, by MD5 challenge-response as Asterisk's
// manager.c checks it, or a plaintext Secret) and CoreSettings itself, and
// hands every other action to reply. It records each request's exact
// bytes, with the ActionID's value written as <id> so goldens do not depend
// on the counter.
type fakeAMI struct {
	t  *testing.T
	ln net.Listener

	mu        sync.Mutex
	password  string
	banner    string
	challenge string // what Challenge answers; "" refuses it
	reply     func(c *fakeConn, r fakeRequest)
	requests  []string
	conns     []*fakeConn
	logins    []string // per Login, "user/<the password it matched>" or "user/<refused>"
}

// fakeChallenge is the fake's challenge unless a test sets another, so the
// Login golden's Key is fixed.
const fakeChallenge = "1234567890"

// md5Key is the Key that answers a challenge, as Asterisk's manager.c
// computes it: the MD5 of the challenge then the secret, in lowercase hex.
func md5Key(challenge, secret string) string {
	sum := md5.Sum([]byte(challenge + secret))
	return hex.EncodeToString(sum[:])
}

// fakeRequest is one action as the fake read it.
type fakeRequest struct {
	action string
	id     string
	fields message
	raw    string
}

// fakeConn is one client connection.
type fakeConn struct {
	nc  net.Conn
	wmu sync.Mutex
}

// send writes one message: lines joined with CRLF, then the empty line.
func (c *fakeConn) send(lines ...string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.nc.Write([]byte(strings.Join(lines, "\r\n") + "\r\n\r\n"))
}

// raw writes bytes as given.
func (c *fakeConn) raw(s string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.nc.Write([]byte(s))
}

func newFakeAMI(t *testing.T, password string) *fakeAMI {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAMI{t: t, ln: ln, password: password, banner: "Asterisk Call Manager/9.0.0", challenge: fakeChallenge}
	go f.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		f.dropAll()
	})
	return f
}

func (f *fakeAMI) addr() string { return f.ln.Addr().String() }

func (f *fakeAMI) setReply(reply func(c *fakeConn, r fakeRequest)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = reply
}

func (f *fakeAMI) setPassword(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.password = p
}

// dropAll closes every client connection, as an Asterisk restart does.
func (f *fakeAMI) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.nc.Close()
	}
	f.conns = nil
}

// loginRequests returns the recorded Login requests.
func (f *fakeAMI) loginRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if strings.HasPrefix(r, "Action: Login\r\n") {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAMI) allRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeAMI) accept() {
	for {
		nc, err := f.ln.Accept()
		if err != nil {
			return
		}
		c := &fakeConn{nc: nc}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		banner := f.banner
		f.mu.Unlock()
		go f.serve(c, banner)
	}
}

func (f *fakeAMI) serve(c *fakeConn, banner string) {
	defer func() { _ = c.nc.Close() }()
	c.raw(banner + "\r\n")
	br := bufio.NewReader(c.nc)
	issued := "" // this connection's challenge, once asked for
	for {
		var raw strings.Builder
		var fields message
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			raw.WriteString(line)
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			k, v, _ := strings.Cut(line, ": ")
			fields = append(fields, field{k, v})
		}
		r := fakeRequest{action: fields.get("Action"), id: fields.get("ActionID"), fields: fields}
		r.raw = strings.Replace(raw.String(), "ActionID: "+r.id+"\r\n", "ActionID: <id>\r\n", 1)
		f.mu.Lock()
		f.requests = append(f.requests, r.raw)
		password, reply, challenge := f.password, f.reply, f.challenge
		f.mu.Unlock()
		switch r.action {
		case "Challenge":
			if challenge == "" || !strings.EqualFold(fields.get("AuthType"), "MD5") {
				c.send("Response: Error", "ActionID: "+r.id, "Message: Must specify AuthType")
				continue
			}
			issued = challenge
			c.send("Response: Success", "ActionID: "+r.id, "Challenge: "+issued)
		case "Login":
			var ok bool
			if strings.EqualFold(fields.get("AuthType"), "MD5") {
				ok = issued != "" && fields.get("Key") == md5Key(issued, password)
			} else {
				ok = fields.get("Secret") == password
			}
			who := fields.get("Username") + "/<refused>"
			if ok {
				who = fields.get("Username") + "/" + password
			}
			f.mu.Lock()
			f.logins = append(f.logins, who)
			f.mu.Unlock()
			if ok {
				c.send("Response: Success", "ActionID: "+r.id, "Message: Authentication accepted")
			} else {
				c.send("Response: Error", "ActionID: "+r.id, "Message: Authentication failed")
			}
		case "CoreSettings":
			c.send("Response: Success", "ActionID: "+r.id, "AMIversion: 9.0.0", "AsteriskVersion: 22.11.0", "SystemName: ")
		default:
			if reply == nil {
				c.send("Response: Success", "ActionID: "+r.id, "Message: ok")
				continue
			}
			reply(c, r)
		}
	}
}

// logSink captures every log line, for the no-password checks.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lines = append(s.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 10})
}

func (s *logSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

// startConn runs a client against the fake as user "patchbay-agent" with
// password pw, and waits until the session is up (unless wantUp is false).
func startConn(t *testing.T, f *fakeAMI, pw string, log logr.Logger) *Conn {
	t.Helper()
	c := New(Config{
		Addr:    f.addr(),
		Backoff: func(int) time.Duration { return 20 * time.Millisecond },
		Log:     log,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v, want nil once ctx is done", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after ctx was done")
		}
	})
	c.SetAccount("patchbay-agent", pw)
	return c
}

// waitFor polls cond until it holds or 5 s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitUp(t *testing.T, c *Conn) {
	t.Helper()
	waitFor(t, "the AMI session", func() bool { return c.State().Up })
}

func ctx5s(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// isNotConnected reports whether err is ErrNotConnected.
func isNotConnected(err error) bool { return errors.Is(err, ErrNotConnected) }
