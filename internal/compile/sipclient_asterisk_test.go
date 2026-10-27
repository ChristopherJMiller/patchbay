//go:build asterisk

package compile_test

// A minimal SIP user agent the real-Asterisk check runs inside the Asterisk
// container (the container has no network, and with rootless Docker the
// host could not reach it anyway): the test binary copies itself in, and
// TestMain runs it as a client when sipClientEnv is set. It REGISTERs, or
// calls a number, as a device would, with the device's credentials, and
// prints one result line.

import (
	"cmp"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// sipClientEnv selects the client mode: "register" or "call".
const sipClientEnv = "PATCHBAY_CHECK_SIP_CLIENT"

func TestMain(m *testing.M) {
	if mode := os.Getenv(sipClientEnv); mode != "" {
		os.Exit(runSIPClient(mode))
	}
	if os.Getenv(amiChannelsEnv) != "" {
		os.Exit(runAMIChannels())
	}
	if dir := os.Getenv(audioSocketEnv); dir != "" {
		os.Exit(runAudioSocketServer(dir))
	}
	os.Exit(m.Run())
}

// sipClient is one client run's settings, from the environment.
type sipClient struct {
	server    string // the PBX, 127.0.0.1:<port>
	transport string // udp or tcp
	user      string // the number: From, To and Contact user
	authUser  string // the digest username: the device ID
	password  string
	port      int // the client's own SIP port
	expires   int // REGISTER: the expiry asked for
	target    string
	contact   string // the Contact host: 127.0.0.1 unless SIP_CONTACT_HOST says otherwise
	// hold is how long an answered call stays up before the client sends
	// BYE (SIP_HOLD, a Go duration; zero: at once). A BYE from the PBX
	// first ends it.
	hold time.Duration
	// referTo, when set (SIP_REFER_TO, a number), blind-transfers the
	// answered call there referAfter (SIP_REFER_AFTER) after it is up, by
	// an in-dialog REFER as a phone's transfer key sends one; the call
	// then lasts until the PBX ends it (or hold passes).
	referTo    string
	referAfter time.Duration
}

func clientFromEnv() (sipClient, error) {
	c := sipClient{
		server:    os.Getenv("SIP_SERVER"),
		transport: os.Getenv("SIP_TRANSPORT"),
		user:      os.Getenv("SIP_USER"),
		authUser:  os.Getenv("SIP_AUTH_USER"),
		password:  os.Getenv("SIP_PASSWORD"),
		target:    os.Getenv("SIP_TARGET"),
		contact:   cmp.Or(os.Getenv("SIP_CONTACT_HOST"), "127.0.0.1"),
		referTo:   os.Getenv("SIP_REFER_TO"),
	}
	var err1, err2, err3, err4 error
	c.port, err1 = strconv.Atoi(os.Getenv("SIP_PORT"))
	c.expires, err2 = strconv.Atoi(os.Getenv("SIP_EXPIRES"))
	if h := os.Getenv("SIP_HOLD"); h != "" {
		c.hold, err3 = time.ParseDuration(h)
	}
	if a := os.Getenv("SIP_REFER_AFTER"); a != "" {
		c.referAfter, err4 = time.ParseDuration(a)
	}
	return c, errors.Join(err1, err2, err3, err4)
}

// runSIPClient prints "status=<code> challenged=<bool> expires=<granted>"
// for a REGISTER, or "status=<code>" for a call (with SIP_HOLD,
// "status=<code> endedBy=<local|remote> held=<seconds>", and with
// SIP_REFER_TO " refer=<the REFER's status>" after it), and exits 0 when
// the exchange completed (whatever the status) or 2 when it could not.
func runSIPClient(mode string) int {
	// The result line is the only output: sipgo's own log lines (a "TCP
	// ref went negative" warning when the PBX's BYE and the client's close
	// cross) would otherwise land in the output the check compares.
	sip.SetDefaultLogger(slog.New(slog.DiscardHandler))
	c, err := clientFromEnv()
	if err != nil {
		fmt.Println("error:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second+c.hold)
	defer cancel()
	switch mode {
	case "register":
		err = c.register(ctx)
	case "call":
		err = c.call(ctx)
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		fmt.Println("error:", err)
		return 2
	}
	return 0
}

// agent starts a user agent listening on 127.0.0.1:port over UDP and TCP,
// as a phone does, and a client sending from it.
// onBye, when set, answers the BYEs the PBX sends.
func (c sipClient) agent(ctx context.Context, onBye sipgo.RequestHandler) (*sipgo.Client, sip.ContactHeader, func(), error) {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("patchbay-asterisk-check"), sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		return nil, sip.ContactHeader{}, nil, err
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return nil, sip.ContactHeader{}, nil, err
	}
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})
	if onBye != nil {
		srv.OnBye(onBye)
	}
	if c.referTo != "" { // a REFER's progress (RFC 3515): taken, not read
		srv.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
			_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
		})
	}
	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"), sipgo.WithClientPort(c.port))
	if err != nil {
		return nil, sip.ContactHeader{}, nil, err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(c.port))
	var lc net.ListenConfig
	udp, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return nil, sip.ContactHeader{}, nil, err
	}
	tcp, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		_ = udp.Close()
		return nil, sip.ContactHeader{}, nil, err
	}
	go func() { _ = srv.ServeUDP(udp) }()
	go func() { _ = srv.ServeTCP(tcp) }()
	contact := sip.ContactHeader{Address: sip.Uri{
		Scheme: "sip", User: c.user, Host: c.contact, Port: c.port,
		UriParams: sip.HeaderParams{{K: "transport", V: c.transport}},
	}}
	return client, contact, func() { _ = tcp.Close(); _ = udp.Close(); _ = ua.Close() }, nil
}

