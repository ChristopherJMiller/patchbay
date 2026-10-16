package bridgeclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/controller/bridgeclient"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

var at = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func document() bridgeapi.Observations {
	return bridgeapi.Observations{
		GeneratedAt: at,
		Bridge:      bridgeapi.BridgeStatus{Version: "v0.2.0", StartedAt: at, StreamsVersion: strings.Repeat("a", 64), Calls: 2},
		Streams: []bridgeapi.StreamObservations{{
			UUID: "89fb6719-3e3d-5ef3-881a-b9f423a8a432", Name: "king5", Number: "705",
			Source: "http://192.168.0.58:5004/auto/v5.1", Pool: "hdhomerun", Listeners: 2,
			Session: &bridgeapi.SessionObservations{State: bridgeapi.SessionPlaying, StartedAt: at, FirstAudioAt: &at, Listeners: 2},
			Calls:   3,
		}},
		Pools: []bridgeapi.PoolObservations{{Name: "hdhomerun", Max: 1, Sessions: []bridgeapi.PoolSession{{
			Source: "http://192.168.0.58:5004/auto/v5.1", Streams: []string{"king5"}, State: bridgeapi.SessionPlaying, Listeners: 2,
		}}}},
	}
}

func newClient(t *testing.T, url string) *bridgeclient.Client {
	t.Helper()
	c, err := bridgeclient.New(bridgeclient.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestObservations(t *testing.T) {
	var path, accept atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.Method + " " + r.URL.Path)
		accept.Store(r.Header.Get("Accept"))
		_ = json.NewEncoder(w).Encode(document())
	}))
	defer srv.Close()
	got, err := newClient(t, srv.URL+"/").Observations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if path.Load() != "GET "+bridgeapi.PathObservations || accept.Load() != "application/json" {
		t.Errorf("request %v (Accept %v), want GET %s", path.Load(), accept.Load(), bridgeapi.PathObservations)
	}
	want := document()
	if got.Bridge.StreamsVersion != want.Bridge.StreamsVersion || len(got.Streams) != 1 || got.Streams[0].Listeners != 2 ||
		got.Streams[0].Session.State != bridgeapi.SessionPlaying || got.Pools[0].Sessions[0].Streams[0] != "king5" || !got.GeneratedAt.Equal(at) {
		t.Errorf("decoded %+v", got)
	}
}

// TestObservationsNullLists: lists the bridge leaves null come back empty,
// and a field the controller does not know (a newer bridge) is ignored.
func TestObservationsNullLists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"generatedAt":"2026-10-16T12:00:00Z","bridge":{"startedAt":"2026-10-16T12:00:00Z","calls":0,"unknownCalls":0,"fullCalls":0,"newField":1},
			"streams":null,"pools":[{"name":"hdhomerun","max":1,"sessions":null}]}`))
	}))
	defer srv.Close()
	got, err := newClient(t, srv.URL).Observations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Streams == nil || got.Pools[0].Sessions == nil {
		t.Errorf("null lists: %+v", got)
	}
	nothing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer nothing.Close()
	if got, err := newClient(t, nothing.URL).Observations(context.Background()); err != nil || got.Streams == nil || got.Pools == nil {
		t.Errorf("an empty document: %+v, %v", got, err)
	}
}

func TestObservationsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("the bridge fell over\x1b[31m " + strings.Repeat("x", 500)))
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL).Observations(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500 Internal Server Error: the bridge fell over") {
		t.Fatalf("error %v, want the status and the body quoted", err)
	}
	if strings.ContainsRune(err.Error(), 0x1b) || len(err.Error()) > 320 {
		t.Errorf("the error %q holds a control character or is unbounded", err)
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer empty.Close()
	if _, err := newClient(t, empty.URL).Observations(context.Background()); err == nil ||
		!strings.HasSuffix(err.Error(), "503 Service Unavailable") {
		t.Errorf("error %v, want it to end at the status", err)
	}
}

func TestObservationsTimesOut(t *testing.T) {
	defer bridgeclient.SetTimeout(200 * time.Millisecond)()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	_, err := newClient(t, srv.URL).Observations(context.Background())
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a slow bridge: %v after %s, want a timeout", err, time.Since(start))
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("error %v, want it to say the deadline passed", err)
	}
	if bridgeclient.RequestTimeout != 5*time.Second {
		t.Errorf("RequestTimeout is %s, want 5s", bridgeclient.RequestTimeout)
	}
}

func TestObservationsNoRedirect(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		_ = json.NewEncoder(w).Encode(document())
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+bridgeapi.PathObservations, http.StatusFound)
	}))
	defer srv.Close()
	for _, hc := range []*http.Client{nil, {}} {
		c, err := bridgeclient.New(bridgeclient.Config{URL: srv.URL, HTTP: hc})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Observations(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
			t.Errorf("a redirect: %v, want an error quoting 302", err)
		}
	}
	if followed.Load() {
		t.Error("the redirect was followed")
	}
}

func TestObservationsTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"streams":[`))
		item := []byte(`{"uuid":"` + strings.Repeat("u", 1000) + `"},`)
		for written := 0; written <= bridgeclient.MaxObservationsSize; written += len(item) {
			if _, err := w.Write(item); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL).Observations(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a huge body: %v, want it cut with an error", err)
	}
	if bridgeclient.MaxObservationsSize != 4<<20 {
		t.Errorf("MaxObservationsSize is %d, want 4 MiB", bridgeclient.MaxObservationsSize)
	}
}

func TestObservationsNotJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not the bridge</html>"))
	}))
	defer srv.Close()
	if _, err := newClient(t, srv.URL).Observations(context.Background()); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("an HTML page: %v, want a decode error", err)
	}
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"streams": {"a": 1}}`))
	}))
	defer wrong.Close()
	if _, err := newClient(t, wrong.URL).Observations(context.Background()); err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Errorf("a wrong shape: %v, want a decode error", err)
	}
}

func TestObservationsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := newClient(t, url).Observations(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "bridgeclient: ") {
		t.Errorf("a closed port: %v, want an error", err)
	}
}

func TestNewValidates(t *testing.T) {
	good := install.StreamBridgeURL(install.NewNames("patchbay"), "telephony", bridgeapi.DefaultControlPort)
	if good != "http://patchbay-streams.telephony.svc:9093" {
		t.Fatalf("StreamBridgeURL = %s", good)
	}
	if _, err := bridgeclient.New(bridgeclient.Config{URL: good}); err != nil {
		t.Errorf("%s: %v", good, err)
	}
	for _, bad := range []string{"", "patchbay-streams:9093", "ftp://h:9093", "http://", "http://user@h:9093", "http://h:9093/?x=1", "http://h:9093/#f", "http://[::1"} {
		if _, err := bridgeclient.New(bridgeclient.Config{URL: bad}); err == nil {
			t.Errorf("New(%q) passed", bad)
		}
	}
}
