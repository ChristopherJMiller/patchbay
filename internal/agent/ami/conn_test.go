package ami

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

const secret = "s3cr3t-ami-password"

// Compile-time: Conn is a Client.
var _ Client = (*Conn)(nil)

func TestLoginAndVersion(t *testing.T) {
	f := newFakeAMI(t, secret)
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	st := c.State()
	if st.Version != "22.11.0" || st.Error != "" || st.Since.IsZero() {
		t.Errorf("State = %+v, want up, version 22.11.0, no error", st)
	}
	// The Key is md5("1234567890" + secret) in lowercase hex.
	want := []string{
		"Action: Challenge\r\nActionID: <id>\r\nAuthType: MD5\r\n\r\n",
		"Action: Login\r\nActionID: <id>\r\nAuthType: MD5\r\nUsername: patchbay-agent\r\n" +
			"Key: 0958c751f71d354be28a9830274e3c1b\r\nEvents: off\r\n\r\n",
		"Action: CoreSettings\r\nActionID: <id>\r\n\r\n",
	}
	if got := f.allRequests(); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

// TestLoginNeverSendsTheSecret: whatever answers on AMI's port (Asterisk,
// or a host-network process that bound 127.0.0.1:5038 while Asterisk was
// down) gets only the MD5 of its own challenge and the secret, never the
// secret itself; a server that refuses the challenge gets no login at all,
// never a plaintext fallback.
func TestLoginNeverSendsTheSecret(t *testing.T) {
	for name, challenge := range map[string]string{"challenge answered": "squatter-nonce", "challenge refused": ""} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAMI(t, secret)
			f.mu.Lock()
			f.challenge = challenge
			f.mu.Unlock()
			c := startConn(t, f, secret, logr.Discard())
			if challenge != "" {
				waitUp(t, c)
			} else {
				waitFor(t, "a refused challenge", func() bool { return strings.Contains(c.State().Error, "challenge") })
				if logins := f.loginRequests(); len(logins) != 0 {
					t.Errorf("a Login was sent after the challenge was refused: %q", logins)
				}
			}
			for _, r := range f.allRequests() {
				if strings.Contains(r, secret) || strings.Contains(strings.ToLower(r), "\nsecret:") {
					t.Errorf("a request carries the secret: %q", r)
				}
			}
		})
	}
}

