package main

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// registeredDesk runs a cisco phone that fetches its config, registers and
// then runs steps, answering calls as answer says; it returns once the
// phone is registered.
func registeredDesk(t *testing.T, password string, answer *script.Answer, steps ...script.Step) (*fakePBX, *runHarness) {
	t.Helper()
	pbx, _, opts := deskPhone(t, password)
	sc := ciscoScript(append([]script.Step{
		{Label: "config", FetchConfig: &script.FetchConfig{}},
		{Label: "register", Register: &script.Register{}},
	}, steps...)...)
	sc.Answer = answer
	sc.ToneHz = 600
	h := startRun(t, sc, opts)
	h.waitState(10*time.Second, "registered", func(s script.State) bool { return s.Registered })
	pbx.contact()
	return pbx, h
}

// TestAnswerRecordsCallerID: an incoming call rings for Answer.After, is
// answered with a PCMU SDP, sends ToneHz, counts each DetectHz in what
// comes in, and records the caller's identity as the INVITE carried it.
func TestAnswerRecordsCallerID(t *testing.T) {
	pbx, h := registeredDesk(t, "not-a-secret-answer",
		&script.Answer{After: dur(200 * time.Millisecond), DetectHz: []float64{800, 1000}},
		script.Step{Label: "wait", WaitCall: &script.WaitCall{Ended: true, Within: dur(10 * time.Second)}})
	rpid := `"Laptop" <sip:150@127.0.0.1>;party=calling;privacy=off;screen=no`
	pai := `"Laptop" <sip:150@127.0.0.1>`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	d, err := pbx.call(ctx, "150", "Laptop", 800,
		sip.NewHeader("Remote-Party-ID", rpid), sip.NewHeader("P-Asserted-Identity", pai))
	if err != nil {
		t.Fatal(err)
	}
	if rang := time.Since(start); rang < 150*time.Millisecond {
		t.Errorf("answered after %s, want after Answer.After (200ms)", rang)
	}
	answer, err := parseAnswer(d.InviteResponse.Body())
	if err != nil || len(answer.PayloadTypes) == 0 || answer.PayloadTypes[0] != payloadTypePCMU {
		t.Fatalf("answer SDP %+v %v\n%s", answer, err, d.InviteResponse.Body())
	}
	time.Sleep(time.Second)
	if err := d.Bye(ctx); err != nil {
		t.Fatal(err)
	}
	s := h.atStep("")
	if len(s.Calls) != 1 {
		t.Fatalf("calls %+v", s.Calls)
	}
	c := s.Calls[0]
	if c.Direction != "in" || c.Number != "150" || c.FromDisplay != "Laptop" || c.RemotePartyID != rpid ||
		c.PAssertedIdentity != pai || c.Status != 200 || !c.Answered || c.End.IsZero() || c.CallID == "" {
		t.Fatalf("call record %+v", c)
	}
	if c.Received < 30 || c.ToneFrames["800"] < 30 || c.ToneFrames["1000"] != 0 {
		t.Errorf("received %d, tone frames %v: want the far end's 800 Hz and none of 1000", c.Received, c.ToneFrames)
	}
}