func (c sipClient) uri(user string) sip.Uri {
	host, port, _ := net.SplitHostPort(c.server)
	p, _ := strconv.Atoi(port)
	return sip.Uri{Scheme: "sip", User: user, Host: host, Port: p, UriParams: sip.HeaderParams{{K: "transport", V: c.transport}}}
}

func (c sipClient) aor() sip.Uri {
	host, _, _ := net.SplitHostPort(c.server)
	return sip.Uri{Scheme: "sip", User: c.user, Host: host}
}

func (c sipClient) register(ctx context.Context) error {
	client, contact, stop, err := c.agent(ctx, nil)
	if err != nil {
		return err
	}
	defer stop()
	req := sip.NewRequest(sip.REGISTER, c.uri(""))
	from := &sip.FromHeader{Address: c.aor(), Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(16))
	exp := sip.ExpiresHeader(c.expires)
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: c.aor()})
	req.AppendHeader(contact.Clone())
	req.AppendHeader(&exp)
	req.SetTransport(strings.ToUpper(c.transport))
	res, err := client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return err
	}
	challenged := res.StatusCode == sip.StatusUnauthorized
	if challenged {
		if res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{Username: c.authUser, Password: c.password}); err != nil {
			return err
		}
	}
	granted, bound := "", ""
	if h := res.Contact(); h != nil {
		granted, _ = h.Params.Get("expires")
		bound = h.Address.String()
	}
	fmt.Printf("status=%d challenged=%t expires=%s contact=%s\n", res.StatusCode, challenged, granted, bound)
	return nil
}

func (c sipClient) call(ctx context.Context) error {
	var cache atomic.Pointer[sipgo.DialogClientCache]
	client, contact, stop, err := c.agent(ctx, func(req *sip.Request, tx sip.ServerTransaction) {
		if d := cache.Load(); d == nil || d.ReadBye(req, tx) != nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
		}
	})
	if err != nil {
		return err
	}
	defer stop()
	dialogs := sipgo.NewDialogClientCache(client, contact)
	cache.Store(dialogs)
	from := &sip.FromHeader{Address: c.aor(), Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(16))
	to := &sip.ToHeader{Address: c.uri(c.target)}
	ct := sip.ContentTypeHeader("application/sdp")
	offer := "v=0\r\no=check 1 1 IN IP4 127.0.0.1\r\ns=check\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n" +
		fmt.Sprintf("m=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=sendrecv\r\n", c.port+1)
	d, err := dialogs.Invite(ctx, c.uri(c.target), []byte(offer), from, to, &ct)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	err = d.WaitAnswer(ctx, sipgo.AnswerOptions{Username: c.authUser, Password: c.password})
	var dialogErr *sipgo.ErrDialogResponse
	switch {
	case errors.As(err, &dialogErr):
		fmt.Printf("status=%d\n", dialogErr.Res.StatusCode)
		return nil
	case err != nil:
		return err
	}
	status := d.InviteResponse.StatusCode
	if err := d.Ack(ctx); err != nil {
		return err
	}
	if c.hold <= 0 {
		if err := d.Bye(ctx); err != nil {
			return err
		}
		fmt.Printf("status=%d\n", status)
		return nil
	}
	start := time.Now()
	hold := time.After(c.hold)
	refer := ""
	if c.referTo != "" {
		select {
		case <-d.Context().Done():
		case <-time.After(c.referAfter):
			code, err := c.refer(ctx, d)
			if err != nil {
				return err
			}
			refer = fmt.Sprintf(" refer=%d", code)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	endedBy := "local"
	select {
	case <-d.Context().Done():
		endedBy = "remote"
	case <-hold:
	case <-ctx.Done():
		return ctx.Err()
	}
	held := time.Since(start)
	if err := d.Bye(ctx); err != nil { // nil once the PBX has ended the dialog
		return err
	}
	fmt.Printf("status=%d endedBy=%s held=%.1f%s\n", status, endedBy, held.Seconds(), refer)
	return nil
}

// refer blind-transfers the call d to c.referTo, as a phone's transfer
// key does: an in-dialog REFER to the PBX's Contact with Refer-To
// sip:<number>@<the PBX>. It returns the REFER's final status (202 when
// the PBX took it; Asterisk does not challenge in-dialog requests, so a
// challenge is a status like any other).
func (c sipClient) refer(ctx context.Context, d *sipgo.DialogClientSession) (int, error) {
	host, _, _ := net.SplitHostPort(c.server)
	referTo := sip.Uri{Scheme: "sip", User: c.referTo, Host: host}
	req := sip.NewRequest(sip.REFER, d.InviteResponse.Contact().Address)
	req.AppendHeader(sip.NewHeader("Refer-To", "<"+referTo.String()+">"))
	from := c.aor()
	req.AppendHeader(sip.NewHeader("Referred-By", "<"+from.String()+">"))
	res, err := d.Do(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("REFER: %w", err)
	}
	return res.StatusCode, nil
}

// requireStatic fails unless the test binary is statically linked: it runs
// inside the Asterisk image, which has none of this host's libraries.
func requireStatic(t *testing.T, path string) {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			t.Fatalf("%s is dynamically linked, so it cannot run inside the Asterisk container as the SIP client: "+
				"build the test with CGO_ENABLED=0 (make asterisk-check does)", path)
		}
	}
}
