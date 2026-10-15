package agentclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentclient"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
)

var _ agentview.Actor = (*agentclient.Client)(nil)

const (
	current  = "not-a-secret-current"
	previous = "not-a-secret-previous"
)

type tokens struct {
	current, previous string
	err               error
}

func (t tokens) Tokens(context.Context) (string, string, error) { return t.current, t.previous, t.err }

// agent is a fake control API: it accepts the tokens in accept and records
// every request's Authorization header.
type agent struct {
	t      *testing.T
	accept map[string]bool
	handle http.HandlerFunc // after authentication

	mu    sync.Mutex
	auths []string
}

func (a *agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.auths = append(a.auths, r.Header.Get("Authorization"))
	a.mu.Unlock()
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !a.accept[tok] {
		writeJSON(w, http.StatusUnauthorized, agentapi.Error{Error: "unauthorized"})
		return
	}
	a.handle(w, r)
}

func (a *agent) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// logSink captures every log line.
type logSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logSink) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.b.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 10})
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newClient(t *testing.T, srv *httptest.Server, ts agentclient.TokenSource, log logr.Logger) *agentclient.Client {
	t.Helper()
	c, err := agentclient.New(agentclient.Config{URL: srv.URL, Tokens: ts, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func observationsHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != agentapi.PathObservations {
			t.Errorf("request %s %s, want GET %s", r.Method, r.URL.Path, agentapi.PathObservations)
		}
		writeJSON(w, http.StatusOK, agentapi.Observations{
			Agent:   agentapi.AgentStatus{AppliedVersion: "v1", AsteriskUp: true},
			Devices: []agentapi.DeviceObservations{{Device: "phone-desk", Calls: 1}},
		})
	}
}

func TestBearerToken(t *testing.T) {
	a := &agent{t: t, accept: map[string]bool{current: true}, handle: observationsHandler(t)}
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := newClient(t, srv, tokens{current: current, previous: previous}, logr.Discard())
	doc, err := c.Observations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Agent.AppliedVersion != "v1" || !doc.Agent.AsteriskUp || len(doc.Devices) != 1 || doc.Devices[0].Calls != 1 {
		t.Errorf("document = %+v", doc)
	}
	if got := a.requests(); len(got) != 1 || got[0] != "Bearer "+current {
		t.Errorf("Authorization headers %q, want one with the current token", got)
	}
}

func TestUnauthorizedRetriesWithPrevious(t *testing.T) {
	a := &agent{t: t, accept: map[string]bool{previous: true}, handle: observationsHandler(t)}
	srv := httptest.NewServer(a)
	defer srv.Close()
	log := &logSink{}
	c := newClient(t, srv, tokens{current: current, previous: previous}, log.logger())
	if _, err := c.Observations(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bearer " + current, "Bearer " + previous}
	if got := a.requests(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Authorization headers %q, want %q", got, want)
	}
	if !strings.Contains(log.String(), "previous") {
		t.Errorf("the retry was not logged:\n%s", log)
	}
	for _, tok := range []string{current, previous} {
		if strings.Contains(log.String(), tok) {
			t.Errorf("a token appears in the log:\n%s", log)
		}
	}

	// Actions retry too, with their body sent again.
	a.handle = func(w http.ResponseWriter, r *http.Request) {
		var req agentapi.ActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Device != "phone-desk" {
			t.Errorf("retried action body: %+v, %v", req, err)
		}
		writeJSON(w, http.StatusOK, agentapi.ActionResponse{Sent: true})
	}
	resp, err := c.Resync(context.Background(), agentapi.ActionRequest{Device: "phone-desk", Version: "v2"})
	if err != nil || !resp.Sent {
		t.Fatalf("Resync = %+v, %v", resp, err)
	}
}

func TestUnauthorizedTwiceFails(t *testing.T) {
	a := &agent{t: t, accept: map[string]bool{}, handle: observationsHandler(t)}
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := newClient(t, srv, tokens{current: current, previous: previous}, logr.Discard())
	_, err := c.Observations(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("Observations = %v, want a 401 error with the agent's message", err)
	}
	if len(a.requests()) != 2 {
		t.Errorf("%d requests, want 2 (current, then previous)", len(a.requests()))
	}

	// No previous token, or the same one: one request only.
	for _, prev := range []string{"", current} {
		a.mu.Lock()
		a.auths = nil
		a.mu.Unlock()
		c := newClient(t, srv, tokens{current: current, previous: prev}, logr.Discard())
		if _, err := c.Observations(context.Background()); err == nil {
			t.Fatal("Observations with a refused token passed")
		}
		if n := len(a.requests()); n != 1 {
			t.Errorf("previous %q: %d requests, want 1", prev, n)
		}
	}
}

func TestServerErrorQuotesTheAgent(t *testing.T) {
	a := &agent{t: t, accept: map[string]bool{current: true}, handle: func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, agentapi.Error{Error: "observation store closed"})
	}}
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := newClient(t, srv, tokens{current: current}, logr.Discard())
	_, err := c.Observations(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "observation store closed") {
		t.Fatalf("Observations = %v, want a 500 error quoting the agent", err)
	}

	// A body that is not an api.Error is quoted, shortened.
	a.handle = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>"+strings.Repeat("x", 1000)+"</html>")
	}
	_, err = c.Observations(context.Background())
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "<html>") || len(err.Error()) > 400 {
		t.Fatalf("Observations = %v, want a short 502 error", err)
	}
	a.handle = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	if _, err = c.Observations(context.Background()); err == nil || !strings.Contains(err.Error(), "no body") {
		t.Fatalf("Observations = %v, want a 503 error saying there was no body", err)
	}
	a.handle = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{not json") }
	if _, err = c.Observations(context.Background()); err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("Observations of a broken document = %v, want a decoding error", err)
	}
}