// TestAnswerRejectsAndCancels: no Answer is 486, Answer.Status that
// status, a CANCEL while ringing 487; none of them is answered. With
// Answer.Hold the phone hangs up itself.
func TestAnswerRejectsAndCancels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, tc := range map[string]struct {
		answer *script.Answer
		wait   time.Duration // how long the PBX lets it ring
		status int
	}{
		"busy":      {nil, 0, 486},
		"status":    {&script.Answer{After: dur(10 * time.Millisecond), Status: 480}, 0, 480},
		"cancelled": {&script.Answer{After: dur(5 * time.Second)}, 300 * time.Millisecond, 487},
	} {
		t.Run(name, func(t *testing.T) {
			pbx, h := registeredDesk(t, "not-a-secret-reject", tc.answer)
			callCtx := ctx
			if tc.wait > 0 {
				var c context.CancelFunc
				callCtx, c = context.WithTimeout(ctx, tc.wait)
				defer c()
			}
			_, err := pbx.call(callCtx, "101", "Kitchen", 0)
			if err == nil {
				t.Fatal("the call was answered")
			}
			if got := dialogStatus(err); tc.wait == 0 && got != tc.status {
				t.Fatalf("the caller got %d (%v), want %d", got, err, tc.status)
			}
			s := h.waitState(5*time.Second, "the call over", func(s script.State) bool {
				return len(s.Calls) == 1 && !s.Calls[0].End.IsZero()
			})
			if c := s.Calls[0]; c.Status != tc.status || c.Answered || c.Number != "101" {
				t.Fatalf("call record %+v, want status %d", c, tc.status)
			}
		})
	}

	t.Run("hold", func(t *testing.T) {
		pbx, h := registeredDesk(t, "not-a-secret-hold", &script.Answer{After: dur(10 * time.Millisecond), Hold: dur(300 * time.Millisecond)})
		d, err := pbx.call(ctx, "101", "Kitchen", 0)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-d.Context().Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the phone did not hang up after Answer.Hold")
		}
		s := h.waitState(5*time.Second, "the call over", func(s script.State) bool {
			return len(s.Calls) == 1 && !s.Calls[0].End.IsZero()
		})
		if c := s.Calls[0]; !c.Answered || c.End.Sub(c.Start) > 3*time.Second {
			t.Fatalf("call record %+v", c)
		}
	})
}

// TestTransferSendsRefer: in an answered call, transfer sends an in-dialog
// REFER (Refer-To sip:150@<the PBX>) through the dialog, answers the
// PBX's digest challenge, takes the 202, answers the refer NOTIFYs and
// leaves the call.
func TestTransferSendsRefer(t *testing.T) {
	pbx, h := registeredDesk(t, "not-a-secret-transfer", &script.Answer{After: dur(10 * time.Millisecond)},
		script.Step{Label: "wait", WaitCall: &script.WaitCall{Within: dur(10 * time.Second)}},
		script.Step{Label: "transfer", Transfer: &script.Transfer{To: "150"}},
	)
	pbx.mu.Lock()
	pbx.challengeRefer = true
	pbx.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := pbx.call(ctx, "101", "Kitchen", 1000)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.Context().Done(): // the phone left the call
	case <-time.After(10 * time.Second):
		t.Fatal("the transferring phone stayed in the call")
	}
	s := h.atStep("")

	// The fake counts the BYE after answering it, and the dialog ends as it
	// answers: the count may still be on its way.
	waitFor(t, 5*time.Second, "the phone's BYE at the PBX", func() bool {
		pbx.mu.Lock()
		defer pbx.mu.Unlock()
		return pbx.byes >= 1
	})
	pbx.mu.Lock()
	refers, byes := append([]pbxRefer(nil), pbx.refers...), pbx.byes
	pbx.mu.Unlock()
	if len(refers) != 1 || refers[0].ReferTo != "<sip:150@127.0.0.1>" || !refers[0].Authorized ||
		refers[0].ReferredBy != "<sip:100@127.0.0.1>" || refers[0].CallID != d.InviteRequest.CallID().Value() {
		t.Fatalf("REFERs the PBX accepted: %+v", refers)
	}
	if byes != 1 {
		t.Errorf("the PBX saw %d BYEs, want the phone's one", byes)
	}
	c := s.Calls[0]
	if c.TransferredTo != "150" || c.TransferStatus != 202 || c.End.IsZero() {
		t.Fatalf("call record %+v", c)
	}
	var frags []string
	for _, n := range s.Notifies {
		if n.Event == "refer" {
			frags = append(frags, strings.TrimSpace(n.Body))
		}
	}
	if len(frags) != 2 || frags[1] != "SIP/2.0 200 OK" {
		t.Errorf("refer NOTIFYs %q", frags)
	}
}

