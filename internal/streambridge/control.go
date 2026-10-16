package streambridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ChristopherJMiller/patchbay/internal/agent/netlimit"
	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

// The control API (docs/plans/release-b.md 6.1 BR.6, §7.2): read only, no
// token (§2(g)). The controller polls api.PathObservations; the kubelet
// probes api.PathHealthz and api.PathReadyz; Prometheus scrapes
// api.PathMetrics.
const (
	// maxControlConns and maxControlConnsPerSource cap the control API's
	// connections (package netlimit): the controller keeps one or two, the
	// kubelet and Prometheus a few. Loopback is outside the total.
	maxControlConns          = 32
	maxControlConnsPerSource = 8
	// controlIdleTimeout closes an idle keep-alive connection.
	controlIdleTimeout = 15 * time.Second
	// controlTimeout bounds reading a request and writing its answer.
	controlTimeout = 30 * time.Second
	// controlShutdown is how long a stopping bridge lets requests finish.
	controlShutdown = 5 * time.Second
)

// control serves the control API from m.
type control struct {
	m *manager
	// needFFmpeg: readiness waits for the decoder's version line (the
	// production Source; a test's Source has none).
	needFFmpeg bool
	metrics    http.Handler
}

func newControl(m *manager, g prometheus.Gatherer, needFFmpeg bool) *control {
	return &control{m: m, needFFmpeg: needFFmpeg, metrics: promhttp.HandlerFor(g, promhttp.HandlerOpts{})}
}

// handler routes the control API (GET, and HEAD, only).
func (c *control) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathObservations, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.m.observations())
	})
	mux.HandleFunc("GET "+api.PathHealthz, func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET "+api.PathReadyz, func(w http.ResponseWriter, _ *http.Request) {
		if why := c.notReady(); why != "" {
			writeText(w, http.StatusServiceUnavailable, why)
			return
		}
		writeText(w, http.StatusOK, "ok")
	})
	mux.Handle("GET "+api.PathMetrics, c.metrics)
	return mux
}

// notReady is why the bridge cannot take calls yet, or "": it has loaded
// a streams.json and (with ffmpeg) found its decoder.
func (c *control) notReady() string {
	loaded, streamsErr, ffmpeg, ffmpegErr := c.m.status()
	switch {
	case !loaded && streamsErr != "":
		return "no streams.json loaded yet: " + streamsErr
	case !loaded:
		return "no streams.json loaded yet"
	case c.needFFmpeg && ffmpeg == "" && ffmpegErr != "":
		return "ffmpeg is not usable: " + ffmpegErr
	case c.needFFmpeg && ffmpeg == "":
		return "ffmpeg -version has not run yet"
	}
	return ""
}

func writeText(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, msg)
}

// serveControl serves h on ln, within the connection caps, until ctx ends.
func serveControl(ctx context.Context, ln net.Listener, h http.Handler, log logr.Logger) error {
	ln = netlimit.New(ln, maxControlConns, maxControlConnsPerSource, log).ExemptLoopback()
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       controlTimeout,
		WriteTimeout:      controlTimeout,
		IdleTimeout:       controlIdleTimeout,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("stream-bridge: control API: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlShutdown)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("stream-bridge: control API: %w", err)
	}
	return nil
}
