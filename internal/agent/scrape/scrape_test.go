package scrape_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/scrape"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// clock is a settable clock; the fake phone moves it as it serves pages.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// phone is a fake phone's web server. Every address the scraper dials
// reaches it (dialed records which), so tests use documentation addresses
// (192.0.2.0/24) that the scraper does not refuse, never loopback.
type phone struct {
	t      *testing.T
	ts     *httptest.Server
	mu     sync.Mutex
	pages  map[string]http.HandlerFunc
	hits   []string
	dialed []string
}

func newPhone(t *testing.T) *phone {
	t.Helper()
	p := &phone{t: t, pages: map[string]http.HandlerFunc{}}
	p.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hits = append(p.hits, r.Method+" "+r.Host+r.URL.RequestURI())
		h := p.pages[r.URL.Path]
		p.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(p.ts.Close)
	return p
}

func (p *phone) handle(path string, h http.HandlerFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages[path] = h
}

// serve makes the phone answer path with body, after setting the clock to
// at (unless at is zero).
func (p *phone) serve(path string, c *clock, at time.Time, body []byte) {
	p.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		if !at.IsZero() {
			c.Set(at)
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write(body)
	})
}

func (p *phone) status(path string, code int) {
	p.handle(path, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

func (p *phone) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.hits)
}

func (p *phone) dials() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.dialed)
}

// client reaches the fake phone whatever address it is asked for.
func (p *phone) client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			p.mu.Lock()
			p.dialed = append(p.dialed, addr)
			p.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, p.ts.Listener.Addr().String())
		},
	}}
}

// healthy makes the phone serve all three pages, moving the clock to
// t0+1s, +2s and +3s as it serves each.
func (p *phone) healthy(c *clock, info api.DeviceInformation, nc scrape.NetworkConfigurationX, log []string) {
	p.serve("/DeviceInformationX", c, t0.Add(1*time.Second), page(p.t, infoX(info)))
	p.serve("/NetworkConfigurationX", c, t0.Add(2*time.Second), page(p.t, nc))
	p.serve("/DeviceLogX", c, t0.Add(3*time.Second), page(p.t, scrape.DeviceLogX{Status: log}))
}

type noRecorder struct{ t *testing.T }

func (r noRecorder) RecordScrape(device string, _ api.WebScrape) {
	r.t.Errorf("Scrape recorded a result for %s; only Run records", device)
}

func newScraper(t *testing.T, cfg scrape.Config) *scrape.Scraper {
	t.Helper()
	if cfg.Recorder == nil {
		cfg.Recorder = noRecorder{t}
	}
	if cfg.Log.GetSink() == nil {
		cfg.Log = logr.Discard()
	}
	return scrape.New(cfg)
}

var (
	desk    = sensor.ScrapeTarget{Device: "phone-desk", Address: "192.0.2.10"}
	deskNC  = scrape.NetworkConfigurationX{AdminVLANID: "", CallManager1: "192.168.0.240  Active"}
	deskLog = []string{"TFTP Timeout : SEPD0C78915E8C8.cnf.xml.sgn", "Trust List Update Failed"}
)

func deskScrape(at time.Time) api.WebScrape {
	info := fullInfo
	return api.WebScrape{
		At: at, Address: desk.Address, DeviceInformation: &info,
		NetworkConfiguration: &api.NetworkConfiguration{CallManager1: "192.168.0.240  Active"},
		DeviceLog:            slices.Clone(deskLog),
	}
}

func TestScrapeSuccess(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.healthy(c, fullInfo, deskNC, deskLog)
	s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now})

	got := s.Scrape(context.Background(), desk)
	// At is when the last page was read: the fake phone moved the clock to
	// t0+3s while it served /DeviceLogX.
	if want := deskScrape(t0.Add(3 * time.Second)); !reflect.DeepEqual(got, want) {
		t.Errorf("Scrape =\n%+v\nwant\n%+v", got, want)
	}
	wantReq := []string{
		"GET 192.0.2.10/DeviceInformationX", "GET 192.0.2.10/NetworkConfigurationX", "GET 192.0.2.10/DeviceLogX",
	}
	if r := p.requests(); !slices.Equal(r, wantReq) {
		t.Errorf("requests %q, want %q", r, wantReq)
	}
	if d := p.dials(); !slices.Equal(d, []string{"192.0.2.10:80", "192.0.2.10:80", "192.0.2.10:80"}) {
		t.Errorf("dialed %q, want port 80 three times", d)
	}
}

