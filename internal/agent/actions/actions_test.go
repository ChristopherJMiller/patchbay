package actions_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/actions"
	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/ami/amitest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

var (
	desk   = drivers.DeviceID(drivers.KindPhone, "desk")
	laptop = drivers.DeviceID(drivers.KindSoftphone, "chris-laptop")
)

const callID = "a84b4c76e66710@192.168.20.5"

func setup(t *testing.T) (*actions.Actions, *amitest.Fake) {
	t.Helper()
	idx := intaketest.House().Compile(t).Index()
	f := amitest.New()
	f.SetUp(true, "22.11.0")
	a := actions.New(actions.Config{
		AMI:     f,
		Applied: func() (string, compile.Index, bool) { return idx.Version, idx, true },
		Log:     logr.Discard(),
	})
	return a, f
}

func notifyFor(action, callID string) ami.Notify {
	return ami.Notify{
		Endpoint: desk,
		Headers: []ami.Header{
			{Name: "Event", Value: "service-control"},
			{Name: "Subscription-State", Value: "active"},
			{Name: "Content-Type", Value: "text/plain"},
		},
		Body: []string{
			"action=" + action,
			"RegisterCallId={" + callID + "}",
			"ConfigVersionStamp={00000000-0000-0000-0000-000000000000}",
			"DialplanVersionStamp={00000000-0000-0000-0000-000000000000}",
			"SoftkeyVersionStamp={00000000-0000-0000-0000-000000000000}",
			"FeatureControlVersionStamp={00000000-0000-0000-0000-000000000000}",
			"HeadsetVersionStamp={0-0000000000}",
		},
	}
}

func equalNotify(a, b ami.Notify) bool {
	return a.Endpoint == b.Endpoint && slices.Equal(a.Headers, b.Headers) && slices.Equal(a.Body, b.Body)
}

func TestNotifyBodies(t *testing.T) {
	a, f := setup(t)
	f.SetContacts(
		ami.Contact{Endpoint: laptop, AOR: "150", CallID: "other@x"},
		ami.Contact{Endpoint: desk, AOR: "100", URI: "sip:100@192.168.20.5:5060", CallID: callID, ExpiresAt: time.Now().Add(time.Minute)},
	)
	ctx := context.Background()
	if r := a.Resync(ctx, api.ActionRequest{Device: desk, Version: "v2"}); !r.Sent || r.Error != "" {
		t.Fatalf("Resync = %+v", r)
	}
	if r := a.Restart(ctx, api.ActionRequest{Device: desk}); !r.Sent || r.Error != "" {
		t.Fatalf("Restart = %+v", r)
	}
	got := f.Notifies()
	want := []ami.Notify{notifyFor("restart", callID), notifyFor("reset", callID)}
	if len(got) != 2 || !equalNotify(got[0], want[0]) || !equalNotify(got[1], want[1]) {
		t.Errorf("NOTIFYs:\n got %+v\nwant %+v", got, want)
	}
}

// TestContactByAOR: a contact without an endpoint is the device's whose
// number is its AOR; one naming another endpoint never is.
func TestContactByAOR(t *testing.T) {
	a, f := setup(t)
	f.SetContacts(
		ami.Contact{Endpoint: "phone-old", AOR: "100", CallID: "not-this-one"},
		ami.Contact{AOR: "100", CallID: callID},
	)
	if r := a.Resync(context.Background(), api.ActionRequest{Device: desk}); !r.Sent {
		t.Fatalf("Resync = %+v", r)
	}
	if n := f.Notifies(); len(n) != 1 || n[0].Body[1] != "RegisterCallId={"+callID+"}" {
		t.Errorf("NOTIFY = %+v", n)
	}
	// The latest of two contacts.
	f.ResetCalls()
	f.SetContacts(
		ami.Contact{Endpoint: desk, CallID: "older", ExpiresAt: time.Unix(100, 0)},
		ami.Contact{Endpoint: desk, CallID: "newer", ExpiresAt: time.Unix(200, 0)},
	)
	a.Resync(context.Background(), api.ActionRequest{Device: desk})
	if n := f.Notifies(); len(n) != 1 || n[0].Body[1] != "RegisterCallId={newer}" {
		t.Errorf("NOTIFY = %+v", n)
	}
}

