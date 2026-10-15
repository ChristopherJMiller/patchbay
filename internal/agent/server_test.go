package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

const token = "test-token"

var pinned = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func newTestServer(t *testing.T, ready func() error) *httptest.Server {
	t.Helper()
	h, err := NewHandler(Options{Tokens: StaticToken(token), Ready: ready, Now: func() time.Time { return pinned }, Log: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, bearer string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, strings.NewReader(`{"device":"desk"}`))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestNewHandlerRequiresToken(t *testing.T) {
	if _, err := NewHandler(Options{}); err == nil {
		t.Fatal("NewHandler without a token: want an error")
	}
}

func TestControlAPI(t *testing.T) {
	ts := newTestServer(t, nil)
	for _, tc := range []struct {
		name, method, path, bearer string
		want                       int
	}{
		{"healthz needs no token", http.MethodGet, api.PathHealthz, "", http.StatusOK},
		{"readyz needs no token", http.MethodGet, api.PathReadyz, "", http.StatusOK},
		{"observations without token", http.MethodGet, api.PathObservations, "", http.StatusUnauthorized},
		{"observations with wrong token", http.MethodGet, api.PathObservations, "nope", http.StatusUnauthorized},
		{"observations", http.MethodGet, api.PathObservations, token, http.StatusOK},
		{"observations is GET only", http.MethodPost, api.PathObservations, token, http.StatusMethodNotAllowed},
		{"resync without token", http.MethodPost, api.PathActionResync, "", http.StatusUnauthorized},
		{"resync not implemented", http.MethodPost, api.PathActionResync, token, http.StatusNotImplemented},
		{"restart not implemented", http.MethodPost, api.PathActionRestart, token, http.StatusNotImplemented},
		{"resync is POST only", http.MethodGet, api.PathActionResync, token, http.StatusMethodNotAllowed},
		{"unknown path", http.MethodGet, "/v1/nothing", token, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, body := do(t, ts, tc.method, tc.path, tc.bearer); got != tc.want {
				t.Fatalf("%s %s = %d (%s), want %d", tc.method, tc.path, got, body, tc.want)
			}
		})
	}
}

func TestObservationsIsAValidEmptyDocument(t *testing.T) {
	ts := newTestServer(t, nil)
	code, body := do(t, ts, http.MethodGet, api.PathObservations, token)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	var doc api.Observations
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if !doc.GeneratedAt.Equal(pinned) || !doc.Agent.StartedAt.Equal(pinned) {
		t.Errorf("times: generatedAt %v, startedAt %v; want %v", doc.GeneratedAt, doc.Agent.StartedAt, pinned)
	}
	if doc.Devices == nil || len(doc.Devices) != 0 || doc.UnknownDevices == nil || doc.Firmware.Files == nil {
		t.Errorf("want every list present and empty, got %s", body)
	}
}

func TestReadyzReportsNotReady(t *testing.T) {
	ts := newTestServer(t, func() error { return errors.New("no config applied yet") })
	code, body := do(t, ts, http.MethodGet, api.PathReadyz, "")
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "no config applied yet") {
		t.Fatalf("readyz = %d %q, want 503 with the reason", code, body)
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Options{Tokens: StaticToken(token)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveOn(ctx, ln, h, logr.Discard()) }()

	resp, err := http.Get("http://" + ln.Addr().String() + api.PathHealthz)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after cancel, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop within 15s of cancel")
	}
}

// TestServeCapsConnections: the control API holds at most
// MaxControlConnsPerSource connections from one address and
// MaxControlConns in all (the W2 review's caps, now on :9440 too); a
// connection over either is closed when accepted, and one closed frees
// its place. The node's own loopback is outside the total: the kubelet's
// probes (host 127.0.0.1) and the CLI's port-forward still get in while
// LAN hosts hold every other place (the final review's probe starvation).
func TestServeCapsConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Options{Tokens: StaticToken(token)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveOn(ctx, ln, h, logr.Discard()) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// held opens a keep-alive connection from source and completes one
	// request on it, so the server holds it idle; it reports whether the
	// server answered.
	held := func(source, path string) (net.Conn, bool) {
		t.Helper()
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}, Timeout: 5 * time.Second}
		c, err := d.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial from %s: %v", source, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: agent\r\n\r\n"); err != nil {
			return c, false
		}
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		return c, err == nil && strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200")
	}

	const lan = "127.0.0.7" // any address but the node's loopback is a LAN host here
	var first net.Conn
	for i := range MaxControlConnsPerSource {
		c, ok := held(lan, api.PathHealthz)
		if !ok {
			t.Fatalf("connection %d from one source was refused; the cap is %d", i+1, MaxControlConnsPerSource)
		}
		if i == 0 {
			first = c
		}
	}
	if _, ok := held(lan, api.PathHealthz); ok {
		t.Fatalf("connection %d from one source was served", MaxControlConnsPerSource+1)
	}
	_ = first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := held(lan, api.PathHealthz); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a closed connection's place was not freed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Other sources fill the total; then no other LAN host gets in.
	open := MaxControlConnsPerSource
	for src := 10; open < MaxControlConns; src++ {
		for i := 0; i < MaxControlConnsPerSource && open < MaxControlConns; i++ {
			if _, ok := held("127.0.0."+strconv.Itoa(src), api.PathHealthz); !ok {
				t.Fatalf("connection %d of %d was refused", open+1, MaxControlConns)
			}
			open++
		}
	}
	if _, ok := held("127.0.0.99", api.PathHealthz); ok {
		t.Fatalf("connection %d in all was served", MaxControlConns+1)
	}
	// The kubelet still probes, from the node's loopback.
	for _, path := range []string{api.PathReadyz, api.PathHealthz} {
		if _, ok := held("127.0.0.1", path); !ok {
			t.Errorf("a probe of %s from 127.0.0.1 was refused while LAN hosts held the total", path)
		}
	}
}