func TestNothingBeforeAnAccount(t *testing.T) {
	f := newFakeAMI(t, secret)
	c := New(Config{Addr: f.addr()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	if got := f.allRequests(); len(got) != 0 {
		t.Errorf("requests before SetAccount: %q", got)
	}
	if st := c.State(); st.Up || st.Error == "" {
		t.Errorf("State = %+v, want down with a reason", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
}

// TestLoginRefusedKeepsThePasswordSecret: a refused login leaves the
// session down, State says why, calls fail at once with ErrNotConnected,
// and neither an error nor a log line holds the password.
func TestLoginRefusedKeepsThePasswordSecret(t *testing.T) {
	f := newFakeAMI(t, "the-right-one")
	var sink logSink
	c := startConn(t, f, secret, sink.logger())
	waitFor(t, "two refused logins", func() bool { return len(f.loginRequests()) >= 2 })
	st := c.State()
	if st.Up || !strings.Contains(st.Error, "Authentication failed") || !strings.Contains(st.Error, "patchbay-agent") {
		t.Errorf("State = %+v, want down, refused as patchbay-agent", st)
	}
	start := time.Now()
	for name, call := range calls(c) {
		if err := call(context.Background()); !isNotConnected(err) {
			t.Errorf("%s = %v, want ErrNotConnected", name, err)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s error holds the password: %v", name, err)
		}
	}
	if time.Since(start) > time.Second {
		t.Errorf("calls while not connected took %s, want at once", time.Since(start))
	}
	for _, text := range []string{st.Error, sink.text()} {
		if strings.Contains(text, secret) {
			t.Errorf("the password leaked: %s", text)
		}
	}
	if !strings.Contains(sink.text(), "Authentication failed") {
		t.Errorf("the refusal was not logged: %s", sink.text())
	}
}

// calls is every Client call, for the "fails the same way" checks.
func calls(c *Conn) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"Reload":           func(ctx context.Context) error { return c.Reload(ctx, "res_pjsip.so") },
		"Endpoints":        func(ctx context.Context) error { _, err := c.Endpoints(ctx); return err },
		"DialplanContexts": func(ctx context.Context) error { _, err := c.DialplanContexts(ctx); return err },
		"Contacts":         func(ctx context.Context) error { _, err := c.Contacts(ctx); return err },
		"Channels":         func(ctx context.Context) error { _, err := c.Channels(ctx); return err },
		"Notify": func(ctx context.Context) error {
			return c.Notify(ctx, Notify{Endpoint: "phone-desk", Headers: []Header{{"Event", "check-sync"}}})
		},
	}
}

// TestGoldenRequests pins the exact bytes of every action.
func TestGoldenRequests(t *testing.T) {
	f := newFakeAMI(t, secret)
	f.setReply(func(c *fakeConn, r fakeRequest) {
		switch r.action {
		case "PJSIPShowEndpoints", "ShowDialPlan", "PJSIPShowContacts", "CoreShowChannels":
			c.send("Response: Success", "ActionID: "+r.id, "EventList: start", "Message: list follows")
			complete := map[string]string{
				"PJSIPShowEndpoints": "EndpointListComplete", "ShowDialPlan": "ShowDialPlanComplete",
				"PJSIPShowContacts": "ContactListComplete", "CoreShowChannels": "CoreShowChannelsComplete",
			}[r.action]
			c.send("Event: "+complete, "ActionID: "+r.id, "EventList: Complete", "ListItems: 0")
		default:
			c.send("Response: Success", "ActionID: "+r.id, "Message: done")
		}
	})
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	ctx := ctx5s(t)
	if err := c.Reload(ctx, "res_pjsip.so"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Endpoints(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DialplanContexts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Contacts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Channels(ctx); err != nil {
		t.Fatal(err)
	}
	n := Notify{
		Endpoint: "phone-desk",
		Headers:  []Header{{"Event", "service-control"}, {"Subscription-State", "active"}, {"Content-Type", "text/plain"}},
		Body:     []string{"action=restart", "RegisterCallId={a84b4c76e66710@192.168.20.5}", "HeadsetVersionStamp={0-0000000000}"},
	}
	if err := c.Notify(ctx, n); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Action: Reload\r\nActionID: <id>\r\nModule: res_pjsip.so\r\n\r\n",
		"Action: PJSIPShowEndpoints\r\nActionID: <id>\r\n\r\n",
		"Action: ShowDialPlan\r\nActionID: <id>\r\n\r\n",
		"Action: PJSIPShowContacts\r\nActionID: <id>\r\n\r\n",
		"Action: CoreShowChannels\r\nActionID: <id>\r\n\r\n",
		"Action: PJSIPNotify\r\nActionID: <id>\r\nEndpoint: phone-desk\r\n" +
			"Variable: Event=service-control\r\nVariable: Subscription-State=active\r\nVariable: Content-Type=text/plain\r\n" +
			"Variable: Content=action=restart\r\nVariable: Content=RegisterCallId={a84b4c76e66710@192.168.20.5}\r\n" +
			"Variable: Content=HeadsetVersionStamp={0-0000000000}\r\n\r\n",
	}
	// After Challenge, Login and CoreSettings.
	if got := f.allRequests()[3:]; !slices.Equal(got, want) {
		t.Errorf("requests:\n got %q\nwant %q", got, want)
	}
}

func TestUniqueActionIDs(t *testing.T) {
	f := newFakeAMI(t, secret)
	var mu sync.Mutex
	ids := map[string]int{}
	f.setReply(func(c *fakeConn, r fakeRequest) {
		mu.Lock()
		ids[r.id]++
		mu.Unlock()
		c.send("Response: Success", "ActionID: "+r.id)
	})
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := c.Reload(ctx5s(t), "pbx_config.so"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 20 {
		t.Errorf("%d distinct ActionIDs for 20 actions: %v", len(ids), ids)
	}
}

// TestListParsing reads each list as Asterisk 22 sends it.
func TestListParsing(t *testing.T) {
	f := newFakeAMI(t, secret)
	f.setReply(func(c *fakeConn, r fakeRequest) {
		id := "ActionID: " + r.id
		start := func() { c.send("Response: Success", id, "EventList: start", "Message: A listing follows") }
		switch r.action {
		case "PJSIPShowEndpoints":
			start()
			c.send("Event: EndpointList", id, "ObjectType: endpoint", "ObjectName: softphone-laptop", "Transport: transport-udp")
			c.send("Event: EndpointList", id, "ObjectType: endpoint", "ObjectName: phone-desk", "Transport: transport-tcp")
			c.send("Event: EndpointListComplete", id, "EventList: Complete", "ListItems: 2")
		case "ShowDialPlan":
			start()
			for _, ctx := range []string{"patchbay-unavailable", "patchbay-devices", "patchbay-devices", "patchbay-devices"} {
				c.send("Event: ListDialplan", id, "Context: "+ctx, "Extension: 100", "Priority: 1", "Application: Dial", "Registrar: pbx_config")
			}
			c.send("Event: ShowDialPlanComplete", id, "EventList: Complete", "ListItems: 4", "ListExtensions: 2", "ListPriorities: 4", "ListContexts: 2")
		case "PJSIPShowContacts":
			start()
			c.send("Event: ContactList", id, "ObjectType: contact", "ObjectName: 100;@6b4a1e8d3f",
				"ViaAddr: 192.168.20.5", "ViaPort: 51234", "QualifyTimeout: 3.000", "CallId: a84b4c76e66710@192.168.20.5",
				"RegServer: ", "PruneOnBoot: no", "Path: ", "Endpoint: phone-desk", "AuthenticateQualify: no",
				"Uri: sip:100@10.9.8.7:5060;transport=tcp", "QualifyFrequency: 0", "UserAgent: Cisco-CP7945G/9.4.2",
				"ExpirationTime: 1790000000", "OutboundProxy: ", "Status: NonQualified", "RoundtripUsec: N/A")
			// No ViaAddr: the URI's host. No Endpoint: the AOR from the ID.
			c.send("Event: ContactList", id, "ObjectType: contact", "ObjectName: 150;@99aa",
				"ViaAddr: ", "CallId: ", "Endpoint: ", "Uri: sip:150@[::ffff:10.244.0.7]:5070",
				"UserAgent: Linphone", "ExpirationTime: 1790000600", "Status: Reachable")
			c.send("Event: ContactList", id, "ObjectType: contact", "ObjectName: 151;@00", "AOR: 151",
				"Uri: sip:151@phone.example", "Status: Unreachable")
			c.send("Event: ContactListComplete", id, "EventList: Complete", "ListItems: 3")
		case "CoreShowChannels":
			start()
			for _, ch := range []string{"PJSIP/phone-desk-0000002a", "PJSIP/softphone-laptop-0000002b", "Local/600@x-00000001;1", "PJSIP/phone-front-desk-000000ff"} {
				c.send("Event: CoreShowChannel", id, "Channel: "+ch, "ChannelState: 6")
			}
			c.send("Event: CoreShowChannelsComplete", id, "EventList: Complete", "ListItems: 4")
		}
	})
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	ctx := ctx5s(t)

	eps, err := c.Endpoints(ctx)
	if err != nil || !slices.Equal(eps, []string{"phone-desk", "softphone-laptop"}) {
		t.Errorf("Endpoints = %v, %v", eps, err)
	}
	ctxs, err := c.DialplanContexts(ctx)
	if err != nil || !slices.Equal(ctxs, []string{"patchbay-devices", "patchbay-unavailable"}) {
		t.Errorf("DialplanContexts = %v, %v", ctxs, err)
	}
	contacts, err := c.Contacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Contact{
		{
			Endpoint: "phone-desk", AOR: "100", URI: "sip:100@10.9.8.7:5060;transport=tcp", Address: "192.168.20.5",
			UserAgent: "Cisco-CP7945G/9.4.2", CallID: "a84b4c76e66710@192.168.20.5",
			ExpiresAt: time.Unix(1790000000, 0).UTC(), Qualify: api.QualifyUnknown,
		},
		{
			AOR: "150", URI: "sip:150@[::ffff:10.244.0.7]:5070", Address: "10.244.0.7", UserAgent: "Linphone",
			ExpiresAt: time.Unix(1790000600, 0).UTC(), Qualify: api.QualifyReachable,
		},
		{AOR: "151", URI: "sip:151@phone.example", Qualify: api.QualifyUnreachable},
	}
	if !slices.Equal(contacts, want) {
		t.Errorf("Contacts:\n got %+v\nwant %+v", contacts, want)
	}
	chans, err := c.Channels(ctx)
	wantChans := []Channel{
		{"PJSIP/phone-desk-0000002a", "phone-desk"}, {"PJSIP/softphone-laptop-0000002b", "softphone-laptop"},
		{"Local/600@x-00000001;1", ""}, {"PJSIP/phone-front-desk-000000ff", "phone-front-desk"},
	}
	if err != nil || !slices.Equal(chans, wantChans) {
		t.Errorf("Channels = %+v, %v", chans, err)
	}
}

// TestEmptyLists: Asterisk answers an empty endpoint or contact list with
// an Error; that is an empty list, not a failure.
func TestEmptyLists(t *testing.T) {
	f := newFakeAMI(t, secret)
	f.setReply(func(c *fakeConn, r fakeRequest) {
		switch r.action {
		case "PJSIPShowEndpoints":
			// Asterisk's message ends in its own "\n".
			c.raw("Response: Error\r\nActionID: " + r.id + "\r\nMessage: No endpoints found\n\r\n\r\n")
		case "PJSIPShowContacts":
			c.raw("Response: Error\r\nActionID: " + r.id + "\r\nMessage: No Contacts found\n\r\n\r\n")
		case "CoreShowChannels":
			c.send("Response: Success", "ActionID: "+r.id, "EventList: start", "Message: Channels will follow")
			c.send("Event: CoreShowChannelsComplete", "ActionID: "+r.id, "EventList: Complete", "ListItems: 0")
		case "ShowDialPlan":
			c.send("Response: Error", "ActionID: "+r.id, "Message: Did not find context")
		}
	})
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	ctx := ctx5s(t)
	if eps, err := c.Endpoints(ctx); err != nil || len(eps) != 0 {
		t.Errorf("Endpoints = %v, %v; want none", eps, err)
	}
	if cs, err := c.Contacts(ctx); err != nil || len(cs) != 0 {
		t.Errorf("Contacts = %v, %v; want none", cs, err)
	}
	if chs, err := c.Channels(ctx); err != nil || len(chs) != 0 {
		t.Errorf("Channels = %v, %v; want none", chs, err)
	}
	// Any other Error is a failure, with Asterisk's message.
	if _, err := c.DialplanContexts(ctx); err == nil || !strings.Contains(err.Error(), "Did not find context") {
		t.Errorf("DialplanContexts = %v, want Asterisk's error", err)
	}
	// The session is still in step after the stray empty lines.
	if eps, err := c.Endpoints(ctx); err != nil || len(eps) != 0 {
		t.Errorf("Endpoints again = %v, %v", eps, err)
	}
}

// TestInterleavedReplies: two lists in flight at once, their events
// interleaved and the second answered first; each call gets its own.
func TestInterleavedReplies(t *testing.T) {
	f := newFakeAMI(t, secret)
	var mu sync.Mutex
	var pending []fakeRequest
	var conn *fakeConn
	f.setReply(func(c *fakeConn, r fakeRequest) {
		mu.Lock()
		defer mu.Unlock()
		conn = c
		pending = append(pending, r)
		if len(pending) < 2 {
			return
		}
		ep, ct := pending[0], pending[1]
		if ep.action != "PJSIPShowEndpoints" {
			ep, ct = ct, ep
		}
		e, k := "ActionID: "+ep.id, "ActionID: "+ct.id
		conn.send("Response: Success", k, "EventList: start")
		conn.send("Response: Success", e, "EventList: start")
		conn.send("Event: ContactList", k, "ObjectName: 100;@1", "Endpoint: phone-desk", "CallId: one")
		conn.send("Event: EndpointList", e, "ObjectName: phone-desk")
		conn.send("Event: FullyBooted", "Status: Fully Booted") // no ActionID: ignored
		conn.send("Event: ContactList", k, "ObjectName: 150;@2", "Endpoint: softphone-laptop", "CallId: two")
		conn.send("Event: ContactListComplete", k, "EventList: Complete")
		conn.send("Event: EndpointList", e, "ObjectName: softphone-laptop")
		conn.send("Event: EndpointListComplete", e, "EventList: Complete")
	})
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	ctx := ctx5s(t)
	var wg sync.WaitGroup
	var eps []string
	var contacts []Contact
	var err1, err2 error
	wg.Go(func() { eps, err1 = c.Endpoints(ctx) })
	wg.Go(func() { contacts, err2 = c.Contacts(ctx) })
	wg.Wait()
	if err1 != nil || !slices.Equal(eps, []string{"phone-desk", "softphone-laptop"}) {
		t.Errorf("Endpoints = %v, %v", eps, err1)
	}
	if err2 != nil || len(contacts) != 2 || contacts[0].CallID != "one" || contacts[1].CallID != "two" {
		t.Errorf("Contacts = %+v, %v", contacts, err2)
	}
}

func TestErrorResponses(t *testing.T) {
	f := newFakeAMI(t, secret)
	f.setReply(func(c *fakeConn, r fakeRequest) {
		switch r.action {
		case "Reload":
			c.send("Response: Error", "ActionID: "+r.id, "Message: No such module")
		case "PJSIPNotify":
			c.send("Response: Error", "ActionID: "+r.id, "Message: Unable to retrieve endpoint phone-gone")
		}
	})
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	ctx := ctx5s(t)
	err := c.Reload(ctx, "res_nope.so")
	if err == nil || err.Error() != "ami: Reload: No such module" {
		t.Errorf("Reload = %v, want Asterisk's message", err)
	}
	err = c.Notify(ctx, Notify{Endpoint: "phone-gone", Body: []string{"x"}})
	if err == nil || !strings.Contains(err.Error(), "Unable to retrieve endpoint phone-gone") {
		t.Errorf("Notify = %v, want Asterisk's message", err)
	}
	if !c.State().Up {
		t.Error("an error answer ended the session")
	}
}

func TestUnsafeValues(t *testing.T) {
	f := newFakeAMI(t, secret)
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	ctx := ctx5s(t)
	before := len(f.allRequests())
	bad := map[string]func() error{
		"reload CR":           func() error { return c.Reload(ctx, "res_pjsip.so\r\nAction: Command") },
		"reload LF":           func() error { return c.Reload(ctx, "pbx_config.so\n") },
		"endpoint LF":         func() error { return c.Notify(ctx, Notify{Endpoint: "phone-desk\nVariable: x=y"}) },
		"header name CR":      func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"Ev\rent", "x"}}}) },
		"header value LF":     func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"Event", "x\ny"}}}) },
		"body CR":             func() error { return c.Notify(ctx, Notify{Endpoint: "e", Body: []string{"a\rb"}}) },
		"header value comma":  func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"Event", "a,b"}}}) },
		"header value quote":  func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"Event", `a"b`}}}) },
		"header value slash":  func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"Event", `a\b`}}}) },
		"header name comma":   func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"A,B", "x"}}}) },
		"header name equals":  func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"A=B", "x"}}}) },
		"header name colon":   func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"A:B", "x"}}}) },
		"header name empty":   func() error { return c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"", "x"}}}) },
		"body comma":          func() error { return c.Notify(ctx, Notify{Endpoint: "e", Body: []string{"RegisterCallId={a,b}"}}) },
		"body quote":          func() error { return c.Notify(ctx, Notify{Endpoint: "e", Body: []string{`x"`}}) },
		"body backslash":      func() error { return c.Notify(ctx, Notify{Endpoint: "e", Body: []string{`x\`}}) },
		"later body line bad": func() error { return c.Notify(ctx, Notify{Endpoint: "e", Body: []string{"ok", "bad,"}}) },
	}
	for name, call := range bad {
		if err := call(); !errors.Is(err, ErrUnsafeValue) {
			t.Errorf("%s: %v, want ErrUnsafeValue", name, err)
		}
	}
	if got := f.allRequests()[before:]; len(got) != 0 {
		t.Errorf("unsafe calls sent %q", got)
	}
	// "=" and ":" are fine in a value and a body line.
	if err := c.Notify(ctx, Notify{Endpoint: "e", Headers: []Header{{"Event", "a=b:c"}}, Body: []string{"k=v:w"}}); err != nil {
		t.Errorf("a safe notify: %v", err)
	}
}

// TestDeadline: a reply that never comes ends the call at its deadline,
// and the session carries on.
func TestDeadline(t *testing.T) {
	f := newFakeAMI(t, secret)
	f.setReply(func(c *fakeConn, r fakeRequest) {
		if r.action == "Reload" {
			return // never answered
		}
		c.send("Response: Success", "ActionID: "+r.id, "EventList: start")
		c.send("Event: EndpointList", "ActionID: "+r.id, "ObjectName: phone-desk")
		// ... and never complete.
	})
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	for name, call := range map[string]func(context.Context) error{
		"Reload":    func(ctx context.Context) error { return c.Reload(ctx, "manager") },
		"Endpoints": func(ctx context.Context) error { _, err := c.Endpoints(ctx); return err },
	} {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := call(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s = %v, want the deadline", name, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s took %s", name, d)
		}
	}
	if !c.State().Up {
		t.Error("a missed deadline ended the session")
	}
}

// TestReconnect: when Asterisk drops the session, State says so, calls
// fail with ErrNotConnected, and the client logs in again.
func TestReconnect(t *testing.T) {
	f := newFakeAMI(t, secret)
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	first := c.State().Since

	// Down for a while: refuse logins so the outage can be observed.
	f.setPassword("not-yet")
	f.dropAll()
	waitFor(t, "a refused login after the drop", func() bool { return len(f.loginRequests()) >= 2 })
	waitFor(t, "the session to be down", func() bool { return !c.State().Up })
	st := c.State()
	if st.Error == "" || !st.Since.After(first) && !st.Since.Equal(first) {
		t.Errorf("State after the drop = %+v", st)
	}
	if err := c.Reload(context.Background(), "res_pjsip.so"); !isNotConnected(err) {
		t.Errorf("Reload while down = %v, want ErrNotConnected", err)
	}
	f.setPassword(secret)
	waitUp(t, c)
	if logins := len(f.loginRequests()); logins < 3 {
		t.Errorf("%d logins, want the first, a refused one and the new one", logins)
	}
	if err := c.Reload(ctx5s(t), "res_pjsip.so"); err != nil {
		t.Errorf("Reload after reconnecting: %v", err)
	}
}

// TestCallInFlightWhenTheSessionDrops gets ErrNotConnected, not a hang.
func TestCallInFlightWhenTheSessionDrops(t *testing.T) {
	f := newFakeAMI(t, secret)
	f.setReply(func(c *fakeConn, _ fakeRequest) { _ = c.nc.Close() })
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	if err := c.Reload(ctx5s(t), "res_pjsip.so"); !isNotConnected(err) {
		t.Errorf("Reload = %v, want ErrNotConnected", err)
	}
}

// TestAccountChange: a new account ends the session and logs in with it.
func TestAccountChange(t *testing.T) {
	f := newFakeAMI(t, secret)
	c := startConn(t, f, secret, logr.Discard())
	waitUp(t, c)
	c.SetAccount("patchbay-agent", secret) // unchanged: nothing happens
	time.Sleep(50 * time.Millisecond)
	if n := len(f.loginRequests()); n != 1 {
		t.Fatalf("%d logins after an unchanged SetAccount, want 1", n)
	}
	f.setPassword("new-password")
	c.SetAccount("patchbay-agent", "new-password")
	waitFor(t, "a login with the new password", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.logins, "patchbay-agent/new-password")
	})
	waitUp(t, c)
	if err := c.Reload(ctx5s(t), "manager"); err != nil {
		t.Errorf("Reload with the new account: %v", err)
	}
}

func TestNotAMI(t *testing.T) {
	f := newFakeAMI(t, secret)
	f.mu.Lock()
	f.banner = "SSH-2.0-OpenSSH_9.9"
	f.mu.Unlock()
	c := startConn(t, f, secret, logr.Discard())
	waitFor(t, "a refused banner", func() bool { return strings.Contains(c.State().Error, "banner") })
	if c.State().Up {
		t.Error("up on a server that is not AMI")
	}
}

func TestDialFailure(t *testing.T) {
	var attempts []int
	var mu sync.Mutex
	c := New(Config{
		Addr: "127.0.0.1:1",
		Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") },
		Backoff: func(n int) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, n)
			return time.Millisecond
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	c.SetAccount("patchbay-agent", secret)
	waitFor(t, "three attempts", func() bool { mu.Lock(); defer mu.Unlock(); return len(attempts) >= 3 })
	mu.Lock()
	if !slices.Equal(attempts[:3], []int{1, 2, 3}) {
		t.Errorf("backoff attempts %v, want 1, 2, 3", attempts)
	}
	mu.Unlock()
	if st := c.State(); st.Up || !strings.Contains(st.Error, "connection refused") {
		t.Errorf("State = %+v", st)
	}
}

func TestDefaultBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{
		1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 100: 30 * time.Second,
	} {
		if got := defaultBackoff(attempt); got != want {
			t.Errorf("defaultBackoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestUnsafeLogin(t *testing.T) {
	f := newFakeAMI(t, secret)
	c := startConn(t, f, "pass\r\nAction: Command", logr.Discard())
	waitFor(t, "a refused login", func() bool { return strings.Contains(c.State().Error, "cannot carry") })
	if got := f.allRequests(); len(got) != 0 {
		t.Errorf("an unsafe login was sent: %q", got)
	}
	if strings.Contains(c.State().Error, "pass") {
		t.Errorf("State error holds the password: %s", c.State().Error)
	}
}

func TestURIHost(t *testing.T) {
	for uri, want := range map[string]string{
		"sip:100@192.168.20.5:5060;transport=tcp": "192.168.20.5",
		"<sip:100@192.168.20.5>":                  "192.168.20.5",
		"sips:192.168.20.6":                       "192.168.20.6",
		"sip:100@[2001:db8::1]:5060":              "2001:db8::1",
		"sip:100@host.example:5060":               "host.example",
		"sip:100@[2001:db8::1":                    "",
	} {
		if got := uriHost(uri); got != want {
			t.Errorf("uriHost(%q) = %q, want %q", uri, got, want)
		}
	}
	if got := canonicalIP("fe80::1%eth0"); got != "" {
		t.Errorf("canonicalIP of a zoned address = %q", got)
	}
}
