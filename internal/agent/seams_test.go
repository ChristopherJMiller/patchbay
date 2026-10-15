package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// fakeActions records what the server hands it.
type fakeActions struct {
	mu   sync.Mutex
	got  []string
	resp api.ActionResponse
}

func (f *fakeActions) Resync(_ context.Context, req api.ActionRequest) api.ActionResponse {
	return f.record("resync", req)
}

func (f *fakeActions) Restart(_ context.Context, req api.ActionRequest) api.ActionResponse {
	return f.record("restart", req)
}

func (f *fakeActions) record(kind string, req api.ActionRequest) api.ActionResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, kind+" "+req.Device+" "+req.Version)
	return f.resp
}

func newSeamServer(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	opts.Tokens = StaticToken(token)
	opts.Now = func() time.Time { return pinned }
	opts.Log = logr.Discard()
	h, err := NewHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func doBody(t *testing.T, ts *httptest.Server, method, path, bearer, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, strings.NewReader(body))
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
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestActionsAreDecodedAndDelegated(t *testing.T) {
	fa := &fakeActions{resp: api.ActionResponse{Sent: false, Error: "no contact"}}
	ts := newSeamServer(t, Options{Actions: fa})

	code, body := doBody(t, ts, http.MethodPost, api.PathActionResync, token, `{"device":"phone-desk","version":"v2"}`)
	var resp api.ActionResponse
	if err := json.Unmarshal(body, &resp); err != nil || code != http.StatusOK || resp != fa.resp {
		t.Fatalf("resync = %d %s (%v), want 200 with the action's answer", code, body, err)
	}
	if code, body := doBody(t, ts, http.MethodPost, api.PathActionRestart, token, `{"device":"phone-desk"}`); code != http.StatusOK {
		t.Fatalf("restart = %d %s", code, body)
	}
	for name, bad := range map[string]string{
		"not JSON":      `device=phone-desk`,
		"unknown field": `{"device":"phone-desk","force":true}`,
		"no device":     `{"version":"v2"}`,
		"too large":     `{"device":"` + strings.Repeat("a", maxActionBody) + `"}`,
	} {
		if code, body := doBody(t, ts, http.MethodPost, api.PathActionResync, token, bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, body)
		}
	}
	if code, _ := doBody(t, ts, http.MethodPost, api.PathActionResync, "", `{"device":"phone-desk"}`); code != http.StatusUnauthorized {
		t.Errorf("resync without a token: %d, want 401", code)
	}
	want := []string{"resync phone-desk v2", "restart phone-desk "}
	if strings.Join(fa.got, "|") != strings.Join(want, "|") {
		t.Errorf("actions saw %q, want %q", fa.got, want)
	}
}

func TestObservationsComeFromTheStore(t *testing.T) {
	var asked time.Time
	ts := newSeamServer(t, Options{Observations: func(now time.Time) api.Observations {
		asked = now
		return api.Observations{
			GeneratedAt: time.Unix(1, 0),
			Agent:       api.AgentStatus{Version: "wrong", AppliedVersion: "v1", AsteriskUp: true},
			Devices:     []api.DeviceObservations{{Device: "phone-desk", Calls: 1}},
		}
	}})
	code, body := doBody(t, ts, http.MethodGet, api.PathObservations, token, "")
	if code != http.StatusOK {
		t.Fatalf("observations = %d %s", code, body)
	}
	if !asked.Equal(pinned) {
		t.Errorf("the store was asked at %v, want %v", asked, pinned)
	}
	var doc api.Observations
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.GeneratedAt.Equal(pinned) || !doc.Agent.StartedAt.Equal(pinned) || doc.Agent.Version != version.Version {
		t.Errorf("the server must own generatedAt, startedAt and version: %s", body)
	}
	if doc.Agent.AppliedVersion != "v1" || !doc.Agent.AsteriskUp || len(doc.Devices) != 1 || doc.Devices[0].Calls != 1 {
		t.Errorf("the store's content was lost: %s", body)
	}
	for _, list := range []string{`"unknownDevices":[]`, `"files":[]`} {
		if !strings.Contains(string(body), list) {
			t.Errorf("want %s in %s", list, body)
		}
	}
}

func TestFirmwareRoute(t *testing.T) {
	ts := newSeamServer(t, Options{})
	code, body := doBody(t, ts, http.MethodPut, api.FirmwarePath("SIP45.9-4-2SR4-3S", "a.sbn"), token, "x")
	var up api.FirmwareUploadResponse
	if err := json.Unmarshal(body, &up); err != nil || code != http.StatusNotImplemented || up.Load != "SIP45.9-4-2SR4-3S" || up.Name != "a.sbn" {
		t.Fatalf("upload without a store = %d %s, want 501 with a FirmwareUploadResponse", code, body)
	}

	var seen []string
	ts = newSeamServer(t, Options{Firmware: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.PathValue("load")+"/"+r.PathValue("name")+"="+string(b))
		w.WriteHeader(http.StatusCreated)
	})})
	if code, _ := doBody(t, ts, http.MethodPut, api.FirmwarePath("L", "f.sbn"), token, "bytes"); code != http.StatusCreated {
		t.Fatalf("upload = %d, want the handler's 201", code)
	}
	if code, _ := doBody(t, ts, http.MethodPut, api.FirmwarePath("L", "f.sbn"), "", "bytes"); code != http.StatusUnauthorized {
		t.Errorf("upload without a token = %d, want 401", code)
	}
	if code, _ := doBody(t, ts, http.MethodGet, api.FirmwarePath("L", "f.sbn"), token, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET of an upload path = %d, want 405", code)
	}
	if code, _ := doBody(t, ts, http.MethodPut, api.PathFirmware+"/L/sub/f.sbn", token, "x"); code != http.StatusNotFound {
		t.Errorf("a nested upload path = %d, want 404", code)
	}
	if strings.Join(seen, "|") != "L/f.sbn=bytes" {
		t.Errorf("the handler saw %q", seen)
	}
}

func TestMetricsRoute(t *testing.T) {
	ts := newSeamServer(t, Options{})
	if code, _ := doBody(t, ts, http.MethodGet, api.PathMetrics, "", ""); code != http.StatusNotFound {
		t.Errorf("metrics without a handler = %d, want 404", code)
	}
	ts = newSeamServer(t, Options{Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "patchbay_agent_up 1\n")
	})})
	if code, body := doBody(t, ts, http.MethodGet, api.PathMetrics, "", ""); code != http.StatusOK || !strings.Contains(string(body), "patchbay_agent_up") {
		t.Errorf("metrics = %d %s, want 200 without a token", code, body)
	}
}
