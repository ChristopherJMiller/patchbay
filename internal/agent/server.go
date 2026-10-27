// Package agent is the PBX pod's sidecar: it senses and acts, and never
// decides (design §5).
//
// It applies compiled bundles (resolve placeholders, swap the live
// directory, reload Asterisk over AMI, verify, roll back on mismatch),
// serves device HTTP :8088 from the live directory, scrapes the phones' own
// web pages, records what the separate file server (TFTP :69, HTTP :6970)
// reports, and answers the controller on the control API (:9440). Its
// memory is disposable: the controller copies anything that must survive
// into resource status.
//
// This package is the control API: the HTTP layer over the components in
// its subpackages, which cmd/patchbay wires together (docs/plans/release-a.md,
// W2 task level, has the package map). Each seam is an Options field; a nil
// one keeps the skeleton's answer (an empty observations document, 501 for
// actions and uploads, no /metrics).
//
// Notes for the components behind it:
//   - The live directory's layout is in internal/compile/layout.go and
//     internal/agent/layout. Serve each tree from its own root
//     (layout.OpenTree), never the whole volume: asterisk/ holds every
//     password.
//   - The control API token is re-read when its Secret changes (TokenFile).
//     The AMI password lives in manager.conf, which apply's module reloads do
//     not touch: when the compiled AMI user changes, apply must also run
//     `manager reload` over the session it already holds, then reconnect
//     with the new password.
package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/netlimit"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// Options configures the control API.
type Options struct {
	// Tokens returns the bearer tokens a /v1 request may carry right now.
	// Required. A TokenFile re-reads the mounted token Secret, so a
	// regenerated token takes effect without a restart; StaticToken is for
	// tests and the environment.
	Tokens func() []string

	// Ready reports whether the agent is ready (the native sidecar's startup
	// probe). Nil means always ready. Ready means "the first config version
	// has been applied and the sound prompts are installed", so Asterisk
	// starts only after valid config exists.
	Ready func() error

	// Observations builds GET /v1/observations' document (the observation
	// store's Document). Nil serves api.Empty. Whatever it returns, the
	// server sets GeneratedAt (now), Agent.Version and Agent.StartedAt
	// (this process's), and turns nil lists into empty ones, so the
	// document never holds null for a list.
	Observations func(now time.Time) api.Observations

	// Actions sends the service-control NOTIFYs behind POST
	// /v1/actions/{resync,restart}. Nil answers 501.
	Actions Actions

	// Push handles POST /v1/actions/push: it stores a phone's message page
	// and pushes it (internal/agent/push). The server has already decoded
	// the request and checked that Device is set; every refusal is Sent
	// false with the reason. Nil answers 501.
	Push Pusher

	// Firmware handles PUT /v1/firmware/{load}/{name} once the request is
	// authenticated (the firmware store's upload handler, which reads the
	// path values "load" and "name" and answers with an
	// api.FirmwareUploadResponse). Nil answers 501.
	Firmware http.Handler

	// Metrics serves GET /metrics (Prometheus), without a token like the
	// probes: it carries counts and version digests, never a secret. Nil
	// leaves /metrics unrouted (404).
	Metrics http.Handler

	// Now is the clock; nil means time.Now. Tests pin it.
	Now func() time.Time

	// Log receives request errors.
	Log logr.Logger
}

// Actions sends a Cisco service-control NOTIFY to a device's contact
// (design §5, Actions). The server has already decoded the request and
// checked that Device is set; an unknown device, a device with no contact
// or no Call-ID, AMI being down or Asterisk refusing are all answered with
// Sent false and the reason, never with an error status.
type Actions interface {
	// Resync sends action=restart (re-read config).
	Resync(ctx context.Context, req api.ActionRequest) api.ActionResponse
	// Restart sends action=reset (reboot).
	Restart(ctx context.Context, req api.ActionRequest) api.ActionResponse
}

// Pusher pushes a message page, a clear or raw Execute URIs to a phone.
type Pusher interface {
	Push(ctx context.Context, req api.PushRequest) api.PushResponse
}

// maxActionBody is the largest action request body the server reads.
const maxActionBody = 4 << 10

