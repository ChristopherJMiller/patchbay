// Package push is the agent's push action: it stores the message page the
// controller sends, and tells the phone to show it by POSTing an Execute
// to the phone's web server (docs/superpowers/specs/2026-10-26-phone-messages-design.md).
//
// The order is fixed. The page is stored first, so that the phone's fetch of
// it, which follows the Execute at once, finds it. Then the Execute goes out
// as POST /CGI/Execute (form field XML, Basic auth with the push account);
// device HTTP answers. A failed or refused push leaves the new page stored
// (so it can be fetched, and a retry needs no resend) but changes nothing
// the observations report: DeviceObservations.Page is the last version the
// phone acknowledged (Status 0), so the controller never takes a page for
// shown that the phone did not accept. A successful Clear drops the stored
// page and the acknowledged version; a failed one keeps both.
//
// The agent never builds a page: it stores and serves the bytes it is given.
// The credentials are read when used and appear in no log line, error or
// answer.
package push

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/scrape"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

const (
	// ClearURI is the Execute item that returns a phone to its idle display.
	// The bench confirmed it on 2026-10-27 (CP-7945G, SIP 9.4(2)SR4-3).
	ClearURI = "Init:Services"

	// Timeout bounds each phase of the POST, like the scraper's.
	Timeout = 5 * time.Second

	// maxAnswer is how much of the phone's answer is read.
	maxAnswer = 4 << 10
	// maxItemLength bounds one raw Execute item.
	maxItemLength = 256
	// maxVersionLength bounds PushRequest.PageVersion.
	maxVersionLength = 64

	executePath = "/CGI/Execute"
)

// rawPrefixes are the URI schemes a raw Execute item may use (nothing that
// dials).
var rawPrefixes = []string{"Init:", "Key:", "SoftKey:", "Play:"}

var (
	deviceName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	versionName = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)
	statusAttr  = regexp.MustCompile(`Status\s*=\s*["'](-?\d{1,6})["']`)
	numberAttr  = regexp.MustCompile(`Number\s*=\s*["'](-?\d{1,6})["']`)
)

// Config configures a Pusher.
type Config struct {
	// Credentials is the push account (apply.Applier.PushCredentials); ok
	// false means the bundle has none yet.
	Credentials func() (user, password string, ok bool)
	// Target is a device's address (observe.Store.Target): the live
	// contact's, else its last served fetch's source.
	Target func(device string) (netip.Addr, bool)
	// DeviceHTTPURL is device HTTP's base URL, "http://<pbx address>:8088":
	// where the phone fetches a pushed page.
	DeviceHTTPURL string
	// Pages is where pages are stored; the same value device HTTP serves.
	Pages *Pages
	// Client sends the POST. Nil means a client with no proxy, no
	// redirects, 5 s timeouts and no keep-alives, like the scraper's. A
	// given client is copied and never follows redirects.
	Client *http.Client
	// Log receives one line per push (V(1)); never a credential.
	Log logr.Logger
}

// Pusher is the push action.
type Pusher struct {
	cfg    Config
	client *http.Client
}

// New returns a Pusher.
func New(cfg Config) (*Pusher, error) {
	switch {
	case cfg.Credentials == nil:
		return nil, errors.New("push: no credentials source")
	case cfg.Target == nil:
		return nil, errors.New("push: no target source")
	case cfg.Pages == nil:
		return nil, errors.New("push: no page store")
	}
	u, err := url.Parse(cfg.DeviceHTTPURL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("push: device HTTP URL %q: want http://<host>:<port>", cfg.DeviceHTTPURL)
	}
	cfg.DeviceHTTPURL = strings.TrimRight(cfg.DeviceHTTPURL, "/")
	p := &Pusher{cfg: cfg}
	if cfg.Client == nil {
		p.client = defaultClient()
	} else {
		c := *cfg.Client
		c.CheckRedirect = noRedirects
		p.client = &c
	}
	return p, nil
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func defaultClient() *http.Client {
	return &http.Client{
		Timeout:       Timeout,
		CheckRedirect: noRedirects,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            (&net.Dialer{Timeout: Timeout}).DialContext,
			DisableKeepAlives:      true,
			ResponseHeaderTimeout:  Timeout,
			MaxResponseHeaderBytes: 64 << 10,
		},
	}
}

// Push stores or drops the device's page, then sends the phone its
// Execute. Every refusal is Sent false with the reason; nothing is sent for
// a request that is malformed.
func (p *Pusher) Push(ctx context.Context, req api.PushRequest) api.PushResponse {
	refuse := func(format string, args ...any) api.PushResponse {
		return api.PushResponse{Error: fmt.Sprintf(format, args...)}
	}
	if err := checkRequest(req); err != nil {
		return refuse("%v", err)
	}
	items := p.items(req)

	// The page is stored before anything can fail, so the controller sees
	// what the agent holds, and before the POST, so the phone's immediate
	// fetch finds it.
	if req.Page != "" && !req.Clear {
		p.cfg.Pages.Set(req.Device, req.PageVersion, []byte(req.Page))
	}

	user, pass, ok := p.cfg.Credentials()
	if !ok || user == "" || pass == "" {
		return refuse("the push credentials are not available yet")
	}
	target, ok := p.cfg.Target(req.Device)
	if !ok {
		return refuse("no address is known for %s", req.Device)
	}
	addr, err := scrape.Scrapeable(target.String())
	if err != nil {
		return refuse("%v", err)
	}

	resp := p.post(ctx, addr, user, pass, executeXML(items))
	if resp.Sent {
		// Only the phone's Status 0 makes a page "on the phone" (or, for a
		// clear, nothing on it); a failed push keeps both as they were.
		switch {
		case req.Clear:
			p.cfg.Pages.Clear(req.Device)
		case req.Page != "":
			p.cfg.Pages.Acknowledge(req.Device, req.PageVersion)
		}
	}
	p.cfg.Log.V(1).Info("push", "device", req.Device, "items", len(items), "sent", resp.Sent, "phone", resp.Phone, "error", resp.Error)
	return resp
}

