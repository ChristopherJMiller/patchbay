// Package agent is the PBX pod's sidecar: it senses and acts, and never
// decides (design §5).
//
// When complete it applies compiled bundles (resolve placeholders, swap the
// live directory, reload Asterisk over AMI, verify, roll back on mismatch),
// serves TFTP :69 and HTTP :6970/:8088 from the live directory, scrapes the
// phones' own web pages, and answers the controller on the control API
// (:9440). Its memory is disposable: the controller copies anything that must
// survive into resource status.
//
// Today it serves only the control API's skeleton: health probes, an
// observations document with nothing observed, and actions that answer 501.
package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// Options configures the control API.
type Options struct {
	// Token is the bearer token every /v1 request must carry. Required.
	Token string

	// Ready reports whether the agent is ready (the native sidecar's startup
	// probe). Nil means always ready. Once apply exists, ready means "the
	// first config version has been applied", so Asterisk starts only after
	// valid config exists.
	Ready func() error

	// Now is the clock; nil means time.Now. Tests pin it.
	Now func() time.Time

	// Log receives request errors.
	Log logr.Logger
}

// NewHandler returns the control API handler.
func NewHandler(opts Options) (http.Handler, error) {
	if opts.Token == "" {
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
	mux.Handle("GET "+api.PathObservations, s.auth(http.HandlerFunc(s.observations)))
	mux.Handle("POST "+api.PathActionResync, s.auth(http.HandlerFunc(s.notImplemented)))
	mux.Handle("POST "+api.PathActionRestart, s.auth(http.HandlerFunc(s.notImplemented)))
	return mux, nil
}

type server struct {
	opts    Options
	started time.Time
}

// auth requires "Authorization: Bearer <token>", compared in constant time.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="patchbay-agent"`)
			writeJSON(w, http.StatusUnauthorized, api.Error{Error: "missing or wrong agent token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) observations(w http.ResponseWriter, _ *http.Request) {
	doc := api.Empty(s.opts.Now().UTC(), api.AgentStatus{
		Version:   version.Version,
		StartedAt: s.started.UTC(),
	})
	writeJSON(w, http.StatusOK, doc)
}

func (s *server) notImplemented(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, api.Error{Error: r.URL.Path + " is not implemented yet"})
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

// Serve runs the control API on addr until ctx is done, then shuts down
// gracefully.
func Serve(ctx context.Context, addr string, h http.Handler, log logr.Logger) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("agent: listen on %s: %w", addr, err)
	}
	return serveOn(ctx, ln, h, log)
}

func serveOn(ctx context.Context, ln net.Listener, h http.Handler, log logr.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
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
