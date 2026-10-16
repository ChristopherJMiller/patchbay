package streambridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// get is a request to the control API's handler.
func get(t *testing.T, h http.Handler, method, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code, rec.Body.String()
}

func TestObservationsDocument(t *testing.T) {
	pool := streams.Pool{Name: "hdhomerun", Max: 2}
	src := newFakeSource().set(srcKiro, silent())
	m, reg := testManager(t, src, nil, doc(t, []streams.Pool{pool}, king5, king5Spare, kiro7, komo4, groovesalad))
	king := join(t, m, king5) // Playing
	popN(t, king, 1)
	join(t, m, kiro7)                                        // Starting: its source is silent
	m.leave(join(t, m, groovesalad))                         // Draining
	if _, err := m.join(streams.UUID("komo4")); err == nil { // refused: the pool runs two
		t.Fatal("komo4 joined a full pool")
	}
	eventually(t, "radio to start", func() bool { return src.started(srcRadio) == 1 })

	code, body := get(t, newControl(m, reg, false).handler(), http.MethodGet, api.PathObservations)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	for _, null := range []string{`"streams":null`, `"pools":null`, `"sessions":null`} {
		if strings.Contains(strings.ReplaceAll(body, " ", ""), null) {
			t.Errorf("a null list: %s", null)
		}
	}
	var o api.Observations
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range o.Streams {
		names = append(names, s.Name)
	}
	if strings.Join(names, " ") != "groovesalad king5 king5-spare kiro7 komo4" {
		t.Errorf("streams %v, want sorted by name", names)
	}
	byName := map[string]api.StreamObservations{}
	for _, s := range o.Streams {
		byName[s.Name] = s
	}
	k := byName["king5"]
	if k.UUID != streams.UUID("king5") || k.Number != "705" || k.Source != srcKing || k.Pool != "hdhomerun" || k.Listeners != 1 ||
		k.Session == nil || k.Session.State != api.SessionPlaying || k.Session.FirstAudioAt == nil || k.LastPlayed == nil || k.Calls != 1 {
		t.Errorf("king5: %+v %+v", k, k.Session)
	}
	if s := byName["king5-spare"]; s.Session == nil || s.Session.Listeners != 1 || s.Listeners != 0 || s.LastPlayed != nil {
		t.Errorf("king5-spare shares king5's session: %+v %+v", s, s.Session)
	}
	if s := byName["kiro7"]; s.Session == nil || s.Session.State != api.SessionStarting || s.Session.FirstAudioAt != nil {
		t.Errorf("kiro7: %+v", s.Session)
	}
	if s := byName["groovesalad"]; s.Session == nil || s.Session.State != api.SessionDraining || s.Listeners != 0 || s.Pool != "" {
		t.Errorf("groovesalad: %+v %+v", s, s.Session)
	}
	if f := byName["komo4"].LastFailure; f == nil || f.Reason != api.FailureTunerBusy ||
		f.Message != "pool hdhomerun (2 at a time) is playing king5 (705), kiro7 (707)" {
		t.Errorf("komo4's failure: %+v", f)
	}
	if len(o.Pools) != 1 || o.Pools[0].Name != "hdhomerun" || o.Pools[0].Max != 2 || len(o.Pools[0].Sessions) != 2 {
		t.Fatalf("pools: %+v", o.Pools)
	}
	if s := o.Pools[0].Sessions; s[0].Source != srcKing || strings.Join(s[0].Streams, ",") != "king5,king5-spare" ||
		s[0].State != api.SessionPlaying || s[0].Listeners != 1 || s[1].Source != srcKiro || s[1].State != api.SessionStarting {
		t.Errorf("pool sessions: %+v", s)
	}
	if o.Bridge.Calls != 0 || o.Bridge.StreamsVersion == "" || o.Bridge.StartedAt.IsZero() || o.Bridge.Version == "" || !o.GeneratedAt.Equal(epoch) {
		t.Errorf("bridge: %+v", o.Bridge)
	}
}

func TestObservationsEmpty(t *testing.T) {
	m, reg := testManager(t, newFakeSource(), nil, streams.Document{})
	_, body := get(t, newControl(m, reg, false).handler(), http.MethodGet, api.PathObservations)
	if !strings.Contains(body, `"streams":[]`) || !strings.Contains(body, `"pools":[]`) {
		t.Errorf("before a streams.json: %s", body)
	}
}

