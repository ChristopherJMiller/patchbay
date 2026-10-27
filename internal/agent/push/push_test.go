package push_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/push"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

const (
	pushUser = "patchbay"
	pushPass = "s3cret-Pa55-word"
	deskURL  = "http://192.168.0.240:8088"
	execFmt  = `<CiscoIPPhoneExecute>%s</CiscoIPPhoneExecute>`
)

// phone is a fake phone web server: it records POST /CGI/Execute.
type phone struct {
	mu     sync.Mutex
	hits   int
	method string
	path   string
	xml    string
	user   string
	pass   string
	basic  bool
	answer string
	status int
}

func (ph *phone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	ph.hits++
	ph.method, ph.path = r.Method, r.URL.Path
	_ = r.ParseForm()
	ph.xml = r.PostForm.Get("XML")
	ph.user, ph.pass, ph.basic = r.BasicAuth()
	if ph.status != 0 {
		w.WriteHeader(ph.status)
	}
	_, _ = w.Write([]byte(ph.answer))
}

type fixture struct {
	ph    *phone
	pages *push.Pages
	p     *push.Pusher
	logs  *strings.Builder
	addr  netip.Addr
	creds bool
}

// newFixture serves the fake phone on 127.0.0.1 and lets the Pusher reach it
// by dialing there whatever address it is given; the target it is told is
// 192.168.20.100, a scrapeable address.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ph := &phone{answer: `<CiscoIPPhoneResponse><ResponseItem Status="0" Data="" URL=""/></CiscoIPPhoneResponse>`}
	ts := httptest.NewServer(ph)
	t.Cleanup(ts.Close)
	f := &fixture{ph: ph, pages: &push.Pages{}, logs: &strings.Builder{}, creds: true, addr: netip.MustParseAddr("192.168.20.100")}
	var mu sync.Mutex
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ts.Listener.Addr().String())
		},
	}}
	p, err := push.New(push.Config{
		Credentials: func() (string, string, bool) { return pushUser, pushPass, f.creds },
		Target: func(device string) (netip.Addr, bool) {
			if device == "phone-nowhere" {
				return netip.Addr{}, false
			}
			return f.addr, true
		},
		DeviceHTTPURL: deskURL,
		Pages:         f.pages,
		Client:        client,
		Log: funcr.New(func(prefix, args string) {
			mu.Lock()
			defer mu.Unlock()
			f.logs.WriteString(prefix + " " + args + "\n")
		}, funcr.Options{Verbosity: 10}),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	return f
}

func (f *fixture) hits() int {
	f.ph.mu.Lock()
	defer f.ph.mu.Unlock()
	return f.ph.hits
}

func item(u string) string { return `<ExecuteItem Priority="0" URL="` + u + `"/>` }

func TestPushPage(t *testing.T) {
	f := newFixture(t)
	resp := f.p.Push(context.Background(), api.PushRequest{
		Device: "phone-desk", PageVersion: "abc", Page: "<CiscoIPPhoneText/>", Beep: true,
	})
	if resp != (api.PushResponse{Sent: true, Phone: "status 0"}) {
		t.Fatalf("response %+v", resp)
	}
	ph := f.ph
	if ph.method != http.MethodPost || ph.path != "/CGI/Execute" {
		t.Errorf("the phone saw %s %s", ph.method, ph.path)
	}
	want := strings.Replace(execFmt, "%s", item("Play:patchbay-chime.raw")+item(deskURL+"/messages/phone-desk.xml?v=abc"), 1)
	if ph.xml != want {
		t.Errorf("XML:\n got %s\nwant %s", ph.xml, want)
	}
	if !ph.basic || ph.user != pushUser || ph.pass != pushPass {
		t.Errorf("Basic auth was %q/%v", ph.user, ph.basic)
	}
	v, page, ok := f.pages.Get("phone-desk")
	if !ok || v != "abc" || string(page) != "<CiscoIPPhoneText/>" {
		t.Errorf("stored page %q %q %v", v, page, ok)
	}
	if got := f.pages.Acknowledged("phone-desk"); got != "abc" {
		t.Errorf("acknowledged %q, want abc", got)
	}
}

func TestPushPageWithoutBeepOrVersion(t *testing.T) {
	f := newFixture(t)
	if resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Page: "<p/>"}); !resp.Sent {
		t.Fatalf("response %+v", resp)
	}
	if want := strings.Replace(execFmt, "%s", item(deskURL+"/messages/phone-desk.xml"), 1); f.ph.xml != want {
		t.Errorf("XML %s, want %s", f.ph.xml, want)
	}
}