// NewHandler returns the control API handler.
func NewHandler(opts Options) (http.Handler, error) {
	if opts.Tokens == nil || !slices.ContainsFunc(opts.Tokens(), func(t string) bool { return t != "" }) {
		return nil, errors.New("agent: a control API token is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Ready == nil {
		opts.Ready = func() error { return nil }
	}
	s := &server{opts: opts, started: opts.Now()}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathHealthz, func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET "+api.PathReadyz, func(w http.ResponseWriter, _ *http.Request) {
		if err := s.opts.Ready(); err != nil {
			writeText(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeText(w, http.StatusOK, "ok")
	})
	if opts.Metrics != nil {
		mux.Handle("GET "+api.PathMetrics, opts.Metrics)
	}
	mux.Handle("GET "+api.PathObservations, s.auth(http.HandlerFunc(s.observations)))
	mux.Handle("POST "+api.PathActionResync, s.auth(s.action(func(a Actions) actionFunc { return a.Resync })))
	mux.Handle("POST "+api.PathActionRestart, s.auth(s.action(func(a Actions) actionFunc { return a.Restart })))
	mux.Handle("POST "+api.PathActionPush, s.auth(http.HandlerFunc(s.push)))
	mux.Handle("PUT "+api.PathFirmware+"/{load}/{name}", s.auth(http.HandlerFunc(s.firmware)))
	return mux, nil
}

type server struct {
	opts    Options
	started time.Time
}

// auth requires "Authorization: Bearer <token>" with a token that
// Options.Tokens accepts now.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !accepted(got, s.opts.Tokens()) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="patchbay-agent"`)
			writeJSON(w, http.StatusUnauthorized, api.Error{Error: "missing or wrong agent token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// accepted reports whether got is one of tokens. Every token is compared in
// constant time; an empty token never matches.
func accepted(got string, tokens []string) bool {
	match := 0
	for _, t := range tokens {
		if t != "" {
			match |= subtle.ConstantTimeCompare([]byte(got), []byte(t))
		}
	}
	return match == 1
}

func (s *server) observations(w http.ResponseWriter, _ *http.Request) {
	now := s.opts.Now()
	agent := api.AgentStatus{Version: version.Version, StartedAt: s.started.UTC()}
	doc := api.Empty(now.UTC(), agent)
	if s.opts.Observations != nil {
		doc = s.opts.Observations(now)
		doc.GeneratedAt = now.UTC()
		doc.Agent.Version, doc.Agent.StartedAt = agent.Version, agent.StartedAt
		if doc.Devices == nil {
			doc.Devices = []api.DeviceObservations{}
		}
		if doc.UnknownDevices == nil {
			doc.UnknownDevices = []api.UnknownDevice{}
		}
		if doc.Firmware.Files == nil {
			doc.Firmware.Files = []api.FirmwareFileStatus{}
		}
	}
	writeJSON(w, http.StatusOK, doc)
}

type actionFunc func(context.Context, api.ActionRequest) api.ActionResponse

// action decodes an ActionRequest (at most maxActionBody bytes, no unknown
// fields, Device set: else 400) and answers 200 with what the action
// returns.
func (s *server) action(pick func(Actions) actionFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Actions == nil {
			writeJSON(w, http.StatusNotImplemented, api.Error{Error: r.URL.Path + " is not implemented yet"})
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxActionBody))
		dec.DisallowUnknownFields()
		var req api.ActionRequest
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, api.Error{Error: "decoding the action request: " + err.Error()})
			return
		}
		if req.Device == "" {
			writeJSON(w, http.StatusBadRequest, api.Error{Error: "the action request names no device"})
			return
		}
		writeJSON(w, http.StatusOK, pick(s.opts.Actions)(r.Context(), req))
	})
}

// push decodes a PushRequest (at most api.MaxPushBody bytes, no unknown
// fields, Device set: else 400) and answers 200 with what the pusher
// returns.
func (s *server) push(w http.ResponseWriter, r *http.Request) {
	if s.opts.Push == nil {
		writeJSON(w, http.StatusNotImplemented, api.Error{Error: r.URL.Path + " is not implemented yet"})
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, api.MaxPushBody))
	dec.DisallowUnknownFields()
	var req api.PushRequest
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "decoding the push request: " + err.Error()})
		return
	}
	if req.Device == "" {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "the push request names no device"})
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Push.Push(r.Context(), req))
}

func (s *server) firmware(w http.ResponseWriter, r *http.Request) {
	// An authenticated upload may take api.FirmwareUploadTimeout (the CLI's
	// budget for one file), not controlTimeout: a 4.6 MB image over a slow
	// port-forward would otherwise be cut mid-body.
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(firmwareUploadTimeout)
	if err := errors.Join(rc.SetReadDeadline(deadline), rc.SetWriteDeadline(deadline)); err != nil {
		s.opts.Log.Info("firmware upload: cannot extend the deadline", "error", err.Error())
	}
	if s.opts.Firmware == nil {
		writeJSON(w, http.StatusNotImplemented, api.FirmwareUploadResponse{
			Load: r.PathValue("load"), Name: r.PathValue("name"), Error: "firmware upload is not implemented yet",
		})
		return
	}
	s.opts.Firmware.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, msg)
}

// The control API's connection caps (package netlimit). It listens on the
// host network, where anything that reaches the node can connect before a
// token is checked, and each connection costs the agent (128 MiB) its
// buffers and a goroutine. The controller keeps one or two keep-alive
// connections from its pod's address. The node's loopback (127.0.0.1, ::1)
// is outside the total, with its own per-source cap: the kubelet probes
// there (the chart's probes set host 127.0.0.1) and the CLI's
// port-forward arrives there, so LAN hosts holding every other place can
// neither fail the probes, which would have the kubelet restart the agent
// again and again, nor lock out the operator.
const (
	MaxControlConns          = 64
	MaxControlConnsPerSource = 16
)

// controlTimeout bounds reading a request (body included) and writing its
// answer; firmwareUploadTimeout replaces it for an authenticated firmware
// upload. Variables so tests can shorten them.
var (
	controlTimeout        = 30 * time.Second
	firmwareUploadTimeout = api.FirmwareUploadTimeout
)

// ControlIdleTimeout is how long the control API keeps an idle keep-alive
// connection: the controller polls every few seconds and reuses its own,
// and a place held by a host that sent one request is freed soon.
const ControlIdleTimeout = 15 * time.Second

// Serve runs the control API on addr until ctx is done, then shuts down
// gracefully. It holds at most MaxControlConns connections, and at most
// MaxControlConnsPerSource from any one source: a connection over either
// cap is closed as soon as it is accepted. The node's loopback is outside
// the total (see MaxControlConns).
func Serve(ctx context.Context, addr string, h http.Handler, log logr.Logger) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("agent: listen on %s: %w", addr, err)
	}
	return serveOn(ctx, ln, h, log)
}

// serveOn serves h on ln, within the connection caps, until ctx is done.
func serveOn(ctx context.Context, ln net.Listener, h http.Handler, log logr.Logger) error {
	ln = netlimit.New(ln, MaxControlConns, MaxControlConnsPerSource, log).ExemptLoopback()
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       controlTimeout,
		WriteTimeout:      controlTimeout,
		IdleTimeout:       ControlIdleTimeout,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("control API listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("agent: control API: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("agent: shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("agent: control API: %w", err)
	}
	return nil
}