// TestRunCalls: a placed call held with the echo's tone expected passes;
// an open call stays up until hangup; a transfer works from a placed call
// too; a tone that does not come, and a call that is refused, fail.
func TestRunCalls(t *testing.T) {
	_, h := registeredDesk(t, "not-a-secret-calls", nil,
		script.Step{Label: "echo", Call: &script.Call{Number: "600", Hold: dur(800 * time.Millisecond), ExpectToneHz: 600, MinToneFrames: 20}},
		script.Step{Label: "open", Call: &script.Call{Number: "600", Open: true}},
		script.Step{Label: "talk", WaitSignal: &script.WaitSignal{Name: "hangup"}},
		script.Step{Label: "hangup", Hangup: &script.Hangup{}},
		script.Step{Label: "again", Call: &script.Call{Number: "601", Open: true}},
		script.Step{Label: "transfer", Transfer: &script.Transfer{To: "150"}},
	)
	s := h.atStep("talk")
	if len(s.Calls) != 2 || !s.Calls[1].Answered || !s.Calls[1].End.IsZero() {
		t.Fatalf("calls %+v", s.Calls)
	}
	if c := s.Calls[0]; c.Direction != "out" || c.Number != "600" || c.Status != 200 || c.End.IsZero() || c.ToneFrames["600"] < 20 {
		t.Fatalf("held call %+v", c)
	}
	time.Sleep(300 * time.Millisecond)
	if s := h.state(); s.Calls[1].Received == 0 || !s.Calls[1].End.IsZero() {
		t.Fatalf("open call while up %+v", s.Calls[1])
	}
	h.signal("hangup")
	s = h.atStep("")
	if c := s.Calls[1]; c.End.IsZero() {
		t.Fatalf("open call after hangup %+v", c)
	}
	if c := s.Calls[2]; c.TransferredTo != "150" || c.TransferStatus != 202 || c.End.IsZero() {
		t.Fatalf("transferred call %+v", c)
	}

	for name, tc := range map[string]struct {
		step script.Step
		want string
	}{
		"no tone":  {script.Step{Label: "x", Call: &script.Call{Number: "600", Hold: dur(500 * time.Millisecond), ExpectToneHz: 800}}, "the 800 Hz tone came in 0 packets"},
		"refused":  {script.Step{Label: "x", Call: &script.Call{Number: "600", Hold: dur(500 * time.Millisecond)}}, "401"},
		"no call":  {script.Step{Label: "x", Transfer: &script.Transfer{To: "150"}}, "no call is up"},
		"hangup 0": {script.Step{Label: "x", Hangup: &script.Hangup{}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			pbx, _, opts := deskPhone(t, "not-a-secret-calls")
			if name == "refused" {
				pbx.setDeskPassword("not-a-secret-other")
			}
			sc := ciscoScript(script.Step{Label: "config", FetchConfig: &script.FetchConfig{}}, tc.step)
			sc.ExitWhenDone = true
			h := startRun(t, sc, opts)
			err := h.wait(10 * time.Second)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run: %v, want %q", err, tc.want)
			}
			if name == "refused" {
				if c := h.r.snapshot().Calls; len(c) != 1 || c[0].Status != 401 || c[0].Answered || c[0].End.IsZero() {
					t.Fatalf("refused call %+v", c)
				}
			}
		})
	}
}

// TestReinvite: a re-INVITE of a call that is up gets the same audio back;
// one for no call gets 481.
func TestReinvite(t *testing.T) {
	pbx, h := registeredDesk(t, "not-a-secret-reinvite", &script.Answer{After: dur(10 * time.Millisecond)})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := pbx.call(ctx, "101", "Kitchen", 0)
	if err != nil {
		t.Fatal(err)
	}
	first := string(d.InviteResponse.Body())
	moved := tonePeer(t, 1000)
	req := sip.NewRequest(sip.INVITE, d.InviteResponse.Contact().Address)
	ct := sip.ContentTypeHeader("application/sdp")
	req.AppendHeader(&ct)
	req.SetBody(buildOffer(moved.Addr(), int(moved.Port()), 8))
	res, err := d.Do(ctx, req)
	if err != nil || res.StatusCode != sip.StatusOK || string(res.Body()) != first {
		t.Fatalf("re-INVITE: %v %v\n%s", res, err, res.Body())
	}
	if err := d.WriteRequest(sip.NewRequest(sip.ACK, req.Recipient)); err != nil {
		t.Fatal(err)
	}
	_ = d.Bye(ctx)
	h.waitState(5*time.Second, "the call over", func(s script.State) bool { return len(s.Calls) == 1 && !s.Calls[0].End.IsZero() })

	// Outside any call.
	stray := sip.NewRequest(sip.INVITE, pbx.contact())
	stray.AppendHeader(&sip.ToHeader{Address: pbx.contact(), Params: sip.HeaderParams{{K: "tag", V: "x"}}})
	stray.SetTransport("TCP")
	res, err = pbx.client.Do(ctx, stray)
	if err != nil || res.StatusCode != sip.StatusCallTransactionDoesNotExists {
		t.Fatalf("stray re-INVITE: %v %v", res, err)
	}
}