func TestPushClear(t *testing.T) {
	f := newFixture(t)
	f.pages.Set("phone-desk", "abc", []byte("<p/>"))
	resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Clear: true})
	if !resp.Sent {
		t.Fatalf("response %+v", resp)
	}
	if want := strings.Replace(execFmt, "%s", item(push.ClearURI), 1); f.ph.xml != want {
		t.Errorf("XML %s, want %s", f.ph.xml, want)
	}
	if push.ClearURI != "Init:Services" {
		t.Errorf("ClearURI = %q", push.ClearURI)
	}
	if _, _, ok := f.pages.Get("phone-desk"); ok {
		t.Error("Clear left the page stored")
	}
}

func TestPushExecuteRaw(t *testing.T) {
	f := newFixture(t)
	if resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Execute: []string{"Key:Services"}}); !resp.Sent {
		t.Fatalf("response %+v", resp)
	}
	if want := strings.Replace(execFmt, "%s", item("Key:Services"), 1); f.ph.xml != want {
		t.Errorf("XML %s, want %s", f.ph.xml, want)
	}
	before := f.hits()
	for name, req := range map[string]api.PushRequest{
		"dial":             {Device: "phone-desk", Execute: []string{"Dial:911"}},
		"four items":       {Device: "phone-desk", Execute: []string{"Key:A", "Key:B", "Key:C", "Key:D"}},
		"http item":        {Device: "phone-desk", Execute: []string{"http://example.com/x"}},
		"empty after kind": {Device: "phone-desk", Execute: []string{"Key:"}},
		"control char":     {Device: "phone-desk", Execute: []string{"Key:Serv\nices"}},
		"with a page":      {Device: "phone-desk", Page: "<p/>", Execute: []string{"Key:Services"}},
		"clear and page":   {Device: "phone-desk", Page: "<p/>", Clear: true},
		"nothing":          {Device: "phone-desk"},
		"bad device":       {Device: "../x", Page: "<p/>"},
		"bad version":      {Device: "phone-desk", Page: "<p/>", PageVersion: "a&b"},
		"huge page":        {Device: "phone-desk", Page: strings.Repeat("a", api.MaxPushPage+1)},
	} {
		if resp := f.p.Push(context.Background(), req); resp.Sent || resp.Error == "" {
			t.Errorf("%s: %+v, want a refusal", name, resp)
		}
	}
	if f.hits() != before {
		t.Error("a refused push reached the phone")
	}
	if _, _, ok := f.pages.Get("phone-desk"); ok {
		t.Error("a refused push stored a page")
	}
}

func TestPushPhoneError(t *testing.T) {
	f := newFixture(t)
	f.ph.answer = `<CiscoIPPhoneError Number="4"/>`
	resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", PageVersion: "abc", Page: "<p/>"})
	if resp.Sent || resp.Phone != "error 4" || resp.Error == "" {
		t.Fatalf("response %+v", resp)
	}
	if v, _, ok := f.pages.Get("phone-desk"); !ok || v != "abc" {
		t.Errorf("the page was not kept after a phone error: %q %v", v, ok)
	}
	if got := f.pages.Acknowledged("phone-desk"); got != "" {
		t.Errorf("a phone error acknowledged %q", got)
	}

	f.ph.answer = `<CiscoIPPhoneResponse><ResponseItem Status="3" Data="" URL=""/></CiscoIPPhoneResponse>`
	if resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Beep: true}); resp.Sent || resp.Phone != "status 3" {
		t.Errorf("a non-zero status: %+v", resp)
	}
	f.ph.answer, f.ph.status = "Unauthorized", http.StatusUnauthorized
	if resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Beep: true}); resp.Sent || resp.Phone != "http 401" {
		t.Errorf("a 401: %+v", resp)
	}
	f.ph.answer, f.ph.status = strings.Repeat("x", 10000), 0
	if resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Beep: true}); resp.Sent || len(resp.Phone) > api.MaxPhoneAnswer || len(resp.Error) > api.MaxPhoneAnswer+100 {
		t.Errorf("a junk answer: %+v", resp)
	}
}