func TestReadyz(t *testing.T) {
	m, reg := testManager(t, newFakeSource(), nil, streams.Document{})
	h := newControl(m, reg, true).handler()
	if code, body := get(t, h, http.MethodGet, api.PathReadyz); code != http.StatusServiceUnavailable || !strings.Contains(body, "no streams.json") {
		t.Errorf("before anything: %d %q", code, body)
	}
	m.setStreamsError("/etc/patchbay/streams/streams.json does not exist")
	if code, body := get(t, h, http.MethodGet, api.PathReadyz); code != http.StatusServiceUnavailable || !strings.Contains(body, "does not exist") {
		t.Errorf("a missing file: %d %q", code, body)
	}
	m.setDocument(doc(t, nil, groovesalad))
	if code, body := get(t, h, http.MethodGet, api.PathReadyz); code != http.StatusServiceUnavailable || !strings.Contains(body, "ffmpeg") {
		t.Errorf("loaded, no ffmpeg yet: %d %q", code, body)
	}
	m.setFFmpeg("", "ffmpeg -version: exec: no such file")
	if code, body := get(t, h, http.MethodGet, api.PathReadyz); code != http.StatusServiceUnavailable || !strings.Contains(body, "no such file") {
		t.Errorf("an unusable ffmpeg: %d %q", code, body)
	}
	m.setFFmpeg("ffmpeg version 8.1.2", "")
	if code, _ := get(t, h, http.MethodGet, api.PathReadyz); code != http.StatusOK {
		t.Errorf("ready: %d", code)
	}
	if code, _ := get(t, newControl(m, reg, false).handler(), http.MethodGet, api.PathReadyz); code != http.StatusOK {
		t.Errorf("a test source needs no ffmpeg: %d", code)
	}
	if code, _ := get(t, h, http.MethodGet, api.PathHealthz); code != http.StatusOK {
		t.Errorf("healthz %d", code)
	}
	if code, _ := get(t, h, http.MethodPost, api.PathObservations); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}
	if code, _ := get(t, h, http.MethodGet, "/v1/actions/stop"); code != http.StatusNotFound {
		t.Errorf("an unknown path: %d", code)
	}
}

// dialFrom connects to addr from the local address ip.
func dialFrom(ip, addr string) (net.Conn, error) {
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}, Timeout: time.Second}
	return d.Dial("tcp", addr)
}

// accepted reports whether the server keeps c open for d (it closes one
// over a cap at once).
func accepted(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestControlCaps(t *testing.T) {
	m, reg := testManager(t, newFakeSource(), nil, doc(t, nil, groovesalad))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- serveControl(ctx, ln, newControl(m, reg, false).handler(), logr.Discard()) }()
	defer func() {
		cancel()
		if err := <-errc; err != nil {
			t.Error(err)
		}
	}()
	addr := ln.Addr().String()
	var held []net.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	hold := func(ip string, n int) {
		t.Helper()
		for i := range n {
			c, err := dialFrom(ip, addr)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, c)
			if !accepted(c, 10*time.Millisecond) {
				t.Fatalf("connection %d from %s was refused", i, ip)
			}
		}
	}
	over := func(ip string) bool {
		t.Helper()
		c, err := dialFrom(ip, addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		return !accepted(c, time.Second)
	}
	hold("127.0.0.2", maxControlConnsPerSource)
	if !over("127.0.0.2") {
		t.Errorf("a source kept more than %d connections", maxControlConnsPerSource)
	}
	for _, ip := range []string{"127.0.0.3", "127.0.0.4", "127.0.0.5"} {
		hold(ip, maxControlConnsPerSource)
	}
	if !over("127.0.0.6") {
		t.Errorf("more than %d connections in all", maxControlConns)
	}
	// The loopback address (probes, a port-forward) is outside the total.
	c, err := dialFrom("127.0.0.1", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if b, _ := io.ReadAll(io.LimitReader(c, 15)); !strings.HasPrefix(string(b), "HTTP/1.1 200") {
		t.Errorf("a probe from loopback with the total full: %q", b)
	}
}
