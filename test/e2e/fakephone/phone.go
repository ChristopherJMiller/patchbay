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

// phoneConfig is one phone's SIP stack: where it listens, the account it
// starts with, and who handles what the PBX sends it.
type phoneConfig struct {
	Account     account
	LocalIP     netip.Addr
	ContactHost string
	SIPPort     int
	UserAgent   string
	// OnNotify, when set, is given every NOTIFY once the phone has answered
	// it 200; otherwise NOTIFYs go to the channel waitNotify reads.
	OnNotify func(req *sip.Request)
	// OnInvite, when set, handles every incoming INVITE (sipgo calls it in
	// the INVITE transaction's own goroutine); otherwise the phone answers
	// 486 Busy Here.
	OnInvite func(req *sip.Request, tx sip.ServerTransaction)
}

// phone is one simulated phone: a SIP user agent listening like a real
// phone (so the PBX can reach it at its Contact), plus a client for its own
// requests. Its account can change while it runs (run mode refetches its
// config).
type phone struct {
	cfg     phoneConfig
	log     *slog.Logger
	localIP netip.Addr

	mu  sync.Mutex
	acc account

	ua      *sipgo.UserAgent
	client  *sipgo.Client
	dialogs *sipgo.DialogClientCache // calls the phone placed
	uas     *sipgo.DialogServerCache // calls the phone answered

	// notifies receives every NOTIFY the PBX sends this phone, unless
	// cfg.OnNotify takes them.
	notifies chan *sip.Request

	closeOnce sync.Once
	closers   []func() error
}

// newPhone sets up the user agent of a one-shot SIP subcommand from its
// flags and starts listening on SIPPort over both UDP and TCP, the way a
// phone does, until ctx ends or close is called.
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
	return startPhone(ctx, phoneConfig{
		Account: account{
			Server: server, Transport: transport,
			User: opts.User, AuthUser: opts.AuthUser, Password: secret(opts.Password),
		},
		LocalIP: local, ContactHost: opts.ContactHost, SIPPort: opts.SIPPort, UserAgent: opts.UserAgent,
	}, log)
}

// startPhone starts a phone's SIP stack on cfg.LocalIP:cfg.SIPPort (UDP
// and TCP) until ctx ends or close is called.
func startPhone(ctx context.Context, cfg phoneConfig, log *slog.Logger) (*phone, error) {
	p := &phone{cfg: cfg, log: log, localIP: cfg.LocalIP, acc: cfg.Account, notifies: make(chan *sip.Request, 8)}
	name := cfg.Account.User
	if name == "" {
		name = "fakephone"
	}
	var err error
	p.ua, err = sipgo.NewUA(
		sipgo.WithUserAgent(name),
		sipgo.WithUserAgentHostname(cfg.LocalIP.String()),
	)
	if err != nil {
		return nil, err
	}
	p.closers = append(p.closers, p.ua.Close)

	srv, err := sipgo.NewServer(p.ua)
	if err != nil {
		p.close()
		return nil, err
	}
	p.client, err = sipgo.NewClient(p.ua,
		sipgo.WithClientHostname(cfg.LocalIP.String()),
		sipgo.WithClientPort(cfg.SIPPort),
	)
	if err != nil {
		p.close()
		return nil, err
	}
	contact := p.contactFor(cfg.Account)
	p.dialogs = sipgo.NewDialogClientCache(p.client, contact)
	p.uas = sipgo.NewDialogServerCache(p.client, contact)

	// What the PBX may send a phone: INVITE, in-dialog ACK and BYE, OPTIONS
	// (qualify) and NOTIFY (Cisco service-control; a REFER's progress).
	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		if cfg.OnInvite != nil {
			cfg.OnInvite(req, tx)
			return
		}
		p.respond(req, tx, sip.StatusBusyHere, "Busy Here")
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = p.uas.ReadAck(req, tx) // an ACK for a non-2xx belongs to its transaction
	})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := p.dialogs.ReadBye(req, tx); err == nil {
			p.log.Info("call ended by the PBX")
			return
		}
		if err := p.uas.ReadBye(req, tx); err == nil {
			p.log.Info("call ended by the PBX")
			return
		}
		p.log.Warn("BYE for no dialog", "callID", req.CallID().Value())
		p.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist")
	})
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		p.respond(req, tx, sip.StatusOK, "OK")
	})
	srv.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
		p.log.Debug("NOTIFY", "event", eventName(req), "body", string(req.Body()))
		p.respond(req, tx, sip.StatusOK, "OK")
		if cfg.OnNotify != nil {
			cfg.OnNotify(req)
			return
		}
		select {
		case p.notifies <- req:
		default:
			p.log.Warn("NOTIFY dropped: nobody is waiting")
		}
	})

	listen := net.JoinHostPort(cfg.LocalIP.String(), strconv.Itoa(cfg.SIPPort))
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