func TestActions(t *testing.T) {
	type call struct {
		method, path, contentType string
		req                       agentapi.ActionRequest
	}
	var got []call
	var mu sync.Mutex
	answer := agentapi.ActionResponse{Sent: false, Error: "no contact"}
	a := &agent{t: t, accept: map[string]bool{current: true}, handle: func(w http.ResponseWriter, r *http.Request) {
		var req agentapi.ActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("action body: %v", err)
		}
		mu.Lock()
		got = append(got, call{r.Method, r.URL.Path, r.Header.Get("Content-Type"), req})
		mu.Unlock()
		writeJSON(w, http.StatusOK, answer)
	}}
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := newClient(t, srv, tokens{current: current}, logr.Discard())
	ctx := context.Background()

	req := agentapi.ActionRequest{Device: "phone-desk", Version: "v2"}
	resp, err := c.Resync(ctx, req)
	if err != nil || resp != answer {
		t.Fatalf("Resync = %+v, %v; want Sent=false with the agent's error and a nil error", resp, err)
	}
	answer = agentapi.ActionResponse{Sent: true}
	resp, err = c.Restart(ctx, req)
	if err != nil || !resp.Sent {
		t.Fatalf("Restart = %+v, %v", resp, err)
	}
	want := []call{
		{http.MethodPost, agentapi.PathActionResync, "application/json", req},
		{http.MethodPost, agentapi.PathActionRestart, "application/json", req},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("calls %+v, want %+v", got, want)
	}

	a.handle = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, agentapi.Error{Error: "no device named"})
	}
	if _, err := c.Resync(ctx, agentapi.ActionRequest{}); err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "no device named") {
		t.Fatalf("Resync answered 400 = %v, want an error quoting the agent", err)
	}
	if got := agentview.ActionFailure(c.Resync(ctx, agentapi.ActionRequest{})); !strings.Contains(got, "400") {
		t.Errorf("ActionFailure = %q", got)
	}
}

func TestTimeouts(t *testing.T) {
	if agentclient.ObservationsTimeout != 5*time.Second || agentclient.ActionTimeout != 10*time.Second {
		t.Fatalf("timeouts %s and %s, want 5s and 10s (plan W3.4)", agentclient.ObservationsTimeout, agentclient.ActionTimeout)
	}
	restore := agentclient.SetTimeouts(100*time.Millisecond, 200*time.Millisecond)
	defer restore()
	release := make(chan struct{})
	a := &agent{t: t, accept: map[string]bool{current: true}, handle: func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}}
	srv := httptest.NewServer(a)
	defer srv.Close()
	defer close(release)
	c := newClient(t, srv, tokens{current: current}, logr.Discard())

	start := time.Now()
	_, err := c.Observations(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) && (err == nil || !strings.Contains(err.Error(), "deadline")) {
		t.Fatalf("Observations of a hung agent = %v, want a deadline error", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Errorf("Observations gave up after %s, want about 100ms", d)
	}
	start = time.Now()
	if _, err := c.Restart(context.Background(), agentapi.ActionRequest{Device: "phone-desk"}); err == nil {
		t.Fatal("Restart of a hung agent passed")
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
		t.Errorf("Restart gave up after %s, want about 200ms", d)
	}

	// A caller's shorter deadline wins.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := c.Resync(ctx, agentapi.ActionRequest{Device: "phone-desk"}); err == nil {
		t.Fatal("Resync past its caller's deadline passed")
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("Resync ignored its caller's deadline (%s)", d)
	}
}