// TestScrapeCapture: a phone that serves exactly the real CP-7945G's three
// pages is scraped: its /DeviceInformationX is adopted as it is (the
// asterisks around versionID are the driver's to strip), its
// /NetworkConfigurationX up to where the phone's renderer crashed, and its
// empty /DeviceLogX is an empty log. Nothing of that is an error.
func TestScrapeCapture(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.serve("/DeviceInformationX", c, t0.Add(time.Second), capture(t, "DeviceInformationX.xml"))
	p.serve("/NetworkConfigurationX", c, t0.Add(2*time.Second), capture(t, "NetworkConfigurationX.xml"))
	p.serve("/DeviceLogX", c, t0.Add(3*time.Second), capture(t, "DeviceLogX.xml"))
	s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now})

	got := s.Scrape(context.Background(), desk)
	info, nc := captureInfo, captureNC
	want := api.WebScrape{At: t0.Add(3 * time.Second), Address: desk.Address, DeviceInformation: &info, NetworkConfiguration: &nc}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scrape =\n%+v\nwant\n%+v", got, want)
	}
	if len(p.requests()) != 3 {
		t.Errorf("requests %q, want all three pages", p.requests())
	}
}

func TestScrapeAddressAndPort(t *testing.T) {
	for _, tc := range []struct {
		addr string
		port int
		host string
		dial string
	}{
		{"192.0.2.10", 8080, "192.0.2.10:8080", "192.0.2.10:8080"},
		{"2001:db8::10", 0, "[2001:db8::10]", "[2001:db8::10]:80"},
		{"2001:db8::10", 8080, "[2001:db8::10]:8080", "[2001:db8::10]:8080"},
		{"::ffff:192.0.2.10", 0, "192.0.2.10", "192.0.2.10:80"},
	} {
		c := &clock{t: t0}
		p := newPhone(t)
		p.healthy(c, fullInfo, deskNC, deskLog)
		s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now, Port: tc.port})
		got := s.Scrape(context.Background(), sensor.ScrapeTarget{Device: "phone-desk", Address: tc.addr})
		if got.Error != "" || got.Address != tc.addr {
			t.Errorf("%s port %d: %+v", tc.addr, tc.port, got)
		}
		if r := p.requests(); len(r) != 3 || r[0] != "GET "+tc.host+"/DeviceInformationX" {
			t.Errorf("%s port %d: requests %q, want Host %s", tc.addr, tc.port, r, tc.host)
		}
		if d := p.dials(); len(d) != 3 || d[0] != tc.dial {
			t.Errorf("%s port %d: dialed %q, want %s", tc.addr, tc.port, d, tc.dial)
		}
	}
}