// account is the account the phone calls as now.
func (p *phone) account() account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acc
}

// setAccount switches the account the phone calls as (and that its
// Contact names), as a phone does after reading a new config.
func (p *phone) setAccount(a account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acc = a
}

// contactFor is the phone's Contact under account a:
// sip:<user>@<local ip or ContactHost>:<SIPPort>;transport=<t>.
func (p *phone) contactFor(a account) sip.ContactHeader {
	host := p.localIP.String()
	if p.cfg.ContactHost != "" {
		host = p.cfg.ContactHost
	}
	return sip.ContactHeader{Address: sip.Uri{
		Scheme:    "sip",
		User:      a.User,
		Host:      host,
		Port:      p.cfg.SIPPort,
		UriParams: sip.HeaderParams{{K: "transport", V: a.Transport}},
	}}
}

// serverURI is sip:[user@]<server>;transport=<t>.
func serverURI(a account, user string) sip.Uri {
	return sip.Uri{
		Scheme:    "sip",
		User:      user,
		Host:      a.Server.Addr().String(),
		Port:      int(a.Server.Port()),
		UriParams: sip.HeaderParams{{K: "transport", V: a.Transport}},
	}
}

// aor is the account's address of record, sip:<user>@<server host>.
func aor(a account) sip.Uri {
	return sip.Uri{Scheme: "sip", User: a.User, Host: a.Server.Addr().String()}
}

func (p *phone) userAgent() sip.Header {
	return sip.NewHeader("User-Agent", p.cfg.UserAgent)
}

// registerRequest is one REGISTER: the account it registers, the expiry it
// asks for (0 unregisters), and the Call-ID and CSeq a refreshing phone
// keeps across its REGISTERs (empty and 0: fresh ones).
type registerRequest struct {
	Account account
	Expires int
	CallID  string
	CSeq    uint32
}

// registerResult is how a REGISTER went.
type registerResult struct {
	Response *sip.Response
	// Challenged is whether the PBX asked for credentials at all.
	Challenged bool
	// CSeq is the CSeq of the last REGISTER sent (after a challenge, one
	// more than the request's).
	CSeq uint32
	// Granted is the expiry a 200 granted (its Contact's expires, else its
	// Expires header, else what was asked).
	Granted int
}

// register sends REGISTER as the phone's account for expires seconds (0
// unregisters), answering a digest challenge, and returns the final
// response and whether the PBX challenged at all. A PBX that accepts an
// unauthenticated REGISTER is misconfigured; the e2e suite checks that it
// challenged.
func (p *phone) register(ctx context.Context, expires int) (res *sip.Response, challenged bool, err error) {
	r, err := p.registerAs(ctx, registerRequest{Account: p.account(), Expires: expires})
	if err != nil {
		return nil, false, err
	}
	return r.Response, r.Challenged, nil
}

