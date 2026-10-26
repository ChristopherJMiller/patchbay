package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
	"github.com/pion/rtp/v2"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

const (
	// byeTimeout bounds a BYE and a REFER.
	byeTimeout = 5 * time.Second
	// referNotifyWait is how long a transferring phone waits for the
	// REFER's final NOTIFY before it leaves the call anyway.
	referNotifyWait = 10 * time.Second
)

// dialogSession is what a call needs of sipgo's client (placed) and server
// (answered) dialog sessions.
type dialogSession interface {
	Bye(ctx context.Context) error
	Do(ctx context.Context, req *sip.Request) (*sip.Response, error)
	Context() context.Context
}

// callEntry is one call; rec is guarded by runner.mu.
type callEntry struct {
	rec script.CallRecord

	// Set once answered: the dialog, the remote target in-dialog requests
	// go to, the audio, and what the phone's SDP said (for a re-INVITE).
	dialog   dialogSession
	target   sip.Uri
	media    *mediaSession
	localSDP []byte

	// hungUp closes once the phone has sent its BYE (or given up on it);
	// ended once the call is over and recorded.
	hungUp   chan struct{}
	hangOnce sync.Once
	ended    chan struct{}
}

// recordLocked is the call's record with its audio counts so far; r.mu is
// held.
func (c *callEntry) recordLocked() script.CallRecord {
	rec := c.rec
	rec.ToneFrames = cloneFrames(rec.ToneFrames)
	if c.media != nil && rec.End.IsZero() {
		rec.Received, rec.ToneFrames = c.media.counts()
	}
	return rec
}

// up is whether the call is answered and not over; r.mu is held.
func (c *callEntry) up() bool { return c.rec.Answered && c.rec.End.IsZero() }

func (r *runner) addCall(rec script.CallRecord) *callEntry {
	c := &callEntry{rec: rec, hungUp: make(chan struct{}), ended: make(chan struct{})}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.changedLocked()
	r.mu.Unlock()
	return c
}

// answered records that c is up on dialog d.
func (r *runner) answered(c *callEntry, d dialogSession, target sip.Uri, m *mediaSession, localSDP []byte) {
	r.mu.Lock()
	c.rec.Status, c.rec.Answered = sip.StatusOK, true
	c.dialog, c.target, c.media, c.localSDP = d, target, m, localSDP
	rec := c.recordLocked()
	r.changedLocked()
	r.mu.Unlock()
	r.log.Info("call", "phase", "answered", "record", rec)
}

// finishCall records the end of c (status is the INVITE's final status
// when it never came up) and closes ended.
func (r *runner) finishCall(c *callEntry, status int, cause error) {
	r.mu.Lock()
	if c.media != nil {
		c.rec.Received, c.rec.ToneFrames = c.media.counts()
	}
	if !c.rec.Answered && status != 0 {
		c.rec.Status = status
	}
	c.rec.End = time.Now()
	rec := c.rec
	r.changedLocked()
	r.mu.Unlock()
	if cause != nil {
		r.log.Info("call", "phase", "ended", "record", rec, "error", cause.Error())
	} else {
		r.log.Info("call", "phase", "ended", "record", rec)
	}
	close(c.ended)
}

// watchCall waits for an answered call to end (either side's BYE, or the
// phone giving up on its own BYE), then stops its audio and records it.
func (r *runner) watchCall(c *callEntry) {
	select {
	case <-c.dialog.Context().Done():
	case <-c.hungUp:
	case <-r.ctx.Done():
	}
	c.media.close()
	r.finishCall(c, 0, nil)
}

// bye hangs up c unless it is already over.
func (r *runner) bye(ctx context.Context, c *callEntry) error {
	defer c.hangOnce.Do(func() { close(c.hungUp) })
	select {
	case <-c.dialog.Context().Done():
		return nil
	default:
	}
	bctx, cancel := context.WithTimeout(ctx, byeTimeout)
	defer cancel()
	if err := c.dialog.Bye(bctx); err != nil {
		if c.dialog.Context().Err() != nil {
			return nil // the far end hung up first (its BYE crossed ours)
		}
		return fmt.Errorf("BYE: %w", err)
	}
	return nil
}