// TestScrapeFailureAdoptsNothing: /DeviceInformationX decides a scrape (it
// carries the load). When it cannot be read the scrape fails at once,
// without the other pages, and carries the previous success's fields
// (none the first time), its own At and Address, and an Error.
func TestScrapeFailureAdoptsNothing(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.healthy(c, fullInfo, deskNC, deskLog)
	p.status("/DeviceInformationX", http.StatusInternalServerError)
	s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now})

	first := s.Scrape(context.Background(), desk)
	want := api.WebScrape{At: t0, Address: desk.Address}
	if first.Error == "" || !strings.Contains(first.Error, "DeviceInformationX") || !strings.Contains(first.Error, "500") {
		t.Errorf("first failure's Error = %q, want it to name /DeviceInformationX and 500", first.Error)
	}
	first.Error = ""
	if !reflect.DeepEqual(first, want) {
		t.Errorf("first failure = %+v, want nothing adopted: %+v", first, want)
	}
	if r := p.requests(); !slices.Equal(r, []string{"GET 192.0.2.10/DeviceInformationX"}) {
		t.Errorf("requests %q, want only /DeviceInformationX", r)
	}

	p.healthy(c, fullInfo, deskNC, deskLog)
	ok := s.Scrape(context.Background(), desk)
	if !reflect.DeepEqual(ok, deskScrape(t0.Add(3*time.Second))) {
		t.Fatalf("success = %+v", ok)
	}
	// The caller owns what it got: scribbling on it changes nothing kept.
	ok.DeviceInformation.VersionID = "scribbled"
	ok.NetworkConfiguration.AdminVLAN = "scribbled"
	ok.DeviceLog[0] = "scribbled"

	info := func(body string) func() {
		return func() { p.serve("/DeviceInformationX", c, time.Time{}, []byte(body)) }
	}
	status := func(code int) func() { return func() { p.status("/DeviceInformationX", code) } }
	for name, fail := range map[string]func(){
		"500":                    status(http.StatusInternalServerError),
		"404":                    status(http.StatusNotFound),
		"503":                    status(http.StatusServiceUnavailable),
		"204":                    status(http.StatusNoContent),
		"an empty 200":           info(``),
		"HTML":                   info(`<html><body>Device Information</body></html>`),
		"breaks off":             info(`<DeviceInformation><versionID>x`),
		"another page's root":    info(`<NetworkConfiguration><AdminVLANId>99</AdminVLANId></NetworkConfiguration>`),
		"the capture, cut short": info(string(capture(t, "DeviceInformationX.xml")[:400])),
	} {
		p.serve("/NetworkConfigurationX", c, time.Time{}, page(t, scrape.NetworkConfigurationX{AdminVLANID: "99"}))
		p.serve("/DeviceLogX", c, time.Time{}, page(t, scrape.DeviceLogX{Status: []string{"other"}}))
		fail()
		c.Set(t0.Add(time.Hour))
		got := s.Scrape(context.Background(), sensor.ScrapeTarget{Device: desk.Device, Address: "192.0.2.11"})
		if got.Error == "" || !strings.Contains(got.Error, "DeviceInformationX") {
			t.Errorf("%s: Error %q, want it to name /DeviceInformationX", name, got.Error)
			continue
		}
		want := deskScrape(got.At)
		want.Address, want.Error = "192.0.2.11", got.Error
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v\nwant the previous success's fields: %+v", name, got, want)
		}
		if !got.At.Equal(t0.Add(time.Hour)) {
			t.Errorf("%s: At = %v, want when it failed", name, got.At)
		}
	}

	// Another device has no previous success of its own.
	p.status("/DeviceInformationX", http.StatusInternalServerError)
	if got := s.Scrape(context.Background(), sensor.ScrapeTarget{Device: "phone-lab", Address: desk.Address}); got.DeviceInformation != nil || got.Error == "" {
		t.Errorf("another device adopted phone-desk's success: %+v", got)
	}
}