// registerAs sends one REGISTER transaction for r, answering a digest
// challenge with r's credentials.
func (p *phone) registerAs(ctx context.Context, r registerRequest) (registerResult, error) {
	a := r.Account
	req := sip.NewRequest(sip.REGISTER, serverURI(a, ""))
	from := &sip.FromHeader{Address: aor(a), Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(16))
	exp := sip.ExpiresHeader(r.Expires)
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: aor(a)})
	if r.CallID != "" {
		id := sip.CallIDHeader(r.CallID)
		req.AppendHeader(&id)
	}
	if r.CSeq != 0 {
		req.AppendHeader(&sip.CSeqHeader{SeqNo: r.CSeq, MethodName: sip.REGISTER})
	}
	contact := p.contactFor(a)
	req.AppendHeader(&contact)
	req.AppendHeader(&exp)
	req.AppendHeader(p.userAgent())
	req.SetTransport(strings.ToUpper(a.Transport))

	var out registerResult
	res, err := p.client.Do(ctx, req, sipgo.ClientRequestBuild)
	if err != nil {
		return out, fmt.Errorf("REGISTER: %w", err)
	}
	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		p.log.Debug("REGISTER challenged", "status", res.StatusCode)
		out.Challenged = true
		res, err = p.client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: a.authUser(),
			Password: a.Password.reveal(),
		})
		if err != nil {
			return out, fmt.Errorf("REGISTER with credentials: %w", err)
		}
	}
	out.Response, out.CSeq = res, req.CSeq().SeqNo
	if res.StatusCode == sip.StatusOK {
		out.Granted = grantedExpiry(res, contact.Address, r.Expires)
	}
	return out, nil
}

// grantedExpiry is the expiry a 200 to REGISTER granted the contact ours:
// its Contact's expires parameter (Asterisk lists every contact of the
// AOR), else the Expires header, else what was asked.
func grantedExpiry(res *sip.Response, ours sip.Uri, asked int) int {
	var first string
	for _, h := range res.GetHeaders("Contact") {
		c, ok := h.(*sip.ContactHeader)
		if !ok || c.Params == nil {
			continue
		}
		v, ok := c.Params.Get("expires")
		if !ok {
			continue
		}
		if c.Address.User == ours.User && c.Address.Host == ours.Host && c.Address.Port == ours.Port {
			first = v
			break
		}
		if first == "" {
			first = v
		}
	}
	if n, err := strconv.Atoi(first); err == nil && n >= 0 {
		return n
	}
	if h := res.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && n >= 0 {
			return n
		}
	}
	return asked
}

// call is one established outgoing call.
type call struct {
	dialog *sipgo.DialogClientSession
	remote remoteMedia
	callID string
}

// invite calls number as the phone's account with an SDP offer for the
// local RTP port and waits for the answer, answering a digest challenge.
// On success the call is established and ACKed; on a final failure the
// error wraps the *sipgo.ErrDialogResponse that carries the status.
func (p *phone) invite(ctx context.Context, number string, rtpPort int) (*call, error) {
	sid, _, _, err := rtpStart()
	if err != nil {
		return nil, err
	}
	a := p.account()
	from := &sip.FromHeader{Address: aor(a), Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(16))
	to := &sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: number, Host: a.Server.Addr().String()}}
	ct := sip.ContentTypeHeader("application/sdp")
	contact := p.contactFor(a)

	d, err := p.dialogs.Invite(ctx, serverURI(a, number), buildOffer(p.localIP, rtpPort, uint64(sid)),
		from, to, &contact, &ct, p.userAgent())
	if err != nil {
		return nil, fmt.Errorf("INVITE %s: %w", number, err)
	}
	callID := d.InviteRequest.CallID().Value()
	err = d.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: a.authUser(),
		Password: a.Password.reveal(),
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
	return &call{dialog: d, remote: remote, callID: callID}, nil
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

// eventName is a NOTIFY's event package: its Event header up to any
// parameter ("refer;id=7" is "refer").
func eventName(req *sip.Request) string {
	h := req.GetHeader("Event")
	if h == nil {
		return ""
	}
	name, _, _ := strings.Cut(h.Value(), ";")
	return strings.TrimSpace(name)
}

// waitNotify waits up to d for a NOTIFY whose Event is event (another
// event is logged and skipped).
func (p *phone) waitNotify(ctx context.Context, event string, d time.Duration) (*sip.Request, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case req := <-p.notifies:
			ev := eventName(req)
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