// placeCall is a Call step.
func (r *runner) placeCall(ctx context.Context, st *script.Call) error {
	if r.currentConfig() == nil {
		return errors.New("no config has been read: a call needs a fetchConfig first")
	}
	conn, err := r.listenRTP()
	if err != nil {
		return err
	}
	c := r.addCall(script.CallRecord{Direction: "out", Number: st.Number, Start: time.Now()})
	cl, err := r.phone.invite(ctx, st.Number, conn.LocalAddr().(*net.UDPAddr).Port)
	if err != nil {
		_ = conn.Close()
		r.finishCall(c, dialogStatus(err), err)
		return fmt.Errorf("call %s: %w", st.Number, err)
	}
	var detect []float64
	if st.ExpectToneHz != 0 {
		detect = []float64{st.ExpectToneHz}
	}
	r.mu.Lock()
	c.rec.CallID = cl.callID
	r.mu.Unlock()
	m := startMedia(conn, cl.remote.Addr, r.toneHz(), detect)
	localSDP := buildAnswerSDP(r.phone.localIP, conn.LocalAddr().(*net.UDPAddr).Port, cl.remote)
	r.answered(c, cl.dialog, cl.dialog.InviteResponse.Contact().Address, m, localSDP)
	go func() {
		r.watchCall(c)
		_ = cl.dialog.Close()
	}()
	if st.Open {
		return nil
	}

	select {
	case <-time.After(st.Hold.D()):
	case <-c.ended:
	case <-ctx.Done():
		return ctx.Err()
	}
	byeErr := r.bye(ctx, c)
	select {
	case <-c.ended:
	case <-ctx.Done():
		return ctx.Err()
	}
	if byeErr != nil {
		return byeErr
	}
	if st.ExpectToneHz == 0 {
		return nil
	}
	r.mu.Lock()
	got := c.rec.ToneFrames[script.HzKey(st.ExpectToneHz)]
	r.mu.Unlock()
	if want := max(st.MinToneFrames, 1); got < want {
		return fmt.Errorf("the %g Hz tone came in %d packets, want at least %d", st.ExpectToneHz, got, want)
	}
	return nil
}

// listenRTP opens a UDP port for a call's audio on the phone's address.
func (r *runner) listenRTP() (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(r.phone.localIP, 0)))
	if err != nil {
		return nil, fmt.Errorf("RTP socket: %w", err)
	}
	return conn, nil
}

