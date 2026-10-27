package devicehttp_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/devicehttp"
	"github.com/ChristopherJMiller/patchbay/internal/agent/push"
)

func TestServesMessagePage(t *testing.T) {
	root := t.TempDir()
	rec := newRecorder(t)
	var pages push.Pages
	pages.Set("phone-desk", "abc", []byte("<CiscoIPPhoneText><Text>Dinner!</Text></CiscoIPPhoneText>"))
	s, err := devicehttp.New(devicehttp.Config{
		LiveDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"), Grants: &grants{}, Recorder: rec,
		Pages: &pages, Log: logr.Discard(),
	})
	must(t, err)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	f := &authFixture{ts: ts, rec: rec}

	r := f.send(t, http.MethodGet, "/messages/phone-desk.xml?v=x", "", nil)
	if r.code != http.StatusOK || r.body != "<CiscoIPPhoneText><Text>Dinner!</Text></CiscoIPPhoneText>" {
		t.Fatalf("got %d %q", r.code, r.body)
	}
	if got := r.header.Get("Content-Type"); got != "text/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if h := f.send(t, http.MethodHead, "/messages/phone-desk.xml", "", nil); h.code != http.StatusOK || h.body != "" {
		t.Errorf("HEAD = %d %q", h.code, h.body)
	}

	want := f.send(t, http.MethodGet, "/nothing-here", "", nil)
	for _, p := range []string{"/messages/phone-other.xml", "/messages/phone-desk", "/messages/.xml", "/messages/a/phone-desk.xml", "/messages/"} {
		if got := f.send(t, http.MethodGet, p, "", nil); got.code != http.StatusNotFound || got.body != want.body {
			t.Errorf("%s: %d %q, want the uniform 404 %q", p, got.code, got.body, want.body)
		}
	}
	pages.Clear("phone-desk")
	if got := f.send(t, http.MethodGet, "/messages/phone-desk.xml", "", nil); got.code != http.StatusNotFound || got.body != want.body {
		t.Errorf("after Clear: %d %q", got.code, got.body)
	}
	if got := rec.all(); len(got) != 1 || got[0].File != "nothing-here" {
		t.Errorf("only the control request should be recorded, got %+v", got)
	}
}
