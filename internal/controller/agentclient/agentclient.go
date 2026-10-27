// Package agentclient is the controller's client of the agent's control API
// (docs/plans/release-a.md W3.4, §7.1): GET /v1/observations for the
// poller, POST /v1/actions/{resync,restart} for the device reconcilers and
// POST /v1/actions/push for the Message reconciler (agentview.Actor).
//
// Every request carries the agent token as a bearer token. The agent
// re-reads its token file when the controller regenerates the Secret, but
// the kubelet refreshes a Secret volume only every minute or so, so for a
// while after a rotation the agent knows only the old token: a 401 is
// retried once with the token the current one replaced (TokenSource).
//
// Requests are bounded: observations time out after 5 s and actions after
// 10 s, a response body may hold at most 16 MiB (observations) or 64 KiB
// (actions, errors), and redirects are never followed. No error or log line
// holds a token.
package agentclient

import (
	"bytes"
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

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// Limits.
const (
	// ObservationsTimeout bounds one GET /v1/observations (both tries).
	ObservationsTimeout = 5 * time.Second
	// ActionTimeout bounds one action POST (both tries).
	ActionTimeout = 10 * time.Second
	// MaxObservationsSize is the largest observations document read.
	MaxObservationsSize = 16 << 20
	// MaxActionResponseSize is the largest action answer, or error body,
	// read.
	MaxActionResponseSize = 64 << 10
)

// The timeouts in use: the constants, shortened by tests.
var (
	observationsTimeout = ObservationsTimeout
	actionTimeout       = ActionTimeout
)

// errorSnippet is how much of a body that is not an api.Error an error
// quotes.
const errorSnippet = 200

// TokenSource gives the agent token: the current one, and the one it
// replaced ("" if none). secrets.Manager implements it.
type TokenSource interface {
	Tokens(ctx context.Context) (current, previous string, err error)
}

// Config configures a Client.
type Config struct {
	// URL is the control API's base URL:
	// install.AgentURL(names, namespace, api.DefaultPort).
	URL string
	// Tokens gives the bearer token.
	Tokens TokenSource
	// HTTP sends the requests; its redirect policy is replaced so that
	// redirects are never followed. Nil: a client with no proxy, a 5 s dial
	// timeout, and the per-call timeouts above.
	HTTP *http.Client
	// Log is the client's logger.
	Log logr.Logger
}

// Client calls the agent's control API.
type Client struct {
	base   *url.URL
	tokens TokenSource
	http   *http.Client
	log    logr.Logger
}

// New checks the configuration and returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("agentclient: URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("agentclient: URL %q is not an http(s)://host[:port] base URL", cfg.URL)
	}
	if cfg.Tokens == nil {
		return nil, errors.New("agentclient: no token source")
	}
	var hc http.Client
	if cfg.HTTP != nil {
		hc = *cfg.HTTP
	} else {
		hc.Transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          2,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: ActionTimeout,
		}
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	u.Path = strings.TrimRight(u.Path, "/")
	return &Client{base: u, tokens: cfg.Tokens, http: &hc, log: cfg.Log}, nil
}

// Observations fetches the agent's observations document.
func (c *Client) Observations(ctx context.Context) (agentapi.Observations, error) {
	ctx, cancel := context.WithTimeout(ctx, observationsTimeout)
	defer cancel()
	var doc agentapi.Observations
	if err := c.do(ctx, http.MethodGet, agentapi.PathObservations, nil, MaxObservationsSize, &doc); err != nil {
		return agentapi.Observations{}, err
	}
	return doc, nil
}

// Resync asks the agent to make a phone re-read its config. An answer
// with Sent=false comes back with a nil error.
func (c *Client) Resync(ctx context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error) {
	return c.action(ctx, agentapi.PathActionResync, req)
}

// Restart asks the agent to reboot a phone. An answer with Sent=false comes
// back with a nil error.
func (c *Client) Restart(ctx context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error) {
	return c.action(ctx, agentapi.PathActionRestart, req)
}

// Push asks the agent to store a message page for a phone and push it (or
// to clear the phone's screen). An answer with Sent=false (the phone
// refused, or could not be reached) comes back with a nil error.
func (c *Client) Push(ctx context.Context, req agentapi.PushRequest) (agentapi.PushResponse, error) {
	return post[agentapi.PushResponse](ctx, c, agentapi.PathActionPush, req)
}

func (c *Client) action(ctx context.Context, path string, req agentapi.ActionRequest) (agentapi.ActionResponse, error) {
	return post[agentapi.ActionResponse](ctx, c, path, req)
}

// post sends an action: req as JSON, the answer decoded into a Resp, within
// actionTimeout.
func post[Resp any](ctx context.Context, c *Client, path string, req any) (Resp, error) {
	var zero Resp
	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return zero, fmt.Errorf("agentclient: encoding the action: %w", err)
	}
	var resp Resp
	if err := c.do(ctx, http.MethodPost, path, body, MaxActionResponseSize, &resp); err != nil {
		return zero, err
	}
	return resp, nil
}

// do sends one request with the current token, and once more with the
// previous one if the agent answers 401, then decodes a 200's body into
// out.
func (c *Client) do(ctx context.Context, method, path string, body []byte, limit int64, out any) error {
	current, previous, err := c.tokens.Tokens(ctx)
	if err != nil {
		return fmt.Errorf("agentclient: the agent token: %w", err)
	}
	what := method + " " + path
	status, raw, err := c.send(ctx, method, path, body, current, limit)
	if err == nil && status == http.StatusUnauthorized && previous != "" && previous != current {
		c.log.V(1).Info("the agent refused the current token; trying the previous one", "request", what)
		status, raw, err = c.send(ctx, method, path, body, previous, limit)
	}
	if err != nil {
		return fmt.Errorf("agentclient: %s: %s", what, redact(err.Error(), current, previous))
	}
	if status != http.StatusOK {
		return fmt.Errorf("agentclient: %s: %d %s: %s", what, status, http.StatusText(status),
			redact(errorMessage(raw), current, previous))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("agentclient: %s: decoding the answer: %w", what, err)
	}
	return nil
}

// send makes one request and reads the answer's body: up to limit bytes for
// a 200, up to MaxActionResponseSize otherwise.
func (c *Client) send(ctx context.Context, method, path string, body []byte, token string, limit int64) (int, []byte, error) {
	u := *c.base
	u.Path += path
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		limit = MaxActionResponseSize
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the answer: %w", err)
	}
	if int64(len(raw)) > limit {
		return 0, nil, fmt.Errorf("the answer (%d) is larger than %d bytes", resp.StatusCode, limit)
	}
	return resp.StatusCode, raw, nil
}

// errorMessage is the agent's api.Error message, or a quoted snippet of a
// body that is not one.
func errorMessage(raw []byte) string {
	var e agentapi.Error
	if err := json.Unmarshal(raw, &e); err == nil && e.Error != "" {
		return e.Error
	}
	if len(raw) == 0 {
		return "(no body)"
	}
	if len(raw) > errorSnippet {
		raw = raw[:errorSnippet]
	}
	return fmt.Sprintf("%q", raw)
}

// redact removes the tokens from a message, so an agent or proxy that
// echoes a header can never put one into an error.
func redact(msg string, tokens ...string) string {
	for _, t := range tokens {
		if t != "" {
			msg = strings.ReplaceAll(msg, t, "[token]")
		}
	}
	return msg
}