// onInvite answers an incoming call as the script's Answer says (no
// Answer: 486), recording the caller's identity as the INVITE carried it,
// and holds the INVITE's goroutine until the call is over.
func (r *runner) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	if to := req.To(); to != nil && to.Params != nil && to.Params.Has("tag") {
		r.onReinvite(req, tx)
		return
	}
	rec := script.CallRecord{Direction: "in", CallID: req.CallID().Value(), Start: time.Now()}
	if from := req.From(); from != nil {
		rec.Number, rec.FromDisplay = from.Address.User, from.DisplayName
	}
	if h := req.GetHeader("Remote-Party-ID"); h != nil {
		rec.RemotePartyID = h.Value()
	}
	if h := req.GetHeader("P-Asserted-Identity"); h != nil {
		rec.PAssertedIdentity = h.Value()
	}
	c := r.addCall(rec)
	r.log.Info("call", "phase", "ringing", "record", rec)
	ans := r.sc.Answer
	if ans == nil {
		r.phone.respond(req, tx, sip.StatusBusyHere, "Busy Here")
		r.finishCall(c, sip.StatusBusyHere, nil)
		return
	}
	d, err := r.phone.uas.ReadInvite(req, tx)
	if err != nil {
		r.phone.respond(req, tx, sip.StatusInternalServerError, "Server Internal Error")
		r.finishCall(c, sip.StatusInternalServerError, err)
		return
	}
	defer func() { _ = d.Close() }()
	contact := r.phone.contactFor(r.phone.account())
	offer, err := parseAnswer(req.Body())
	if err != nil {
		_ = d.Respond(sip.StatusNotAcceptableHere, "Not Acceptable Here", nil, &contact)
		r.finishCall(c, sip.StatusNotAcceptableHere, err)
		return
	}
	_ = d.Respond(sip.StatusRinging, "Ringing", nil, &contact, r.phone.userAgent())
	after := ans.After.D()
	if after == 0 {
		after = script.DefaultAnswerAfter
	}
	select {
	case <-time.After(after):
	case <-d.Context().Done(): // the caller gave up: CANCEL, 487
		r.finishCall(c, sip.StatusRequestTerminated, nil)
		return
	case <-r.ctx.Done():
		_ = d.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil, &contact)
		r.finishCall(c, sip.StatusTemporarilyUnavailable, nil)
		return
	}
	if ans.Status != 0 {
		_ = d.Respond(ans.Status, reasonPhrase(ans.Status), nil, &contact)
		r.finishCall(c, ans.Status, nil)
		return
	}

	conn, err := r.listenRTP()
	if err != nil {
		_ = d.Respond(sip.StatusInternalServerError, "Server Internal Error", nil, &contact)
		r.finishCall(c, sip.StatusInternalServerError, err)
		return
	}
	sdp := buildAnswerSDP(r.phone.localIP, conn.LocalAddr().(*net.UDPAddr).Port, offer)
	res := sip.NewSDPResponseFromRequest(d.InviteRequest, sdp)
	res.AppendHeader(&contact)
	res.AppendHeader(r.phone.userAgent())
	m := startMedia(conn, offer.Addr, r.toneHz(), ans.DetectHz)
	r.mu.Lock()
	c.media, c.localSDP = m, sdp // a re-INVITE may come as soon as the ACK
	r.mu.Unlock()
	if err := d.WriteResponse(res); err != nil { // returns once the ACK came
		m.close()
		r.finishCall(c, sip.StatusOK, fmt.Errorf("answering: %w", err))
		return
	}
	target := sip.Uri{}
	if h := d.InviteRequest.Contact(); h != nil {
		target = h.Address
	}
	r.answered(c, d, target, m, sdp)
	if ans.Hold > 0 {
		go func() {
			select {
			case <-time.After(ans.Hold.D()):
				if err := r.bye(r.ctx, c); err != nil {
					r.log.Warn("call", "phase", "hangup", "error", err.Error())
				}
			case <-c.ended:
			}
		}()
	}
	r.watchCall(c)
}

// onReinvite answers a re-INVITE of a call that is up with the same
// audio, sent to wherever the new offer says.
func (r *runner) onReinvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()
	var (
		sdp   []byte
		media *mediaSession
	)
	r.mu.Lock()
	for _, e := range r.calls {
		if e.rec.CallID == callID && e.localSDP != nil && e.rec.End.IsZero() {
			sdp, media = e.localSDP, e.media
		}
	}
	r.mu.Unlock()
	if sdp == nil {
		r.phone.respond(req, tx, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist")
		return
	}
	if offer, err := parseAnswer(req.Body()); err == nil {
		media.setRemote(offer.Addr)
	}
	res := sip.NewSDPResponseFromRequest(req, sdp)
	contact := r.phone.contactFor(r.phone.account())
	res.AppendHeader(&contact)
	if err := tx.Respond(res); err != nil {
		r.log.Warn("re-INVITE", "error", err.Error())
	}
}

