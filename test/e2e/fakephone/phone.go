package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// sipOptions are the flags every SIP subcommand shares.
type sipOptions struct {
	Server    string // the PBX, host:port
	Transport string // tcp (the 79x5's choice) or udp
	User      string // the SIP user: the phone's number (AOR, From, To, Contact)
	AuthUser  string // the digest username (the device ID); empty means User
	Password  string
	LocalIP   string // the address put in Via, Contact and SDP; detected when empty
	// ContactHost replaces LocalIP in the Contact only (a contact the PBX's
	// contact ACL refuses, while the request itself comes from LocalIP).
	ContactHost string
	SIPPort     int // the local SIP port the phone listens on
	UserAgent   string
}

// phone is one simulated phone: a SIP user agent listening like a real
// phone (so the PBX can reach it at its Contact), plus a client for its own
// requests.
type phone struct {
	opts      sipOptions
	log       *slog.Logger
	server    netip.AddrPort
	transport string // lower case
	localIP   netip.Addr

	ua      *sipgo.UserAgent
	client  *sipgo.Client
	dialogs *sipgo.DialogClientCache
	contact sip.ContactHeader

	// notifies receives every NOTIFY the PBX sends this phone.
	notifies chan *sip.Request

	closeOnce sync.Once
	closers   []func() error
}

// newPhone sets up the user agent and starts listening on SIPPort over
// both UDP and TCP, the way a phone does, until ctx ends or close is called.
func newPhone(ctx context.Context, opts sipOptions, log *slog.Logger) (*phone, error) {
	server, err := netip.ParseAddrPort(opts.Server)
	if err != nil {
		return nil, fmt.Errorf("--server must be ip:port: %w", err)
	}
	transport := strings.ToLower(opts.Transport)
	if transport != "tcp" && transport != "udp" {
		return nil, fmt.Errorf("--transport must be tcp or udp, not %q", opts.Transport)
	}
	if opts.User == "" {
		return nil, errors.New("--user is required")
	}
	local, err := localAddrFor(opts.LocalIP, server)
	if err != nil {
		return nil, err
	}

	p := &phone{opts: opts, log: log, server: server, transport: transport, localIP: local,
		notifies: make(chan *sip.Request, 8)}
	p.ua, err = sipgo.NewUA(
		sipgo.WithUserAgent(opts.User),
		sipgo.WithUserAgentHostname(local.String()),
	)
	if err != nil {
		return nil, err
	}
	p.closers = append(p.closers, p.ua.Close)

	srv, err := sipgo.NewServer(p.ua)
	if err != nil {
		return nil, err
	}
	p.client, err = sipgo.NewClient(p.ua,
		sipgo.WithClientHostname(local.String()),
		sipgo.WithClientPort(opts.SIPPort),
	)
	if err != nil {
		return nil, err
	}

	contactHost := local.String()
	if opts.ContactHost != "" {
		contactHost = opts.ContactHost
	}
	p.contact = sip.ContactHeader{Address: sip.Uri{
		Scheme:    "sip",
		User:      opts.User,
		Host:      contactHost,
		Port:      opts.SIPPort,
		UriParams: sip.HeaderParams{{K: "transport", V: transport}},
	}}
	p.dialogs = sipgo.NewDialogClientCache(p.client, p.contact)

	// What the PBX may send a phone: in-dialog BYE, OPTIONS (qualify) and
	// NOTIFY (Cisco service-control, later phases).
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := p.dialogs.ReadBye(req, tx); err != nil {
			p.log.Warn("BYE for no dialog", "err", err)
			p.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist")
			return
		}
		p.log.Info("call ended by the PBX")
	})
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		p.respond(req, tx, sip.StatusOK, "OK")
	})
	srv.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
		ev := ""
		if h := req.GetHeader("Event"); h != nil {
			ev = h.Value()
		}
		p.log.Info("NOTIFY", "event", ev, "body", string(req.Body()))
		p.respond(req, tx, sip.StatusOK, "OK")
		select {
		case p.notifies <- req:
		default:
			p.log.Warn("NOTIFY dropped: nobody is waiting")
		}
	})

	listen := net.JoinHostPort(local.String(), strconv.Itoa(opts.SIPPort))
	var lc net.ListenConfig
	udp, err := lc.ListenPacket(ctx, "udp", listen)
	if err != nil {
		p.close()
		return nil, fmt.Errorf("SIP listener: %w", err)
	}
	p.closers = append(p.closers, udp.Close)
	tcp, err := lc.Listen(ctx, "tcp", listen)
	if err != nil {
		p.close()
		return nil, fmt.Errorf("SIP listener: %w", err)
	}
	p.closers = append(p.closers, tcp.Close)
	go func() { _ = srv.ServeUDP(udp) }()
	go func() { _ = srv.ServeTCP(tcp) }()
	go func() {
		<-ctx.Done()
		p.close()
	}()
	log.Debug("listening", "addr", listen)
	return p, nil
}

// close stops listening and frees the user agent. It is safe to call twice
// and from any goroutine.
func (p *phone) close() {
	p.closeOnce.Do(func() {
		for i := len(p.closers) - 1; i >= 0; i-- {
			_ = p.closers[i]()
		}
	})
}