func TestPushRefusals(t *testing.T) {
	f := newFixture(t)
	push1 := func(device string) api.PushResponse {
		return f.p.Push(context.Background(), api.PushRequest{Device: device, PageVersion: "v", Page: "<p/>"})
	}
	f.creds = false
	if resp := push1("phone-desk"); resp.Sent || resp.Error == "" {
		t.Errorf("no credentials: %+v", resp)
	}
	if v, _, ok := f.pages.Get("phone-desk"); !ok || v != "v" || f.pages.Acknowledged("phone-desk") != "" {
		t.Errorf("after a no-credentials refusal: stored %q %v, acknowledged %q; want stored, not acknowledged", v, ok, f.pages.Acknowledged("phone-desk"))
	}
	f.creds = true
	if resp := push1("phone-nowhere"); resp.Sent || resp.Error == "" {
		t.Errorf("no target: %+v", resp)
	}
	if v, _, ok := f.pages.Get("phone-nowhere"); !ok || v != "v" || f.pages.Acknowledged("phone-nowhere") != "" {
		t.Errorf("after a no-target refusal: stored %q %v, acknowledged %q; want stored, not acknowledged", v, ok, f.pages.Acknowledged("phone-nowhere"))
	}
	for _, a := range []string{"127.0.0.1", "169.254.1.1", "224.0.0.1", "0.0.0.0", "::1"} {
		f.addr = netip.MustParseAddr(a)
		if resp := push1("phone-desk"); resp.Sent || resp.Error == "" {
			t.Errorf("%s: %+v", a, resp)
		}
	}
	if f.hits() != 0 {
		t.Errorf("%d requests left on refusals", f.hits())
	}
}

func TestPushNeverLogsCredentials(t *testing.T) {
	f := newFixture(t)
	f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", PageVersion: "abc", Page: "<p/>", Beep: true})
	f.ph.answer = `<CiscoIPPhoneError Number="4"/>`
	f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Beep: true})
	f.creds = false
	resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Beep: true})
	if f.logs.Len() == 0 {
		t.Fatal("nothing was logged at all")
	}
	for _, secret := range []string{pushPass, "Basic ", "czNjcmV0"} {
		if strings.Contains(f.logs.String(), secret) || strings.Contains(resp.Error, secret) {
			t.Errorf("the log or an answer holds %q:\n%s", secret, f.logs)
		}
	}
}