func TestBuildAnswerSDP(t *testing.T) {
	ip := netip.MustParseAddr("192.0.2.1")
	for _, offered := range [][]int{{0, 101}, {9, 0}} {
		got, err := parseAnswer(buildAnswerSDP(ip, 4000, remoteMedia{PayloadTypes: offered}))
		if err != nil || got.Addr != netip.AddrPortFrom(ip, 4000) || got.PayloadTypes[0] != 0 {
			t.Fatalf("offer %v: %+v %v", offered, got, err)
		}
		if events := len(got.PayloadTypes) == 2; events != (offered[1] == 101) {
			t.Errorf("offer %v: answered %v", offered, got.PayloadTypes)
		}
	}
	if reasonPhrase(486) != "Busy Here" || reasonPhrase(480) != "Temporarily Unavailable" ||
		reasonPhrase(404) != "Not Found" || reasonPhrase(603) != "Decline" || reasonPhrase(599) != "Rejected" {
		t.Error("reason phrases")
	}
}

func TestSipfragStatus(t *testing.T) {
	for body, want := range map[string]int{
		"SIP/2.0 200 OK\r\n":    200,
		"SIP/2.0 100 Trying":    100,
		"  SIP/2.0 603 Decline": 603,
		"":                      0,
		"HTTP/1.1 200 OK":       0,
		"SIP/2.0":               0,
	} {
		if got := sipfragStatus(body); got != want {
			t.Errorf("%q: %d, want %d", body, got, want)
		}
	}
}

func TestAsksRefetch(t *testing.T) {
	for _, tc := range []struct {
		event, body string
		want        bool
	}{
		{"service-control", "action=restart\r\nRegisterCallId={x}", true},
		{"service-control", "RegisterCallId={x}\nAction=Reset\n", true},
		{"service-control", "action=apply", false},
		{"service-control", "", false},
		{"check-sync", "", true},
		{"refer", "SIP/2.0 200 OK", false},
	} {
		if got := asksRefetch(tc.event, tc.body); got != tc.want {
			t.Errorf("%s %q: %t", tc.event, tc.body, got)
		}
	}
}

func TestGrantedExpiry(t *testing.T) {
	ours := sip.Uri{Scheme: "sip", User: "100", Host: "10.0.0.2", Port: 5060}
	res := func(hdrs ...sip.Header) *sip.Response {
		r := sip.NewResponse(200, "OK")
		for _, h := range hdrs {
			r.AppendHeader(h)
		}
		return r
	}
	contact := func(user string, expires string) *sip.ContactHeader {
		c := &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: user, Host: "10.0.0.2", Port: 5060}, Params: sip.NewParams()}
		if expires != "" {
			c.Params.Add("expires", expires)
		}
		return c
	}
	exp := sip.ExpiresHeader(90)
	for name, tc := range map[string]struct {
		res  *sip.Response
		want int
	}{
		"ours among others": {res(contact("101", "30"), contact("100", "120")), 120},
		"only another":      {res(contact("101", "30")), 30},
		"expires header":    {res(contact("100", ""), &exp), 90},
		"nothing":           {res(), 600},
	} {
		if got := grantedExpiry(tc.res, ours, 600); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}
