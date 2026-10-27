package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/scrape"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// signalName is a signal's name: a DNS label, as script.WaitSignal
// requires.
var signalName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// serveWeb starts the phone's HTTP server on the script's WebListen: the
// State, the signals, and in cisco mode the three pages the agent scrapes.
func (r *runner) serveWeb(ctx context.Context) error {
	addr := r.sc.WebListen
	if addr == "" {
		addr = script.DefaultWebListen
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("web server: %w", err)
	}
	r.webAddr = ln.Addr().(*net.TCPAddr).AddrPort()
	srv := &http.Server{Handler: r.webHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return nil
}

func (r *runner) webHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+script.PathState, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.snapshot())
	})
	mux.HandleFunc("POST "+script.PathSignalPrefix+"{name}", func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("name")
		if !signalName.MatchString(name) {
			http.Error(w, "a signal name is a DNS label", http.StatusBadRequest)
			return
		}
		r.signal(name)
		w.WriteHeader(http.StatusNoContent)
	})
	if r.sc.Mode == script.ModeCisco {
		mux.HandleFunc("GET "+scrape.PathDeviceInformation, r.page(func(web script.Web) ([]byte, error) {
			return scrape.MarshalPage(scrape.DeviceInformationX{
				VersionID: web.VersionID, AppLoadID: web.AppLoadID, BootLoadID: web.BootLoadID,
				HardwareRevision: web.HardwareRevision, SerialNumber: web.SerialNumber, ModelNumber: web.ModelNumber,
			})
		}))
		mux.HandleFunc("GET "+scrape.PathNetworkConfiguration, r.page(func(web script.Web) ([]byte, error) {
			b, err := scrape.MarshalPage(scrape.NetworkConfigurationX{AdminVLANID: web.AdminVLAN, CallManager1: web.CallManager1})
			if err != nil || !web.NetworkConfigurationBreaksOff {
				return b, err
			}
			return breakOff(b), nil
		}))
		mux.HandleFunc("POST /CGI/Execute", r.execute)
		mux.HandleFunc("GET "+scrape.PathDeviceLog, r.page(func(web script.Web) ([]byte, error) {
			if web.EmptyDeviceLog {
				return nil, nil
			}
			return scrape.MarshalPage(scrape.DeviceLogX{Status: web.Log})
		}))
	}
	return mux
}

// crashTail is where a real CP-7945G on SIP 9.4(2)SR4-3 ends its
// /NetworkConfigurationX (2026-10-15): its renderer throws while it
// writes this element, and the page stops there.
const crashTail = "<EnergywisePowerLevel>java.lang.NullPointerException"

// breakOff ends a marshalled /NetworkConfigurationX as that phone does:
// its fields, then crashTail in place of the closing tag.
func breakOff(page []byte) []byte {
	if i := bytes.LastIndex(page, []byte("</NetworkConfiguration>")); i >= 0 {
		page = page[:i:i]
	}
	return append(page, crashTail...)
}

// page serves one scraped page from the current Web, or 503 while the web
// server is down or before any Web was set.
func (r *runner) page(build func(script.Web) ([]byte, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		web, set := cloneWeb(r.state.Web), r.webSet
		r.mu.Unlock()
		if !set || web.Down {
			http.Error(w, "the phone's web server is not answering", http.StatusServiceUnavailable)
			return
		}
		b, err := build(web)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write(b)
	}
}

// maxPushForm bounds a push's form; maxPushBody bounds the bodies the
// phone reads back (the authentication answer, the page).
const (
	maxPushForm = 64 << 10
	maxPushBody = 64 << 10
)

// pushAuthorized is the exact body the authenticationURL answers to allow.
const pushAuthorized = "AUTHORIZED"

type executeX struct {
	XMLName xml.Name `xml:"CiscoIPPhoneExecute"`
	Items   []struct {
		URL string `xml:"URL,attr"`
	} `xml:"ExecuteItem"`
}

type textX struct {
	Text string `xml:"Text"`
}