// checkRequest refuses a request that is malformed or that asks for
// something the push never sends.
func checkRequest(req api.PushRequest) error {
	switch {
	case len(req.Device) > drivers.DeviceIDMaxLength || !deviceName.MatchString(req.Device) || strings.Contains(req.Device, ".."):
		return errors.New("the device name is not usable in a URL")
	case len(req.PageVersion) > maxVersionLength || !versionName.MatchString(req.PageVersion):
		return errors.New("the page version is not usable in a URL")
	case len(req.Page) > api.MaxPushPage:
		return fmt.Errorf("the page is larger than %d bytes", api.MaxPushPage)
	case req.Clear && req.Page != "":
		return errors.New("a push cannot both clear and show a page")
	case len(req.Execute) > api.MaxPushExecute:
		return fmt.Errorf("an Execute holds at most %d items", api.MaxPushExecute)
	case len(req.Execute) > 0 && (req.Page != "" || req.Beep || req.Clear):
		return errors.New("raw Execute items cannot be combined with a page, a beep or a clear")
	case len(req.Execute) == 0 && req.Page == "" && !req.Beep && !req.Clear:
		return errors.New("nothing to push")
	}
	for _, item := range req.Execute {
		if len(item) > maxItemLength || strings.ContainsFunc(item, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return errors.New("an Execute item is too long or holds control characters")
		}
		if !hasAnyPrefix(item, rawPrefixes) {
			return fmt.Errorf("an Execute item must start with one of %s", strings.Join(rawPrefixes, " "))
		}
	}
	return nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) && len(s) > len(p) {
			return true
		}
	}
	return false
}

// items are the Execute's URIs, in order: raw ones alone; else the chime,
// then the page (or the clearing URI).
func (p *Pusher) items(req api.PushRequest) []string {
	if len(req.Execute) > 0 {
		return req.Execute
	}
	var items []string
	if req.Beep {
		items = append(items, "Play:"+path.Base(compile.ChimeFile))
	}
	switch {
	case req.Clear:
		items = append(items, ClearURI)
	case req.Page != "":
		u := p.cfg.DeviceHTTPURL + compile.MessagesPathPrefix + req.Device + ".xml"
		if req.PageVersion != "" {
			u += "?v=" + url.QueryEscape(req.PageVersion)
		}
		items = append(items, u)
	}
	return items
}

// executeXML is the CiscoIPPhoneExecute document for items.
func executeXML(items []string) string {
	var b strings.Builder
	b.WriteString("<CiscoIPPhoneExecute>")
	for _, it := range items {
		b.WriteString(`<ExecuteItem Priority="0" URL="`)
		_ = xml.EscapeText(&b, []byte(it))
		b.WriteString(`"/>`)
	}
	b.WriteString("</CiscoIPPhoneExecute>")
	return b.String()
}

// post sends the Execute and reads the phone's answer.
func (p *Pusher) post(ctx context.Context, addr netip.Addr, user, pass, doc string) api.PushResponse {
	host := addr.String()
	if addr.Is6() {
		host = "[" + host + "]"
	}
	body := url.Values{"XML": {doc}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+executePath, strings.NewReader(body))
	if err != nil {
		return api.PushResponse{Error: "building the request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "patchbay-agent/"+version.Version)
	req.SetBasicAuth(user, pass)
	resp, err := p.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return api.PushResponse{Error: bound("the phone could not be reached: "+err.Error(), api.MaxPhoneAnswer)}
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return api.PushResponse{Error: bound("reading the phone's answer: "+err.Error(), api.MaxPhoneAnswer)}
	}
	return interpret(resp.StatusCode, answer)
}

// interpret reads the phone's answer: a CiscoIPPhoneResponse with Status 0
// is success; any other status, a CiscoIPPhoneError or an HTTP failure is
// not.
func interpret(code int, answer []byte) api.PushResponse {
	if code < 200 || code > 299 {
		return api.PushResponse{Phone: fmt.Sprintf("http %d", code), Error: fmt.Sprintf("the phone answered HTTP %d", code)}
	}
	if m := numberAttr.FindSubmatch(answer); m != nil && bytes.Contains(answer, []byte("CiscoIPPhoneError")) {
		return api.PushResponse{Phone: "error " + string(m[1]), Error: "the phone refused the push"}
	}
	if bytes.Contains(answer, []byte("CiscoIPPhoneResponse")) {
		all := statusAttr.FindAllSubmatch(answer, -1)
		for _, m := range all {
			if string(m[1]) != "0" {
				return api.PushResponse{Phone: "status " + string(m[1]), Error: "the phone did not accept the push"}
			}
		}
		if len(all) > 0 {
			return api.PushResponse{Sent: true, Phone: "status 0"}
		}
	}
	return api.PushResponse{Error: "the phone's answer was not a CiscoIPPhoneResponse"}
}

// bound cuts s to at most n bytes.
func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
