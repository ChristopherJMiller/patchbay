package main

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/rtp"
)

// fakePBX is an in-process PBX on 127.0.0.1 (UDP and TCP) for the tests
// that need more than a registrar: it challenges REGISTER, INVITE and
// (optionally) REFER with digest auth as Asterisk does, answers the phone's
// calls with an RTP peer, accepts an in-dialog REFER and reports it with a
// sipfrag NOTIFY, and sends the phone NOTIFYs and calls of its own.
type fakePBX struct {
	t      *testing.T
	addr   netip.AddrPort
	client *sipgo.Client
	uas    *sipgo.DialogServerCache
	uac    *sipgo.DialogClientCache

	mu             sync.Mutex
	passwords      map[string]string // digest username -> password
	maxGrant       int               // grant at most this expiry; 0: what was asked
	answerHz       float64           // the tone the PBX's RTP peer sends back; 0 echoes
	challengeRefer bool
	registers      []pbxRegister
	contacts       map[string]sip.Uri // SIP user -> the contact of its last 200
	invites        []pbxInvite
	refers         []pbxRefer
	byes           int
}

type pbxRegister struct {
	AuthUser  string // "" when the REGISTER carried no credentials
	Status    int
	Expires   int // asked for
	Granted   int
	CallID    string
	CSeq      uint32
	Transport string
	Contact   string
	At        time.Time
}

type pbxInvite struct {
	Number   string // the request URI's user
	From     string
	AuthUser string
}

type pbxRefer struct {
	ReferTo    string
	ReferredBy string
	CallID     string
	Authorized bool // carried valid credentials
}

var pbxChallenge = digest.Challenge{Realm: "patchbay-test", Nonce: "n0nce", Algorithm: "MD5"}

func startPBX(t *testing.T, passwords map[string]string) *fakePBX {
	t.Helper()
	port := freePort(t)
	pbx := &fakePBX{
		t:         t,
		addr:      netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)),
		passwords: passwords,
		contacts:  map[string]sip.Uri{},
	}
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("fake-pbx"), sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}
	pbx.client, err = sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"), sipgo.WithClientPort(port))
	if err != nil {
		t.Fatal(err)
	}
	contact := sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "pbx", Host: "127.0.0.1", Port: port}}
	pbx.uas = sipgo.NewDialogServerCache(pbx.client, contact)
	pbx.uac = sipgo.NewDialogClientCache(pbx.client, contact)

	srv.OnRegister(pbx.onRegister)
	srv.OnInvite(pbx.onInvite)
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) { _ = pbx.uas.ReadAck(req, tx) })
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		err := pbx.uas.ReadBye(req, tx)
		if err != nil {
			err = pbx.uac.ReadBye(req, tx)
		}
		if err != nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "No Dialog", nil))
			return
		}
		pbx.mu.Lock()
		pbx.byes++
		pbx.mu.Unlock()
	})
	srv.OnRefer(pbx.onRefer)
	srv.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})

	udp, err := net.ListenPacket("udp", pbx.addr.String())
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", pbx.addr.String())
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
	return pbx
}

// setAnswerHz sets the tone the PBX's RTP peer sends back in its calls.
func (pbx *fakePBX) setAnswerHz(hz float64) {
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	pbx.answerHz = hz
}

// authorized checks req's Authorization against the PBX's passwords; it
// returns the digest username, or "" without (valid) credentials.
func (pbx *fakePBX) authorized(req *sip.Request) (string, bool) {
	h := req.GetHeader("Authorization")
	if h == nil {
		return "", false
	}
	cred, err := digest.ParseCredentials(h.Value())
	if err != nil {
		return "", false
	}
	pbx.mu.Lock()
	password, ok := pbx.passwords[cred.Username]
	pbx.mu.Unlock()
	if !ok {
		return cred.Username, false
	}
	want, err := digest.Digest(&pbxChallenge, digest.Options{
		Method: req.Method.String(), URI: cred.URI, Username: cred.Username, Password: password,
	})
	return cred.Username, err == nil && want.Response == cred.Response
}

func (pbx *fakePBX) challenge(req *sip.Request, tx sip.ServerTransaction) {
	res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
	res.AppendHeader(sip.NewHeader("WWW-Authenticate", pbxChallenge.String()))
	_ = tx.Respond(res)
}