// execute is POST /CGI/Execute as a real 79x5 documents it: it asks the
// SEP file's authenticationURL (UserID, Password, devicename) whether the
// Basic credentials may push, and only on exactly AUTHORIZED takes the
// items and fetches the first http one. The password is passed on and
// never kept.
func (r *runner) execute(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	var authURL string
	if r.config != nil {
		authURL = r.config.Public.AuthenticationURL
	}
	r.mu.Unlock()
	if authURL == "" {
		http.Error(w, "the phone has read no authenticationURL", http.StatusServiceUnavailable)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxPushForm)
	if err := req.ParseForm(); err != nil {
		writeCiscoError(w, http.StatusBadRequest, 1)
		return
	}
	var x executeX
	parseErr := xml.Unmarshal([]byte(req.PostFormValue("XML")), &x) //nolint:gosec // the form is bounded by MaxBytesReader; the target is a fixed struct.
	rec := script.PushRecord{At: time.Now(), Items: []string{}, Status: 4}
	for _, it := range x.Items {
		rec.Items = append(rec.Items, it.URL)
	}
	user, pass, _ := req.BasicAuth()
	if !r.askAuthentication(req.Context(), authURL, user, pass) {
		r.recordPush(rec)
		writeCiscoError(w, http.StatusUnauthorized, 4)
		return
	}
	if parseErr != nil {
		writeCiscoError(w, http.StatusBadRequest, 1)
		return
	}
	rec.Authorized, rec.Status = true, 0
	for _, it := range rec.Items {
		if strings.HasPrefix(it, "http://") || strings.HasPrefix(it, "https://") {
			rec.Fetched = r.fetchText(req.Context(), it)
			break
		}
	}
	r.recordPush(rec)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = io.WriteString(w, `<CiscoIPPhoneResponse><ResponseItem Status="0" Data="" URL=""/></CiscoIPPhoneResponse>`)
}

// askAuthentication GETs authURL with the push's credentials; true only
// for a 200 whose body is exactly AUTHORIZED. A failure is a refusal.
func (r *runner) askAuthentication(ctx context.Context, authURL, user, pass string) bool {
	u, err := url.Parse(authURL)
	if err != nil {
		return false
	}
	q := u.Query()
	q.Set("UserID", user)
	q.Set("Password", pass)
	q.Set("devicename", "SEP"+r.sc.MAC)
	u.RawQuery = q.Encode()
	body, status, err := r.getBounded(ctx, u.String())
	if err != nil {
		r.log.Info("push: authentication check failed", "error", err.Error()) // never the URL: it carries the password
		return false
	}
	return status == http.StatusOK && string(body) == pushAuthorized
}

// fetchText GETs a pushed page and returns its <Text> ("" on any failure).
func (r *runner) fetchText(ctx context.Context, pageURL string) string {
	body, status, err := r.getBounded(ctx, pageURL)
	if err != nil || status != http.StatusOK {
		r.log.Info("push: page fetch failed", "status", status)
		return ""
	}
	var t textX
	if err := xml.Unmarshal(body, &t); err != nil {
		return ""
	}
	return strings.TrimSpace(t.Text)
}

// getBounded GETs u with the run's HTTP client and returns the status and
// up to maxPushBody bytes. The error does not quote u.
func (r *runner) getBounded(ctx context.Context, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, errors.New("not a valid URL")
	}
	req.Header.Set("User-Agent", r.userAgent())
	res, err := r.opts.HTTP.Do(req)
	if err != nil {
		return nil, 0, unwrapURLError(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxPushBody))
	return b, res.StatusCode, err
}

func (r *runner) recordPush(rec script.PushRecord) {
	r.mu.Lock()
	r.state.Pushes = append(r.state.Pushes, rec)
	r.changedLocked()
	r.mu.Unlock()
	r.log.Info("push", "authorized", rec.Authorized, "items", rec.Items, "status", rec.Status)
}

func writeCiscoError(w http.ResponseWriter, code, number int) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `<CiscoIPPhoneError Number="%d"/>`, number)
}