// TestScrapeBestEffortPages: /NetworkConfigurationX and /DeviceLogX are
// best effort. One that cannot be read leaves its field empty (nil, never
// the previous success's: that may predate what the phone runs now) and
// the scrape succeeds with everything else it read; a page that breaks
// off keeps what came before the break.
func TestScrapeBestEffortPages(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now})
	other := api.DeviceInformation{VersionID: "SIP45.9-3-1SR1-1S", ModelNumber: "CP-7945G"}
	otherNC := &api.NetworkConfiguration{AdminVLAN: "99"}
	otherLog := []string{"other"}
	nc := func(fail func()) func() { return fail }
	serveNC := func(body string) func() {
		return func() { p.serve("/NetworkConfigurationX", c, time.Time{}, []byte(body)) }
	}
	serveLog := func(body string) func() {
		return func() { p.serve("/DeviceLogX", c, time.Time{}, []byte(body)) }
	}
	redirect := func(path string) func() {
		return func() {
			p.handle(path, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) })
		}
	}
	for _, tc := range []struct {
		name string
		fail func()
		nc   *api.NetworkConfiguration
		log  []string
	}{
		{"NetworkConfigurationX 503", nc(func() { p.status("/NetworkConfigurationX", http.StatusServiceUnavailable) }), nil, otherLog},
		{"NetworkConfigurationX 404", nc(func() { p.status("/NetworkConfigurationX", http.StatusNotFound) }), nil, otherLog},
		{"NetworkConfigurationX 204", nc(func() { p.status("/NetworkConfigurationX", http.StatusNoContent) }), nil, otherLog},
		{"NetworkConfigurationX is HTML", serveNC(`<html><body>Network Configuration</body></html>`), nil, otherLog},
		{"NetworkConfigurationX redirects", redirect("/NetworkConfigurationX"), nil, otherLog},
		{"NetworkConfigurationX breaks off", serveNC(`<NetworkConfiguration><AdminVLANId>7</AdminVLANId><CallManager1>192.168`),
			&api.NetworkConfiguration{AdminVLAN: "7"}, otherLog},
		{"NetworkConfigurationX as the real phone serves it", serveNC(string(capture(t, "NetworkConfigurationX.xml"))), &captureNC, otherLog},
		{"DeviceLogX 500", func() { p.status("/DeviceLogX", http.StatusInternalServerError) }, otherNC, nil},
		{"DeviceLogX 404", func() { p.status("/DeviceLogX", http.StatusNotFound) }, otherNC, nil},
		{"DeviceLogX 204", func() { p.status("/DeviceLogX", http.StatusNoContent) }, otherNC, nil},
		{"DeviceLogX is HTML", serveLog(`<html><body>Status Messages</body></html>`), otherNC, nil},
		{"DeviceLogX breaks off", serveLog(`<DeviceLog><status>a</status><status>b`), otherNC, nil},
		{"DeviceLogX redirects", redirect("/DeviceLogX"), otherNC, nil},
		{"DeviceLogX is empty", serveLog(``), otherNC, nil},
		{"both", func() {
			p.status("/NetworkConfigurationX", http.StatusInternalServerError)
			p.status("/DeviceLogX", http.StatusInternalServerError)
		}, nil, nil},
	} {
		// A full success first: a page that fails now must not show it.
		p.healthy(c, fullInfo, deskNC, deskLog)
		if ok := s.Scrape(context.Background(), desk); ok.Error != "" {
			t.Fatalf("%s: the success before: %s", tc.name, ok.Error)
		}
		p.serve("/DeviceInformationX", c, t0.Add(time.Hour), page(t, infoX(other)))
		p.serve("/NetworkConfigurationX", c, time.Time{}, page(t, scrape.NetworkConfigurationX{AdminVLANID: "99"}))
		p.serve("/DeviceLogX", c, time.Time{}, page(t, scrape.DeviceLogX{Status: otherLog}))
		tc.fail()
		before := len(p.requests())

		got := s.Scrape(context.Background(), desk)
		info := other
		want := api.WebScrape{At: t0.Add(time.Hour), Address: desk.Address, DeviceInformation: &info,
			NetworkConfiguration: tc.nc, DeviceLog: tc.log}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, want)
		}
		if r := p.requests()[before:]; len(r) != 3 {
			t.Errorf("%s: requests %q, want all three pages", tc.name, r)
		}
		for _, r := range p.requests() {
			if strings.Contains(r, "elsewhere") {
				t.Errorf("%s: a redirect was followed: %s", tc.name, r)
			}
		}
	}
}

// TestRefusedAddresses: no request leaves for an address that cannot be
// a phone on the voice VLAN.
func TestRefusedAddresses(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.healthy(c, fullInfo, deskNC, deskLog)
	s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now})
	for _, addr := range []string{
		"127.0.0.1", "127.1.2.3", "::1", "::ffff:127.0.0.1", // loopback
		"0.0.0.0", "::", // unspecified
		"224.0.0.1", "239.255.255.250", "ff02::1", "ff05::2", // multicast
		"169.254.10.20", "fe80::1", "fe80::1%eth0", // link-local
		"255.255.255.255",                                   // broadcast
		"", "phone.example", "192.0.2.10:80", "192.0.2.300", // not an address
	} {
		c.Set(t0.Add(time.Minute))
		got := s.Scrape(context.Background(), sensor.ScrapeTarget{Device: "phone-desk", Address: addr})
		if got.Error == "" || got.Address != addr || !got.At.Equal(t0.Add(time.Minute)) || got.DeviceInformation != nil {
			t.Errorf("%q: %+v, want a failed scrape", addr, got)
		}
	}
	if r := p.requests(); len(r) != 0 {
		t.Errorf("requests went out: %q", r)
	}
	if d := p.dials(); len(d) != 0 {
		t.Errorf("dialed %q", d)
	}
	// The control: a documentation address is scraped.
	if got := s.Scrape(context.Background(), desk); got.Error != "" {
		t.Errorf("192.0.2.10 refused: %s", got.Error)
	}
}

