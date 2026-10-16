package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
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
