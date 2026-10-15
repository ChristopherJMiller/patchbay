package amitest_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/ami/amitest"
)

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := amitest.New()
	if err := f.Reload(ctx, "res_pjsip.so"); !errors.Is(err, ami.ErrNotConnected) {
		t.Errorf("Reload while down = %v", err)
	}
	if f.State().Up {
		t.Error("a new fake is up")
	}
	f.SetUp(true, "22.11.0")
	if st := f.State(); !st.Up || st.Version != "22.11.0" {
		t.Errorf("State = %+v", st)
	}
	f.SetAccount("u", "p")
	if u, p := f.Account(); u != "u" || p != "p" {
		t.Errorf("Account = %s %s", u, p)
	}
	f.SetEndpoints("b", "a")
	f.SetContexts("y", "x")
	f.SetContacts(ami.Contact{Endpoint: "a", CallID: "c"})
	f.SetChannels(ami.Channel{Name: "PJSIP/a-1", Endpoint: "a"})
	if eps, _ := f.Endpoints(ctx); !slices.Equal(eps, []string{"a", "b"}) {
		t.Errorf("Endpoints = %v", eps)
	}
	if cs, _ := f.DialplanContexts(ctx); !slices.Equal(cs, []string{"x", "y"}) {
		t.Errorf("DialplanContexts = %v", cs)
	}
	if cs, _ := f.Contacts(ctx); len(cs) != 1 || cs[0].CallID != "c" {
		t.Errorf("Contacts = %v", cs)
	}
	if chs, _ := f.Channels(ctx); len(chs) != 1 {
		t.Errorf("Channels = %v", chs)
	}
	boom := errors.New("boom")
	f.OnReload(func(m string) error { return boom })
	f.OnEndpoints(func() ([]string, error) { return []string{"z"}, nil })
	f.OnContexts(func() ([]string, error) { return nil, boom })
	f.OnContacts(func() ([]ami.Contact, error) { return nil, boom })
	f.OnChannels(func() ([]ami.Channel, error) { return nil, boom })
	f.OnNotify(func(ami.Notify) error { return boom })
	if err := f.Reload(ctx, "manager"); !errors.Is(err, boom) {
		t.Errorf("Reload = %v", err)
	}
	if eps, _ := f.Endpoints(ctx); !slices.Equal(eps, []string{"z"}) {
		t.Errorf("Endpoints = %v", eps)
	}
	if _, err := f.DialplanContexts(ctx); !errors.Is(err, boom) {
		t.Errorf("DialplanContexts = %v", err)
	}
	if _, err := f.Contacts(ctx); !errors.Is(err, boom) {
		t.Errorf("Contacts = %v", err)
	}
	if _, err := f.Channels(ctx); !errors.Is(err, boom) {
		t.Errorf("Channels = %v", err)
	}
	if err := f.Notify(ctx, ami.Notify{Endpoint: "a", Body: []string{"x"}}); !errors.Is(err, boom) {
		t.Errorf("Notify = %v", err)
	}
	if n := f.Notifies(); len(n) != 1 || n[0].Endpoint != "a" {
		t.Errorf("Notifies = %v", n)
	}
	want := []string{"Reload res_pjsip.so", "SetAccount u", "Endpoints", "DialplanContexts", "Contacts", "Channels",
		"Reload manager", "Endpoints", "DialplanContexts", "Contacts", "Channels", "Notify a"}
	if got := f.Calls(); !slices.Equal(got, want) {
		t.Errorf("Calls = %q, want %q", got, want)
	}
	f.ResetCalls()
	if len(f.Calls()) != 0 || len(f.Notifies()) != 0 {
		t.Error("ResetCalls kept calls")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Endpoints(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Endpoints with a done context = %v", err)
	}
	f.SetUp(false, "")
	if _, err := f.Endpoints(ctx); !errors.Is(err, ami.ErrNotConnected) {
		t.Errorf("Endpoints while down = %v", err)
	}
}