func TestNewRefusesIncompleteConfig(t *testing.T) {
	ok := func() (string, string, bool) { return "", "", false }
	tg := func(string) (netip.Addr, bool) { return netip.Addr{}, false }
	for name, cfg := range map[string]push.Config{
		"none":        {},
		"no target":   {Credentials: ok, Pages: &push.Pages{}, DeviceHTTPURL: deskURL},
		"no pages":    {Credentials: ok, Target: tg, DeviceHTTPURL: deskURL},
		"bad url":     {Credentials: ok, Target: tg, Pages: &push.Pages{}, DeviceHTTPURL: "192.168.0.240:8088"},
		"https url":   {Credentials: ok, Target: tg, Pages: &push.Pages{}, DeviceHTTPURL: "https://192.168.0.240:8088"},
		"no creds fn": {Target: tg, Pages: &push.Pages{}, DeviceHTTPURL: deskURL},
	} {
		if _, err := push.New(cfg); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}

func TestPushAcknowledgement(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if resp := f.p.Push(ctx, api.PushRequest{Device: "phone-desk", PageVersion: "v1", Page: "<one/>"}); !resp.Sent {
		t.Fatalf("response %+v", resp)
	}
	if got := f.pages.Acknowledged("phone-desk"); got != "v1" {
		t.Fatalf("acknowledged %q after a successful push, want v1", got)
	}
	// A failed push after a successful one: the new bytes are stored, the
	// observation still shows the old version.
	f.ph.answer = `<CiscoIPPhoneError Number="4"/>`
	if resp := f.p.Push(ctx, api.PushRequest{Device: "phone-desk", PageVersion: "v2", Page: "<two/>"}); resp.Sent {
		t.Fatalf("response %+v", resp)
	}
	if v, page, _ := f.pages.Get("phone-desk"); v != "v2" || string(page) != "<two/>" {
		t.Errorf("stored %q %q, want the v2 bytes", v, page)
	}
	if got := f.pages.Acknowledged("phone-desk"); got != "v1" {
		t.Errorf("acknowledged %q after a failed push, want v1", got)
	}
	// A failed Clear keeps both.
	if resp := f.p.Push(ctx, api.PushRequest{Device: "phone-desk", Clear: true}); resp.Sent {
		t.Fatalf("response %+v", resp)
	}
	if _, _, ok := f.pages.Get("phone-desk"); !ok || f.pages.Acknowledged("phone-desk") != "v1" {
		t.Errorf("a failed Clear changed the page (%v) or its acknowledgement %q", ok, f.pages.Acknowledged("phone-desk"))
	}
	// A beep or a raw item does not acknowledge anything.
	f.ph.answer = `<CiscoIPPhoneResponse><ResponseItem Status="0" Data="" URL=""/></CiscoIPPhoneResponse>`
	f.p.Push(ctx, api.PushRequest{Device: "phone-desk", Beep: true})
	if got := f.pages.Acknowledged("phone-desk"); got != "v1" {
		t.Errorf("acknowledged %q after a beep, want v1", got)
	}
	// A successful Clear empties both.
	if resp := f.p.Push(ctx, api.PushRequest{Device: "phone-desk", Clear: true}); !resp.Sent {
		t.Fatalf("response %+v", resp)
	}
	if _, _, ok := f.pages.Get("phone-desk"); ok || f.pages.Acknowledged("phone-desk") != "" {
		t.Errorf("a successful Clear left the page (%v) or version %q", ok, f.pages.Acknowledged("phone-desk"))
	}
}

func TestPushDeviceNames(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if resp := f.p.Push(ctx, api.PushRequest{Device: "phone-desk.lab", PageVersion: "v", Page: "<p/>"}); !resp.Sent {
		t.Errorf("phone-desk.lab: %+v", resp)
	}
	if want := strings.Replace(execFmt, "%s", item(deskURL+"/messages/phone-desk.lab.xml?v=v"), 1); f.ph.xml != want {
		t.Errorf("XML %s, want %s", f.ph.xml, want)
	}
	before := f.hits()
	for _, bad := range []string{"phone-../x", "phone-..x", "phone/x", "", ".phone", strings.Repeat("a", drivers.DeviceIDMaxLength+1)} {
		if resp := f.p.Push(ctx, api.PushRequest{Device: bad, Page: "<p/>"}); resp.Sent || resp.Error == "" {
			t.Errorf("%q: %+v, want a refusal", bad, resp)
		}
	}
	if f.hits() != before {
		t.Error("a refused device name reached the phone")
	}
	if resp := f.p.Push(ctx, api.PushRequest{Device: strings.Repeat("a", drivers.DeviceIDMaxLength), Page: "<p/>"}); !resp.Sent {
		t.Errorf("a %d-character name: %+v", drivers.DeviceIDMaxLength, resp)
	}
}

func TestPushHTTPFailureBeatsBody(t *testing.T) {
	f := newFixture(t)
	f.ph.status = http.StatusInternalServerError // with the default, good body
	resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", PageVersion: "v", Page: "<p/>"})
	if resp.Sent || resp.Phone != "http 500" {
		t.Errorf("response %+v, want http 500 and not sent", resp)
	}
	if f.pages.Acknowledged("phone-desk") != "" {
		t.Error("a 500 acknowledged the page")
	}
}

func TestPushEscapesRawItems(t *testing.T) {
	f := newFixture(t)
	if resp := f.p.Push(context.Background(), api.PushRequest{Device: "phone-desk", Execute: []string{`Play:a&b"c`}}); !resp.Sent {
		t.Fatalf("response %+v", resp)
	}
	if !strings.Contains(f.ph.xml, `URL="Play:a&amp;b&#34;c"`) {
		t.Errorf("XML %s, want & and \" escaped", f.ph.xml)
	}
}

func TestPushUnreachablePhone(t *testing.T) {
	f := newFixture(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadAddr := dead.Listener.Addr().String()
	dead.Close() // nothing listens there now
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, deadAddr)
		},
	}}
	p, err := push.New(push.Config{
		Credentials:   func() (string, string, bool) { return pushUser, pushPass, true },
		Target:        func(string) (netip.Addr, bool) { return f.addr, true },
		DeviceHTTPURL: deskURL, Pages: f.pages, Client: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := p.Push(context.Background(), api.PushRequest{Device: "phone-desk", PageVersion: "v", Page: "<p/>"})
	if resp.Sent || resp.Error == "" || len(resp.Error) > api.MaxPhoneAnswer {
		t.Fatalf("response %+v", resp)
	}
	if strings.Contains(resp.Error, "http://") || strings.Contains(resp.Error, "/CGI/Execute") || strings.Contains(resp.Error, f.addr.String()) {
		t.Errorf("the error carries the URL: %q", resp.Error)
	}
	if f.pages.Acknowledged("phone-desk") != "" {
		t.Error("an unreachable phone acknowledged the page")
	}
}