func TestNotSent(t *testing.T) {
	cases := map[string]struct {
		device   string
		contacts []ami.Contact
		down     bool
		notify   error
		contErr  error
		want     string
	}{
		"unknown device":   {device: "phone-nope", want: "unknown device phone-nope"},
		"no contact":       {device: desk, contacts: []ami.Contact{{Endpoint: laptop, CallID: "x"}}, want: "no live contact"},
		"other endpoint":   {device: desk, contacts: []ami.Contact{{Endpoint: "phone-old", AOR: "100", CallID: "x"}}, want: "no live contact"},
		"no Call-ID":       {device: desk, contacts: []ami.Contact{{Endpoint: desk}}, want: "no Call-ID"},
		"brace":            {device: desk, contacts: []ami.Contact{{Endpoint: desk, CallID: "a}b"}}, want: "unsafe Call-ID"},
		"open brace":       {device: desk, contacts: []ami.Contact{{Endpoint: desk, CallID: "{ab"}}, want: "unsafe Call-ID"},
		"comma":            {device: desk, contacts: []ami.Contact{{Endpoint: desk, CallID: "a,b"}}, want: "unsafe Call-ID"},
		"quote":            {device: desk, contacts: []ami.Contact{{Endpoint: desk, CallID: `a"b`}}, want: "unsafe Call-ID"},
		"backslash":        {device: desk, contacts: []ami.Contact{{Endpoint: desk, CallID: `a\b`}}, want: "unsafe Call-ID"},
		"line break":       {device: desk, contacts: []ami.Contact{{Endpoint: desk, CallID: "a\r\nAction: Command"}}, want: "unsafe Call-ID"},
		"space":            {device: desk, contacts: []ami.Contact{{Endpoint: desk, CallID: "a b"}}, want: "unsafe Call-ID"},
		"AMI down":         {device: desk, down: true, want: ami.ErrNotConnected.Error()},
		"contacts fail":    {device: desk, contErr: errors.New("ami: PJSIPShowContacts: boom"), want: "ami: PJSIPShowContacts: boom"},
		"Asterisk refuses": {device: desk, contacts: []ami.Contact{{Endpoint: desk, CallID: callID}}, notify: errors.New("ami: PJSIPNotify: Unable to retrieve endpoint"), want: "ami: PJSIPNotify: Unable to retrieve endpoint"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, f := setup(t)
			f.SetContacts(tc.contacts...)
			if tc.down {
				f.SetUp(false, "")
			}
			if tc.notify != nil {
				f.OnNotify(func(ami.Notify) error { return tc.notify })
			}
			if tc.contErr != nil {
				f.OnContacts(func() ([]ami.Contact, error) { return nil, tc.contErr })
			}
			for _, act := range []func(context.Context, api.ActionRequest) api.ActionResponse{a.Resync, a.Restart} {
				r := act(context.Background(), api.ActionRequest{Device: tc.device})
				if r.Sent || r.Error != tc.want {
					t.Errorf("= %+v, want not sent: %q", r, tc.want)
				}
			}
			if tc.notify == nil && len(f.Notifies()) != 0 {
				t.Errorf("a NOTIFY was sent: %+v", f.Notifies())
			}
		})
	}
}

func TestBeforeAnyApply(t *testing.T) {
	f := amitest.New()
	f.SetUp(true, "")
	for _, applied := range []func() (string, compile.Index, bool){
		nil, func() (string, compile.Index, bool) { return "", compile.Index{}, false },
	} {
		a := actions.New(actions.Config{AMI: f, Applied: applied})
		if r := a.Resync(context.Background(), api.ActionRequest{Device: desk}); r.Sent || r.Error != "unknown device "+desk {
			t.Errorf("= %+v", r)
		}
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("AMI calls %q", calls)
	}
}

// TestTimeout: an action is bounded by Config.Timeout.
func TestTimeout(t *testing.T) {
	idx := intaketest.House().Compile(t).Index()
	f := amitest.New()
	f.SetUp(true, "")
	a := actions.New(actions.Config{
		AMI: blockingAMI{f}, Applied: func() (string, compile.Index, bool) { return idx.Version, idx, true },
		Timeout: 50 * time.Millisecond,
	})
	start := time.Now()
	r := a.Resync(context.Background(), api.ActionRequest{Device: desk})
	if r.Sent || !strings.Contains(r.Error, "deadline") || time.Since(start) > 2*time.Second {
		t.Errorf("= %+v after %s", r, time.Since(start))
	}
}

// blockingAMI's Contacts waits for its context.
type blockingAMI struct{ *amitest.Fake }

func (b blockingAMI) Contacts(ctx context.Context) ([]ami.Contact, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
