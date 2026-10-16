// Package bridgeclient is the controller's client of the stream bridge's
// control API (docs/plans/release-b.md RT.4, §7.2): GET /v1/observations,
// which the stream poller (internal/controller/streamstate) calls every
// few seconds.
//
// The control API needs no token (§2(g)): the document holds names,
// numbers, sources (which hold no credentials) and bounded text. Requests
// are bounded all the same: each times out after RequestTimeout, a
// response body may hold at most MaxObservationsSize bytes, redirects are
// never followed, and no proxy is used. Errors quote the status and at
// most errorSnippet printable bytes of an error body, never more.
package bridgeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

// Limits.
const (
	// RequestTimeout bounds one GET of the observations.
	RequestTimeout = 5 * time.Second
	// MaxObservationsSize is the largest observations document read. The
	// bridge's document for 1000 streams (streams.MaxStreams) is well under
	// 1 MiB.
	MaxObservationsSize = 4 << 20
)

// requestTimeout is RequestTimeout, shortened by tests.
var requestTimeout = RequestTimeout

// errorSnippet is how much of a non-200 body an error quotes.
const errorSnippet = 200

// Config configures a Client.
type Config struct {
	// URL is the control API's base URL:
	// install.StreamBridgeURL(names, namespace, bridgeapi.DefaultControlPort).
	URL string
	// HTTP sends the requests; its redirect policy is replaced so that
	// redirects are never followed. Nil: a client with no proxy and a 5 s
	// dial timeout.
	HTTP *http.Client
	// Log is the client's logger.
	Log logr.Logger
}

// Client calls the stream bridge's control API.
type Client struct {
	observations string // the observations URL
	http         *http.Client
	log          logr.Logger
}

// New checks the configuration and returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("bridgeclient: URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("bridgeclient: URL %q is not an http(s)://host[:port] base URL", cfg.URL)
	}
	var hc http.Client
	if cfg.HTTP != nil {
		hc = *cfg.HTTP
	} else {
		hc.Transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          1,
			MaxIdleConnsPerHost:   1,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: RequestTimeout,
		}
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	u.Path = strings.TrimRight(u.Path, "/") + bridgeapi.PathObservations
	return &Client{observations: u.String(), http: &hc, log: cfg.Log}, nil
}

// Observations fetches the bridge's observations document. A list the
// document leaves null is empty.
func (c *Client) Observations(ctx context.Context) (bridgeapi.Observations, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.observations, nil)
	if err != nil {
		return bridgeapi.Observations{}, fmt.Errorf("bridgeclient: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return bridgeapi.Observations{}, fmt.Errorf("bridgeclient: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	what := "GET " + bridgeapi.PathObservations
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorSnippet+1))
		c.log.V(1).Info("the stream bridge answered an error", "status", resp.StatusCode)
		return bridgeapi.Observations{}, fmt.Errorf("bridgeclient: %s: %d %s%s", what, resp.StatusCode, http.StatusText(resp.StatusCode), snippet(raw))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxObservationsSize+1))
	if err != nil {
		return bridgeapi.Observations{}, fmt.Errorf("bridgeclient: %s: reading the answer: %w", what, err)
	}
	if len(raw) > MaxObservationsSize {
		return bridgeapi.Observations{}, fmt.Errorf("bridgeclient: %s: the answer is larger than %d bytes", what, MaxObservationsSize)
	}
	var doc bridgeapi.Observations
	if err := json.Unmarshal(raw, &doc); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return bridgeapi.Observations{}, fmt.Errorf("bridgeclient: %s: the answer is not JSON (at byte %d)", what, syntax.Offset)
		}
		return bridgeapi.Observations{}, fmt.Errorf("bridgeclient: %s: decoding the answer: %w", what, err)
	}
	if doc.Streams == nil {
		doc.Streams = []bridgeapi.StreamObservations{}
	}
	if doc.Pools == nil {
		doc.Pools = []bridgeapi.PoolObservations{}
	}
	for i := range doc.Pools {
		if doc.Pools[i].Sessions == nil {
			doc.Pools[i].Sessions = []bridgeapi.PoolSession{}
		}
	}
	return doc, nil
}

// snippet is ": <body>", printable and bounded, or "" for an empty body.
func snippet(raw []byte) string {
	s := strings.TrimSpace(install.Text(string(raw), errorSnippet))
	if s == "" {
		return ""
	}
	return ": " + s
}