func TestBodyLimit(t *testing.T) {
	const limit = 256 << 10
	padded := func(root, inner string, n int) []byte {
		head := `<?xml version="1.0"?><` + root + `>` + inner + `<!--`
		tail := `--></` + root + `>`
		return []byte(head + strings.Repeat("x", n-len(head)-len(tail)) + tail)
	}
	c := &clock{t: t0}
	p := newPhone(t)
	s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now})

	p.healthy(c, fullInfo, deskNC, deskLog)
	p.serve("/DeviceLogX", c, time.Time{}, padded("DeviceLog", "<status>a</status>", limit))
	if got := s.Scrape(context.Background(), desk); got.Error != "" || !slices.Equal(got.DeviceLog, []string{"a"}) {
		t.Errorf("a page of exactly 256 KiB: %q, Error %q", got.DeviceLog, got.Error)
	}
	// A best-effort page over the limit is not read; the scrape stands.
	p.serve("/DeviceLogX", c, time.Time{}, padded("DeviceLog", "<status>a</status>", limit+1))
	if got := s.Scrape(context.Background(), desk); got.Error != "" || got.DeviceLog != nil {
		t.Errorf("a log page of 256 KiB + 1: %q, Error %q; want the scrape without it", got.DeviceLog, got.Error)
	}
	p.serve("/NetworkConfigurationX", c, time.Time{}, padded("NetworkConfiguration", "<AdminVLANId>5</AdminVLANId>", limit+1))
	if got := s.Scrape(context.Background(), desk); got.Error != "" || got.NetworkConfiguration != nil {
		t.Errorf("a network page of 256 KiB + 1: %+v, Error %q; want the scrape without it", got.NetworkConfiguration, got.Error)
	}
	// /DeviceInformationX over the limit fails the scrape.
	p.serve("/DeviceInformationX", c, time.Time{}, padded("DeviceInformation", "<versionID>SIP45.9-4-2SR4-3S</versionID>", limit+1))
	if got := s.Scrape(context.Background(), desk); got.Error == "" || !strings.Contains(got.Error, "DeviceInformationX") {
		t.Errorf("a device page of 256 KiB + 1: Error %q", got.Error)
	}
	// A page that never ends fails at the limit, not at the timeout.
	p.handle("/DeviceInformationX", func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat("<a>x</a>", 512))
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	start := time.Now()
	if got := s.Scrape(context.Background(), desk); got.Error == "" || !strings.Contains(got.Error, "DeviceInformationX") {
		t.Errorf("an endless page: Error %q", got.Error)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("an endless page took %v to fail", d)
	}
}

func TestNoRedirects(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.healthy(c, fullInfo, deskNC, deskLog)
	p.handle("/DeviceInformationX", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	p.serve("/elsewhere", c, time.Time{}, page(t, infoX(fullInfo)))

	// Neither the default client's policy nor a caller's client follows it.
	follows := p.client()
	follows.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	for name, client := range map[string]*http.Client{"given client": follows, "default policy": p.client()} {
		s := newScraper(t, scrape.Config{Client: client, Now: c.Now})
		if got := s.Scrape(context.Background(), desk); got.Error == "" || !strings.Contains(got.Error, "302") {
			t.Errorf("%s: a redirect: Error %q, want the 302 to fail the scrape", name, got.Error)
		}
	}
	for _, r := range p.requests() {
		if strings.Contains(r, "elsewhere") {
			t.Errorf("the redirect was followed: %s", r)
		}
	}
}

func TestTimeout(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.healthy(c, fullInfo, deskNC, deskLog)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now})
	s.SetPageTimeout(100 * time.Millisecond)

	silent := func(http.ResponseWriter, *http.Request) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}
	trickle := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			for _, b := range []byte(body) {
				if _, err := w.Write([]byte{b}); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-release:
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}
	}
	start := time.Now()
	// Best-effort pages that never answer, or trickle: each is given up on
	// after its timeout, and the scrape stands without them.
	p.handle("/NetworkConfigurationX", silent)
	p.handle("/DeviceLogX", trickle(`<DeviceLog><status>slow</status></DeviceLog>`))
	got := s.Scrape(context.Background(), desk)
	if got.Error != "" || got.DeviceInformation == nil || got.NetworkConfiguration != nil || got.DeviceLog != nil {
		t.Errorf("silent and trickling best-effort pages: %+v", got)
	}
	// A silent or trickling /DeviceInformationX fails it.
	p.handle("/DeviceInformationX", silent)
	if got := s.Scrape(context.Background(), desk); got.Error == "" || !strings.Contains(got.Error, "DeviceInformationX") {
		t.Errorf("a silent phone: Error %q", got.Error)
	}
	p.handle("/DeviceInformationX", trickle(string(page(t, infoX(fullInfo)))))
	if got := s.Scrape(context.Background(), desk); got.Error == "" || !strings.Contains(got.Error, "DeviceInformationX") {
		t.Errorf("a trickling phone: Error %q", got.Error)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("four timeouts of 100 ms took %v", d)
	}
	// The caller's context bounds a scrape too.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.Scrape(ctx, desk); got.Error == "" {
		t.Error("a cancelled context: the scrape succeeded")
	}
	// A context that ends after /DeviceInformationX fails the scrape: the
	// best-effort pages were not given their chance.
	p.healthy(c, fullInfo, deskNC, deskLog)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	p.handle("/NetworkConfigurationX", func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	if got := s.Scrape(ctx, desk); got.Error == "" {
		t.Errorf("a context cancelled mid-scrape: the scrape succeeded: %+v", got)
	}
}

