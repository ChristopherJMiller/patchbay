package push_test

import (
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/push"
)

func TestPages(t *testing.T) {
	var p push.Pages
	if _, _, ok := p.Get("phone-desk"); ok {
		t.Fatal("an empty store holds a page")
	}
	in := []byte("<a/>")
	p.Set("phone-desk", "v1", in)
	in[1] = 'z' // the store kept its own copy
	v, got, ok := p.Get("phone-desk")
	if !ok || v != "v1" || string(got) != "<a/>" {
		t.Fatalf("Get = %q %q %v", v, got, ok)
	}
	got[1] = 'q' // and hands out copies
	if _, again, _ := p.Get("phone-desk"); string(again) != "<a/>" {
		t.Errorf("the stored page changed through a Get: %q", again)
	}
	p.Set("phone-desk", "v2", []byte("<b/>"))
	if v, got, _ := p.Get("phone-desk"); v != "v2" || string(got) != "<b/>" {
		t.Errorf("Set did not replace: %q %q", v, got)
	}
	p.Clear("phone-desk")
	p.Clear("never-set")
	if _, _, ok := p.Get("phone-desk"); ok {
		t.Error("Clear left the page")
	}
}

func TestPagesAcknowledge(t *testing.T) {
	var p push.Pages
	if p.Acknowledged("phone-desk") != "" {
		t.Fatal("an empty store has an acknowledgement")
	}
	p.Set("phone-desk", "v1", []byte("<a/>"))
	if p.Acknowledged("phone-desk") != "" {
		t.Error("Set acknowledged the page")
	}
	p.Acknowledge("phone-desk", "v1")
	p.Set("phone-desk", "v2", []byte("<b/>"))
	if got := p.Acknowledged("phone-desk"); got != "v1" {
		t.Errorf("Set changed the acknowledgement to %q", got)
	}
	p.Clear("phone-desk")
	if got := p.Acknowledged("phone-desk"); got != "" {
		t.Errorf("Clear left the acknowledgement %q", got)
	}
}

// TestPagesUnacknowledge: a restarted phone's acknowledgement goes and its
// stored page stays (the phone may still fetch it).
func TestPagesUnacknowledge(t *testing.T) {
	var p push.Pages
	p.Unacknowledge("never-set")
	p.Set("phone-desk", "v1", []byte("<a/>"))
	p.Acknowledge("phone-desk", "v1")
	p.Unacknowledge("phone-desk")
	if got := p.Acknowledged("phone-desk"); got != "" {
		t.Errorf("Unacknowledge left the acknowledgement %q", got)
	}
	if v, body, ok := p.Get("phone-desk"); !ok || v != "v1" || string(body) != "<a/>" {
		t.Errorf("Unacknowledge dropped the stored page: %q %q %v", v, body, ok)
	}
	p.Acknowledge("phone-desk", "v1")
	if got := p.Acknowledged("phone-desk"); got != "v1" {
		t.Errorf("acknowledged again: %q, want v1", got)
	}
}