func TestNoRedirects(t *testing.T) {
	hit := make(chan struct{}, 1)
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit <- struct{}{} }))
	defer other.Close()
	a := &agent{t: t, accept: map[string]bool{current: true}, handle: func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+agentapi.PathObservations, http.StatusFound)
	}}
	srv := httptest.NewServer(a)
	defer srv.Close()

	custom := &http.Client{}
	for _, hc := range []*http.Client{nil, custom} {
		c, err := agentclient.New(agentclient.Config{URL: srv.URL, Tokens: tokens{current: current}, HTTP: hc})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Observations(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
			t.Fatalf("a redirect = %v, want a 302 error", err)
		}
	}
	select {
	case <-hit:
		t.Fatal("the redirect was followed (with the bearer token)")
	default:
	}
	if custom.CheckRedirect != nil {
		t.Error("New changed the caller's http.Client")
	}
}

func TestBodyLimits(t *testing.T) {
	size := 0
	a := &agent{t: t, accept: map[string]bool{current: true}, handle: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A valid document padded with white space to size bytes.
		_, _ = io.WriteString(w, `{"sent":true}`)
		_, _ = io.WriteString(w, strings.Repeat(" ", size-len(`{"sent":true}`)))
	}}
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := newClient(t, srv, tokens{current: current}, logr.Discard())
	ctx := context.Background()

	size = agentclient.MaxActionResponseSize
	if _, err := c.Resync(ctx, agentapi.ActionRequest{Device: "d"}); err != nil {
		t.Fatalf("an answer of exactly the limit: %v", err)
	}
	size = agentclient.MaxActionResponseSize + 1
	if _, err := c.Resync(ctx, agentapi.ActionRequest{Device: "d"}); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("an action answer over 64 KiB = %v, want a size error", err)
	}
	size = agentclient.MaxObservationsSize
	if _, err := c.Observations(ctx); err != nil {
		t.Fatalf("observations of exactly the limit: %v", err)
	}
	size = agentclient.MaxObservationsSize + 1
	if _, err := c.Observations(ctx); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("observations over 16 MiB = %v, want a size error", err)
	}
}

func TestNoTokenInErrorsOrLogs(t *testing.T) {
	// An agent (or a proxy in front of it) that echoes the header.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, agentapi.Error{Error: "bad token " + r.Header.Get("Authorization")})
	}))
	log := &logSink{}
	c := newClient(t, srv, tokens{current: current, previous: previous}, log.logger())
	ctx := context.Background()
	var errs []error
	_, err := c.Observations(ctx)
	errs = append(errs, err)
	_, err = c.Resync(ctx, agentapi.ActionRequest{Device: "d"})
	errs = append(errs, err)
	srv.Close()
	_, err = c.Restart(ctx, agentapi.ActionRequest{Device: "d"})
	errs = append(errs, err)
	for _, err := range errs {
		if err == nil {
			t.Fatal("a refused request passed")
		}
		for _, tok := range []string{current, previous} {
			if strings.Contains(err.Error(), tok) {
				t.Errorf("error holds a token: %v", err)
			}
		}
	}
	if !strings.Contains(errs[0].Error(), "[token]") {
		t.Errorf("the echoed token was not redacted: %v", errs[0])
	}
	for _, tok := range []string{current, previous} {
		if strings.Contains(log.String(), tok) {
			t.Errorf("a token appears in the log:\n%s", log)
		}
	}
}

func TestTokenSourceError(t *testing.T) {
	a := &agent{t: t, accept: map[string]bool{current: true}, handle: observationsHandler(t)}
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := newClient(t, srv, tokens{err: errors.New("no agent token Secret yet")}, logr.Discard())
	if _, err := c.Observations(context.Background()); err == nil || !strings.Contains(err.Error(), "no agent token Secret yet") {
		t.Fatalf("Observations = %v, want the token source's error", err)
	}
	if n := len(a.requests()); n != 0 {
		t.Errorf("%d requests without a token, want 0", n)
	}
}

func TestBaseURLWithPath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeJSON(w, http.StatusOK, agentapi.Observations{})
	}))
	defer srv.Close()
	c, err := agentclient.New(agentclient.Config{URL: srv.URL + "/proxy/", Tokens: tokens{current: current}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Observations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/proxy"+agentapi.PathObservations {
		t.Errorf("path %q, want /proxy%s", path, agentapi.PathObservations)
	}
}

func TestNewValidates(t *testing.T) {
	for _, u := range []string{"", "patchbay-agent:9440", "ftp://x", "http://", "http://u:p@x", "http://x?q=1", "http://x#f", "http://%zz"} {
		if _, err := agentclient.New(agentclient.Config{URL: u, Tokens: tokens{}}); err == nil {
			t.Errorf("New(%q) passed", u)
		}
	}
	if _, err := agentclient.New(agentclient.Config{URL: "http://patchbay-agent.telephony.svc:9440"}); err == nil {
		t.Error("New without a token source passed")
	}
	if _, err := agentclient.New(agentclient.Config{URL: "http://patchbay-agent.telephony.svc:9440", Tokens: tokens{}}); err != nil {
		t.Errorf("New with the in-cluster URL: %v", err)
	}
}