// TestDefaultClient: without a client, each page has 5 s, no proxy, and no
// redirect is followed.
func TestDefaultClient(t *testing.T) {
	s := scrape.New(scrape.Config{})
	c := s.Client()
	if c.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", c.Timeout)
	}
	if s.PageTimeout() != 5*time.Second {
		t.Errorf("page timeout = %v, want 5s", s.PageTimeout())
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the default transport uses a proxy")
	}
	req := httptest.NewRequest(http.MethodGet, "http://192.0.2.10/DeviceInformationX", nil)
	if err := c.CheckRedirect(req, []*http.Request{req}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

func TestErrorIsBoundedText(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.healthy(c, fullInfo, deskNC, deskLog)
	// The XML error quotes the phone's own text: a long element name with
	// control characters.
	p.serve("/DeviceInformationX", c, time.Time{}, []byte("<"+strings.Repeat("A", 2000)+"\x01/>"))
	s := newScraper(t, scrape.Config{Client: p.client(), Now: c.Now})
	got := s.Scrape(context.Background(), desk)
	if got.Error == "" || len(got.Error) > 256 {
		t.Errorf("Error has %d bytes, want 1 to 256: %q", len(got.Error), got.Error)
	}
	for _, r := range got.Error {
		if r < 0x20 || r == 0x7f {
			t.Errorf("Error holds a control character: %q", got.Error)
		}
	}
}

// TestSchedule: a device is due when never scraped, when its address
// changed, or when its interval (60 s without a live contact, 5 min with
// one) has passed since its last attempt; one in flight is never due
// twice; one no longer targeted is forgotten.
func TestSchedule(t *testing.T) {
	lab := sensor.ScrapeTarget{Device: "phone-lab", Address: "192.0.2.20", Live: true}
	at := t0.Add
	setup := func(t *testing.T) (attempt func(time.Duration, ...sensor.ScrapeTarget), check func(time.Duration, []string, ...sensor.ScrapeTarget), s *scrape.Scraper) {
		s = newScraper(t, scrape.Config{})
		attempt = func(d time.Duration, ts ...sensor.ScrapeTarget) {
			for _, tg := range ts {
				s.Begin(tg, at(d))
				s.End(tg.Device)
			}
		}
		check = func(d time.Duration, want []string, ts ...sensor.ScrapeTarget) {
			t.Helper()
			if got := s.Due(at(d), ts); !slices.Equal(got, want) {
				t.Errorf("at +%v: due %q, want %q", d, got, want)
			}
		}
		return attempt, check, s
	}

	t.Run("intervals", func(t *testing.T) {
		attempt, check, _ := setup(t)
		check(0, []string{"phone-desk", "phone-lab"}, lab, desk, lab) // never scraped; a duplicate once
		attempt(0, desk, lab)
		check(59*time.Second, nil, desk, lab)
		check(60*time.Second, []string{"phone-desk"}, desk, lab) // no live contact: 60 s
		check(299*time.Second, []string{"phone-desk"}, desk, lab)
		check(300*time.Second, []string{"phone-desk", "phone-lab"}, desk, lab) // live: 5 min
		attempt(300*time.Second, desk)
		check(359*time.Second, []string{"phone-lab"}, desk, lab)
	})

	t.Run("address change", func(t *testing.T) {
		attempt, check, _ := setup(t)
		attempt(0, desk)
		moved := desk
		moved.Address = "192.0.2.11"
		check(time.Second, []string{"phone-desk"}, moved)
		attempt(time.Second, moved)
		check(2*time.Second, nil, moved)
		check(2*time.Second, []string{"phone-desk"}, desk)
	})

	t.Run("live switching", func(t *testing.T) {
		attempt, check, _ := setup(t)
		liveDesk, deadLab := desk, lab
		liveDesk.Live, deadLab.Live = true, false
		attempt(0, desk, lab)
		check(60*time.Second, []string{"phone-desk"}, desk, lab)
		check(60*time.Second, []string{"phone-lab"}, liveDesk, deadLab) // desk went live, lab lost its contact
		check(299*time.Second, nil, liveDesk, lab)
	})

	t.Run("in flight", func(t *testing.T) {
		_, check, s := setup(t)
		s.Begin(desk, at(0))
		check(10*time.Hour, nil, desk)
		moved := desk
		moved.Address = "192.0.2.11"
		check(10*time.Hour, nil, moved)
		s.End(desk.Device)
		check(10*time.Hour, []string{"phone-desk"}, desk)
	})

	t.Run("removal", func(t *testing.T) {
		attempt, check, s := setup(t)
		attempt(0, desk, lab)
		check(time.Second, nil, desk)
		if k := s.Known(); !slices.Equal(k, []string{"phone-desk"}) {
			t.Errorf("remembered %q after phone-lab left, want only phone-desk", k)
		}
		check(2*time.Second, []string{"phone-lab"}, desk, lab) // back: never scraped
		check(3*time.Second, nil)
		if k := s.Known(); len(k) != 0 {
			t.Errorf("remembered %q with no targets", k)
		}
	})

	t.Run("order", func(t *testing.T) {
		_, _, s := setup(t)
		a := sensor.ScrapeTarget{Device: "phone-a", Address: "192.0.2.1"}
		b := sensor.ScrapeTarget{Device: "phone-b", Address: "192.0.2.2"}
		n := sensor.ScrapeTarget{Device: "phone-n", Address: "192.0.2.3"}
		s.Begin(b, t0)
		s.End(b.Device)
		s.Begin(a, t0.Add(time.Second))
		s.End(a.Device)
		if got := s.Due(t0.Add(time.Hour), []sensor.ScrapeTarget{a, b, n}); !slices.Equal(got, []string{"phone-n", "phone-b", "phone-a"}) {
			t.Errorf("order %q, want never scraped first, then the oldest attempt", got)
		}
	})
}

// targets is a mutable ScrapeTargets.
type targets struct {
	mu sync.Mutex
	ts []sensor.ScrapeTarget
}

func (f *targets) ScrapeTargets(time.Time) []sensor.ScrapeTarget {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ts)
}