func (pbx *fakePBX) onRegister(req *sip.Request, tx sip.ServerTransaction) {
	asked := 3600
	if h := req.GetHeader("Expires"); h != nil {
		asked, _ = strconv.Atoi(h.Value())
	}
	rec := pbxRegister{
		Expires: asked, CallID: req.CallID().Value(), CSeq: req.CSeq().SeqNo,
		Transport: strings.ToLower(req.Transport()), At: time.Now(),
	}
	if c := req.Contact(); c != nil {
		rec.Contact = c.Address.String()
	}
	user, ok := pbx.authorized(req)
	rec.AuthUser = user
	defer func() {
		pbx.mu.Lock()
		pbx.registers = append(pbx.registers, rec)
		pbx.mu.Unlock()
	}()
	if !ok {
		rec.Status = sip.StatusUnauthorized
		pbx.challenge(req, tx)
		return
	}
	pbx.mu.Lock()
	rec.Granted = asked
	if pbx.maxGrant > 0 && asked > pbx.maxGrant {
		rec.Granted = pbx.maxGrant
	}
	if c := req.Contact(); c != nil && asked > 0 {
		pbx.contacts[req.To().Address.User] = c.Address
	}
	pbx.mu.Unlock()
	rec.Status = sip.StatusOK
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	if c := req.Contact(); c != nil && asked > 0 {
		granted := c.Clone()
		granted.Params = sip.NewParams()
		granted.Params.Add("expires", strconv.Itoa(rec.Granted))
		res.AppendHeader(granted)
	}
	_ = tx.Respond(res)
}

// onInvite answers the phone's call after a digest challenge, with an RTP
// peer that echoes or sends answerHz, and holds the dialog until it ends.
func (pbx *fakePBX) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	user, ok := pbx.authorized(req)
	if !ok {
		pbx.challenge(req, tx)
		return
	}
	pbx.mu.Lock()
	pbx.invites = append(pbx.invites, pbxInvite{Number: req.Recipient.User, From: req.From().Address.User, AuthUser: user})
	hz := pbx.answerHz
	pbx.mu.Unlock()
	d, err := pbx.uas.ReadInvite(req, tx)
	if err != nil {
		pbx.t.Errorf("fake PBX: %v", err)
		return
	}
	defer func() { _ = d.Close() }()
	rtpAddr := tonePeer(pbx.t, hz)
	answer := buildOffer(rtpAddr.Addr(), int(rtpAddr.Port()), 1)
	if err := d.RespondSDP(answer); err != nil {
		pbx.t.Logf("fake PBX: answering: %v", err)
		return
	}
	<-d.Context().Done()
}

// onRefer accepts an in-dialog REFER (after a challenge, if
// challengeRefer) and reports the transfer with a sipfrag NOTIFY.
func (pbx *fakePBX) onRefer(req *sip.Request, tx sip.ServerTransaction) {
	_, authorized := pbx.authorized(req)
	pbx.mu.Lock()
	challenge := pbx.challengeRefer && !authorized
	pbx.mu.Unlock()
	if challenge {
		pbx.challenge(req, tx)
		return
	}
	rec := pbxRefer{CallID: req.CallID().Value(), Authorized: authorized}
	if h := req.GetHeader("Refer-To"); h != nil {
		rec.ReferTo = h.Value()
	}
	if h := req.GetHeader("Referred-By"); h != nil {
		rec.ReferredBy = h.Value()
	}
	pbx.mu.Lock()
	pbx.refers = append(pbx.refers, rec)
	pbx.mu.Unlock()
	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusAccepted, "Accepted", nil))

	var (
		do     func(context.Context, *sip.Request) (*sip.Response, error)
		target sip.Uri
	)
	if d, err := pbx.uas.MatchDialogRequest(req); err == nil {
		do, target = d.Do, d.InviteRequest.Contact().Address
	} else if d, err := pbx.uac.MatchRequestDialog(req); err == nil {
		do, target = d.Do, d.InviteResponse.Contact().Address
	} else {
		pbx.t.Errorf("fake PBX: REFER outside a dialog")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, frag := range []string{"SIP/2.0 100 Trying", "SIP/2.0 200 OK"} {
			n := sip.NewRequest(sip.NOTIFY, target)
			n.AppendHeader(sip.NewHeader("Event", "refer"))
			state := "active;expires=60"
			if strings.Contains(frag, "200") {
				state = "terminated;reason=noresource"
			}
			n.AppendHeader(sip.NewHeader("Subscription-State", state))
			ct := sip.ContentTypeHeader("message/sipfrag;version=2.0")
			n.AppendHeader(&ct)
			n.SetBody([]byte(frag + "\r\n"))
			if _, err := do(ctx, n); err != nil {
				pbx.t.Logf("fake PBX: refer NOTIFY: %v", err)
			}
		}
	}()
}

// tonePeer is an RTP peer on 127.0.0.1 that answers every packet it
// receives with one 20 ms frame of a continuous hz tone, or echoes the
// packet when hz is 0.
func tonePeer(t *testing.T, hz float64) netip.AddrPort {
	t.Helper()
	if hz == 0 {
		return peer(t, true)
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		gen := &toneGenerator{freq: hz, amplitude: 0.5}
		buf := make([]byte, 1500)
		var seq uint16
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			var in rtp.Packet
			if in.Unmarshal(buf[:n]) != nil {
				continue
			}
			seq++
			out := rtp.Packet{
				Header:  rtp.Header{Version: 2, PayloadType: payloadTypePCMU, SequenceNumber: seq, Timestamp: uint32(seq) * samplesPerFrame, SSRC: 7},
				Payload: gen.nextFrame(),
			}
			b, err := out.Marshal()
			if err != nil {
				continue
			}
			_, _ = c.WriteToUDPAddrPort(b, from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}