// hangupAll is a Hangup step: BYE every call that is up, and wait for each
// to be over.
func (r *runner) hangupAll(ctx context.Context) error {
	r.mu.Lock()
	var up []*callEntry
	for _, c := range r.calls {
		if c.up() {
			up = append(up, c)
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, c := range up {
		errs = append(errs, r.bye(ctx, c))
	}
	for _, c := range up {
		select {
		case <-c.ended:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.Join(errs...)
}

// transfer is a Transfer step: a blind transfer of the latest answered
// call that is up, by an in-dialog REFER to sip:<to>@<the PBX> (answering
// a digest challenge), which must be accepted with 202. The phone then
// leaves the call: once the REFER's NOTIFY reports a final status, or the
// far end hangs up, or referNotifyWait passes.
func (r *runner) transfer(ctx context.Context, to string) error {
	r.mu.Lock()
	var c *callEntry
	for _, e := range r.calls {
		if e.up() {
			c = e
		}
	}
	r.mu.Unlock()
	if c == nil {
		return errors.New("no call is up to transfer")
	}
	acc := r.phone.account()
	referTo := sip.Uri{Scheme: "sip", User: to, Host: acc.Server.Addr().String()}
	referredBy := aor(acc)
	refer := func() *sip.Request {
		req := sip.NewRequest(sip.REFER, c.target)
		req.AppendHeader(sip.NewHeader("Refer-To", "<"+referTo.String()+">"))
		req.AppendHeader(sip.NewHeader("Referred-By", "<"+referredBy.String()+">"))
		contact := r.phone.contactFor(acc)
		req.AppendHeader(&contact)
		req.AppendHeader(r.phone.userAgent())
		return req
	}
	tctx, cancel := context.WithTimeout(ctx, byeTimeout)
	defer cancel()
	res, err := c.dialog.Do(tctx, refer())
	if err == nil && (res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired) {
		req := refer()
		if err = applyDigest(req, res, acc); err == nil {
			res, err = c.dialog.Do(tctx, req)
		}
	}
	status := 0
	if err == nil {
		status = res.StatusCode
	}
	r.mu.Lock()
	c.rec.TransferredTo, c.rec.TransferStatus = to, status
	callID := c.rec.CallID
	r.changedLocked()
	r.mu.Unlock()
	r.log.Info("transfer", "to", to, "callID", callID, "status", status)
	if err != nil {
		return fmt.Errorf("REFER: %w", err)
	}
	if status != sip.StatusAccepted {
		return fmt.Errorf("REFER answered %d, want 202", status)
	}

	_ = r.waitUntil(ctx, referNotifyWait, "final NOTIFY for the REFER", func() bool {
		if !c.rec.End.IsZero() {
			return true
		}
		for _, n := range r.notifies {
			if n.callID == callID && n.rec.Event == "refer" && n.done && sipfragStatus(n.rec.Body) >= 200 {
				return true
			}
		}
		return false
	})
	if err := r.bye(ctx, c); err != nil {
		// The transfer is done; a BYE the PBX did not take changes nothing.
		r.log.Warn("transfer", "phase", "leaving", "error", err.Error())
	}
	select {
	case <-c.ended:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitCall is a WaitCall step.
func (r *runner) waitCall(ctx context.Context, w *script.WaitCall) error {
	r.mu.Lock()
	from := len(r.calls)
	r.mu.Unlock()
	what := "answered incoming call"
	if w.Ended {
		what = "incoming call answered and ended"
	}
	return r.waitUntil(ctx, w.Within.D(), what, func() bool {
		for _, c := range r.calls[from:] {
			if c.rec.Direction == "in" && c.rec.Answered && (!w.Ended || !c.rec.End.IsZero()) {
				return true
			}
		}
		return false
	})
}

// applyDigest adds the credentials a 401 or 407 asked for to req.
func applyDigest(req *sip.Request, res *sip.Response, acc account) error {
	challengeHeader, authHeader := "WWW-Authenticate", "Authorization"
	if res.StatusCode == sip.StatusProxyAuthRequired {
		challengeHeader, authHeader = "Proxy-Authenticate", "Proxy-Authorization"
	}
	h := res.GetHeader(challengeHeader)
	if h == nil {
		return fmt.Errorf("a %d without %s", res.StatusCode, challengeHeader)
	}
	chal, err := digest.ParseChallenge(h.Value())
	if err != nil {
		return fmt.Errorf("parsing the challenge: %w", err)
	}
	chal.Algorithm = strings.ToUpper(chal.Algorithm)
	cred, err := digest.Digest(chal, digest.Options{
		Method: req.Method.String(), URI: req.Recipient.Addr(),
		Username: acc.authUser(), Password: acc.Password.reveal(),
	})
	if err != nil {
		return fmt.Errorf("answering the challenge: %w", err)
	}
	req.AppendHeader(sip.NewHeader(authHeader, cred.String()))
	return nil
}

// dialogStatus is the final status of a failed INVITE, or 0.
func dialogStatus(err error) int {
	var de *sipgo.ErrDialogResponse
	if errors.As(err, &de) && de.Res != nil {
		return de.Res.StatusCode
	}
	return 0
}

// reasonPhrase is the reason phrase for a final failure status.
func reasonPhrase(status int) string {
	switch status {
	case sip.StatusBusyHere:
		return "Busy Here"
	case sip.StatusTemporarilyUnavailable:
		return "Temporarily Unavailable"
	case sip.StatusNotFound:
		return "Not Found"
	case sip.StatusGlobalDecline:
		return "Decline"
	}
	return "Rejected"
}

// buildAnswerSDP answers an offer with PCMU (and telephone-events, if
// offered on 101), 20 ms packets.
func buildAnswerSDP(ip netip.Addr, port int, offer remoteMedia) []byte {
	pts := strconv.Itoa(payloadTypePCMU)
	events := false
	for _, pt := range offer.PayloadTypes {
		if pt == 101 {
			events = true
		}
	}
	if events {
		pts += " 101"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "v=0\r\no=fakephone 1 1 IN IP4 %s\r\ns=fakephone\r\nc=IN IP4 %s\r\nt=0 0\r\n", ip, ip)
	fmt.Fprintf(&b, "m=audio %d RTP/AVP %s\r\n", port, pts)
	fmt.Fprintf(&b, "a=rtpmap:%d PCMU/%d\r\n", payloadTypePCMU, sampleRate)
	if events {
		fmt.Fprintf(&b, "a=rtpmap:101 telephone-event/%d\r\na=fmtp:101 0-15\r\n", sampleRate)
	}
	fmt.Fprintf(&b, "a=ptime:%d\r\na=sendrecv\r\n", frameDuration)
	return []byte(b.String())
}

// mediaSession is one call's audio, for as long as the call is up: a
// continuous tone out every 20 ms, and a count of what comes in (packets,
// and those carrying each detect frequency).
type mediaSession struct {
	conn   *net.UDPConn
	detect []float64

	mu       sync.Mutex
	remote   netip.AddrPort
	received int
	frames   map[string]int

	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func startMedia(conn *net.UDPConn, remote netip.AddrPort, toneHz float64, detect []float64) *mediaSession {
	m := &mediaSession{conn: conn, detect: detect, remote: remote, stop: make(chan struct{})}
	if len(detect) > 0 {
		m.frames = make(map[string]int, len(detect))
		for _, hz := range detect {
			m.frames[script.HzKey(hz)] = 0
		}
	}
	m.wg.Add(2)
	go m.send(toneHz)
	go m.receive()
	return m
}

func (m *mediaSession) setRemote(a netip.AddrPort) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remote = a
}

// counts is what has come in so far.
func (m *mediaSession) counts() (int, map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.received, cloneFrames(m.frames)
}

// close stops the audio and frees the port; safe to call twice.
func (m *mediaSession) close() {
	m.closeOnce.Do(func() {
		close(m.stop)
		_ = m.conn.Close()
		m.wg.Wait()
	})
}

func (m *mediaSession) send(toneHz float64) {
	defer m.wg.Done()
	ssrc, seq, ts, err := rtpStart()
	if err != nil {
		return
	}
	gen := &toneGenerator{freq: toneHz, amplitude: 0.5}
	ticker := time.NewTicker(frameDuration * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 12+samplesPerFrame)
	for first := true; ; first = false {
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version: 2, Marker: first, PayloadType: payloadTypePCMU,
				SequenceNumber: seq, Timestamp: ts, SSRC: ssrc,
			},
			Payload: gen.nextFrame(),
		}
		if n, err := pkt.MarshalTo(buf); err == nil {
			m.mu.Lock()
			dst := m.remote
			m.mu.Unlock()
			_, _ = m.conn.WriteToUDPAddrPort(buf[:n], dst)
		}
		seq++
		ts += samplesPerFrame
		select {
		case <-m.stop:
			return
		case <-ticker.C:
		}
	}
}

func (m *mediaSession) receive() {
	defer m.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, _, err := m.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return // closed
		}
		var pkt rtp.Packet
		if pkt.Unmarshal(buf[:n]) != nil {
			continue
		}
		m.mu.Lock()
		m.received++
		if pkt.PayloadType == payloadTypePCMU {
			for _, hz := range m.detect {
				if analyseULaw(pkt.Payload, hz).toneDetected() {
					m.frames[script.HzKey(hz)]++
				}
			}
		}
		m.mu.Unlock()
	}
}