func (f *targets) set(ts ...sensor.ScrapeTarget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ts = ts
}

type recorded struct {
	device string
	w      api.WebScrape
}

type chanRecorder chan recorded

func (r chanRecorder) RecordScrape(device string, w api.WebScrape) { r <- recorded{device, w} }

func TestRunRecordsEachScrape(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.healthy(c, fullInfo, deskNC, deskLog)
	tg := &targets{}
	tg.set(desk)
	rec := make(chanRecorder, 16)
	s := scrape.New(scrape.Config{
		Targets: tg, Recorder: rec, Client: p.client(), Now: c.Now, Tick: 5 * time.Millisecond, Log: logr.Discard(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case got := <-rec:
		if got.device != "phone-desk" || !reflect.DeepEqual(got.w, deskScrape(t0.Add(3*time.Second))) {
			t.Errorf("recorded %s %+v", got.device, got.w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was recorded")
	}
	// Not due again for 60 s of the (frozen) clock.
	select {
	case got := <-rec:
		t.Errorf("scraped again before its interval: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	// The clock moves on: due again.
	c.Set(t0.Add(time.Hour))
	select {
	case <-rec:
	case <-time.After(5 * time.Second):
		t.Fatal("not scraped again after its interval")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunLogsIncompletePages: a scrape that went without part of a
// best-effort page succeeds and is recorded, and the log (V(1)) says what
// it went without; the empty /DeviceLogX is not worth a line.
func TestRunLogsIncompletePages(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.serve("/DeviceInformationX", c, time.Time{}, capture(t, "DeviceInformationX.xml"))
	p.serve("/NetworkConfigurationX", c, time.Time{}, capture(t, "NetworkConfigurationX.xml"))
	p.serve("/DeviceLogX", c, time.Time{}, capture(t, "DeviceLogX.xml"))
	var (
		mu     sync.Mutex
		logged []string
	)
	log := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{Verbosity: 1})
	tg := &targets{}
	tg.set(desk)
	rec := make(chanRecorder, 16)
	s := scrape.New(scrape.Config{Targets: tg, Recorder: rec, Client: p.client(), Now: c.Now, Tick: 5 * time.Millisecond, Log: log})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case got := <-rec:
		if got.w.Error != "" || got.w.NetworkConfiguration == nil || *got.w.NetworkConfiguration != captureNC {
			t.Errorf("recorded %+v", got.w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was recorded")
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], "phone-desk") ||
		!strings.Contains(logged[0], "/NetworkConfigurationX: incomplete page") || !strings.Contains(logged[0], "unexpected EOF") ||
		strings.Contains(logged[0], "DeviceLogX") {
		t.Errorf("logged %q; want one line naming phone-desk and the break in /NetworkConfigurationX", logged)
	}
}

// TestRunConcurrency: at most Concurrency scrapes run at once; the rest
// wait for a slot; on shutdown Run waits for them and records none it
// cancelled.
func TestRunConcurrency(t *testing.T) {
	c := &clock{t: t0}
	p := newPhone(t)
	p.healthy(c, fullInfo, deskNC, deskLog)
	var (
		mu            sync.Mutex
		active, peak  int
		released      = make(chan struct{})
		releaseOnce   sync.Once
		releaseBlocks = func() { releaseOnce.Do(func() { close(released) }) }
	)
	t.Cleanup(releaseBlocks)
	p.handle("/DeviceInformationX", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()
		select {
		case <-released:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(page(t, infoX(fullInfo)))
	})
	tg := &targets{}
	var all []sensor.ScrapeTarget
	for i := range 5 {
		all = append(all, sensor.ScrapeTarget{Device: fmt.Sprintf("phone-%d", i), Address: fmt.Sprintf("192.0.2.%d", 10+i)})
	}
	tg.set(all...)
	rec := make(chanRecorder, 16)
	s := scrape.New(scrape.Config{
		Targets: tg, Recorder: rec, Client: p.client(), Now: c.Now, Tick: 5 * time.Millisecond, Concurrency: 2, Log: logr.Discard(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if peak != 2 {
		t.Errorf("peak concurrency %d, want 2", peak)
	}
	mu.Unlock()
	releaseBlocks()
	seen := map[string]bool{}
	for len(seen) < 5 {
		select {
		case got := <-rec:
			if got.w.Error != "" {
				t.Errorf("%s: %s", got.device, got.w.Error)
			}
			seen[got.device] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 5 devices scraped", len(seen))
		}
	}
	mu.Lock()
	if peak != 2 {
		t.Errorf("peak concurrency %d, want 2", peak)
	}
	mu.Unlock()

	// A scrape in flight at shutdown is cancelled and not recorded.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	p.handle("/DeviceInformationX", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	tg.set(sensor.ScrapeTarget{Device: "phone-new", Address: "192.0.2.99"})
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(p.requests(), func(r string) bool { return strings.HasPrefix(r, "GET 192.0.2.99/") }) {
		if time.Now().After(deadline) {
			t.Fatal("phone-new was never scraped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	select {
	case got := <-rec:
		t.Errorf("a cancelled scrape was recorded: %s %+v", got.device, got.w)
	default:
	}
}

func TestRunNeedsTargetsAndRecorder(t *testing.T) {
	if err := scrape.New(scrape.Config{}).Run(context.Background()); err == nil {
		t.Error("Run without Targets and Recorder succeeded")
	}
}