func (p *phone) respond(req *sip.Request, tx sip.ServerTransaction, code int, reason string) {
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	if err := tx.Respond(res); err != nil {
		p.log.Warn("respond", "code", code, "err", err)
	}
}

// serverURI is sip:[user@]<server>;transport=<t>.
func (p *phone) serverURI(user string) sip.Uri {
	return sip.Uri{
		Scheme:    "sip",
		User:      user,
		Host:      p.server.Addr().String(),
		Port:      int(p.server.Port()),
		UriParams: sip.HeaderParams{{K: "transport", V: p.transport}},
	}
}

// aor is the phone's address of record, sip:<user>@<server host>.
func (p *phone) aor() sip.Uri {
	return sip.Uri{Scheme: "sip", User: p.opts.User, Host: p.server.Addr().String()}
}

// authUser is the digest username: the device ID, which is not the SIP
// user on compiled config (a device's number is its SIP user).
func (p *phone) authUser() string {
	if p.opts.AuthUser != "" {
		return p.opts.AuthUser
	}
	return p.opts.User
}

func (p *phone) userAgent() sip.Header {
	return sip.NewHeader("User-Agent", p.opts.UserAgent)
}

// register sends REGISTER for expires seconds (0 unregisters), answering a
// digest challenge, and returns the final response and whether the PBX
// challenged at all. A PBX that accepts an unauthenticated REGISTER is
// misconfigured; the e2e suite checks that it challenged.
func (p *phone) register(ctx context.Context, expires int) (res *sip.Response, challenged bool, err error) {
	req := sip.NewRequest(sip.REGISTER, p.serverURI(""))
	from := &sip.FromHeader{Address: p.aor(), Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(16))
	exp := sip.ExpiresHeader(expires)
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: p.aor()})
	req.AppendHeader(p.contact.Clone())
	req.AppendHeader(&exp)
	req.AppendHeader(p.userAgent())
	req.SetTransport(strings.ToUpper(p.transport))

	res, err = p.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return nil, false, fmt.Errorf("REGISTER: %w", err)
	}
	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		p.log.Debug("REGISTER challenged", "status", res.StatusCode)
		challenged = true
		res, err = p.client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: p.authUser(),
			Password: p.opts.Password,
		})
		if err != nil {
			return nil, true, fmt.Errorf("REGISTER with credentials: %w", err)
		}
	}
	return res, challenged, nil
}

// call is one established outgoing call.
type call struct {
	dialog *sipgo.DialogClientSession
	remote remoteMedia
}

// invite calls number with an SDP offer for the local RTP port and waits for
// the answer, answering a digest challenge. On success the call is
// established and ACKed.
func (p *phone) invite(ctx context.Context, number string, rtpPort int) (*call, error) {
	sid, _, _, err := rtpStart()
	if err != nil {
		return nil, err
	}
	from := &sip.FromHeader{Address: p.aor(), Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(16))
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: number, Host: p.server.Addr().String()}}
	ct := sip.ContentTypeHeader("application/sdp")

	d, err := p.dialogs.Invite(ctx, p.serverURI(number), buildOffer(p.localIP, rtpPort, uint64(sid)),
		from, to, &ct, p.userAgent())
	if err != nil {
		return nil, fmt.Errorf("INVITE %s: %w", number, err)
	}
	err = d.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: p.authUser(),
		Password: p.opts.Password,
		OnResponse: func(res *sip.Response) error {
			p.log.Debug("INVITE response", "status", res.StatusCode, "reason", res.Reason)
			return nil
		},
	})
	if err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("INVITE %s: %w", number, err)
	}
	if err := d.Ack(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("ACK: %w", err)
	}
	remote, err := parseAnswer(d.InviteResponse.Body())
	if err != nil {
		_ = d.Bye(ctx)
		_ = d.Close()
		return nil, fmt.Errorf("answer SDP: %w", err)
	}
	return &call{dialog: d, remote: remote}, nil
}

// hangup sends BYE (unless the PBX already ended the call) and frees the dialog.
func (c *call) hangup(ctx context.Context) error {
	defer func() { _ = c.dialog.Close() }()
	if c.dialog.LoadState() == sip.DialogStateEnded {
		return nil
	}
	return c.dialog.Bye(ctx)
}

// localAddrFor returns explicit if set, else the local address the kernel
// would use to reach server (no packet is sent).
func localAddrFor(explicit string, server netip.AddrPort) (netip.Addr, error) {
	if explicit != "" {
		a, err := netip.ParseAddr(explicit)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("--local-ip: %w", err)
		}
		return a, nil
	}
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(server))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("finding the local address towards %s: %w", server, err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}

// waitNotify waits up to d for a NOTIFY whose Event is event (another
// event is logged and skipped).
func (p *phone) waitNotify(ctx context.Context, event string, d time.Duration) (*sip.Request, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case req := <-p.notifies:
			ev := ""
			if h := req.GetHeader("Event"); h != nil {
				ev = h.Value()
			}
			if ev == event {
				return req, nil
			}
			p.log.Info("ignoring a NOTIFY", "event", ev, "want", event)
		case <-timer.C:
			return nil, fmt.Errorf("no NOTIFY with Event %s within %s", event, d)
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for a NOTIFY: %w", ctx.Err())
		}
	}
}
