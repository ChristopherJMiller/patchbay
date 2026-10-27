package agentview_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

func TestSnapshotFor(t *testing.T) {
	polled := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	s := agentview.Snapshot{
		Document: agentapi.Observations{
			Agent:    agentapi.AgentStatus{AppliedVersion: "v1", AsteriskUp: true},
			Devices:  []agentapi.DeviceObservations{{Device: "phone-desk", Calls: 1}, {Device: "softphone-laptop"}},
			Firmware: agentapi.FirmwareStore{Files: []agentapi.FirmwareFileStatus{{Load: "L", Name: "f", Present: true}}},
		},
		PolledAt:       polled,
		AgentReachable: true,
		Polls:          3,
	}
	if !s.Started() || (agentview.Snapshot{}).Started() {
		t.Error("Started is Polls > 0")
	}
	obs := s.For("phone-desk", "v2")
	if !obs.AgentReachable || !obs.PolledAt.Equal(polled) || obs.Agent.AppliedVersion != "v1" ||
		obs.CompiledVersion != "v2" || len(obs.Firmware.Files) != 1 || obs.Device == nil || obs.Device.Calls != 1 {
		t.Errorf("For = %+v", obs)
	}
	if obs.ConfigLive() {
		t.Error("v1 applied, v2 compiled: not live")
	}
	if s.For("phone-kitchen", "v1").Device != nil {
		t.Error("an unseen device has observations")
	}
	// The entry is a copy.
	obs.Device.Calls = 7
	if s.Document.Devices[0].Calls != 1 {
		t.Error("For returned the document's own entry")
	}
	// Before the first poll the agent is unreachable to every driver.
	if got := (agentview.Snapshot{}).For("phone-desk", "v1").AgentProblem(polled, 30*time.Second); got == "" {
		t.Error("the zero snapshot has no agent problem")
	}
}

func TestHolder(t *testing.T) {
	var h agentview.Holder
	var o agentview.Observer = &h
	if o.Snapshot().Started() {
		t.Fatal("a new Holder has started")
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			h.Set(agentview.Snapshot{Polls: uint64(i + 1)})
			_ = o.Snapshot()
		})
	}
	wg.Wait()
	if !o.Snapshot().Started() {
		t.Error("Set did not publish")
	}
}

func TestActionFailure(t *testing.T) {
	for _, c := range []struct {
		resp agentapi.ActionResponse
		err  error
		want string
	}{
		{agentapi.ActionResponse{Sent: true}, nil, ""},
		{agentapi.ActionResponse{Sent: false, Error: "no contact"}, nil, "no contact"},
		{agentapi.ActionResponse{}, nil, "the agent did not send it"},
		{agentapi.ActionResponse{Sent: true}, errors.New("401"), "401"},
	} {
		if got := agentview.ActionFailure(c.resp, c.err); got != c.want {
			t.Errorf("ActionFailure(%+v, %v) = %q, want %q", c.resp, c.err, got, c.want)
		}
	}
}

func TestPushFailure(t *testing.T) {
	for _, c := range []struct {
		resp agentapi.PushResponse
		err  error
		want string
	}{
		{agentapi.PushResponse{Sent: true, Phone: "status 0"}, nil, ""},
		{agentapi.PushResponse{Error: "no address is known for phone-desk"}, nil, "no address is known for phone-desk"},
		{agentapi.PushResponse{Error: "the phone did not accept the push", Phone: "status 4"}, nil,
			"the phone did not accept the push (the phone answered status 4)"},
		{agentapi.PushResponse{}, nil, "the agent did not send it"},
		{agentapi.PushResponse{Sent: true}, errors.New("401"), "401"},
	} {
		if got := agentview.PushFailure(c.resp, c.err); got != c.want {
			t.Errorf("PushFailure(%+v, %v) = %q, want %q", c.resp, c.err, got, c.want)
		}
	}
}

func TestChangesNotify(t *testing.T) {
	ctx := context.Background()
	c := agentview.NewChanges(1)
	if err := c.Notify(ctx, drivers.KindPhone, "telephony", "desk"); err != nil {
		t.Fatal(err)
	}
	if e := <-c.Phones; e.Object.Namespace != "telephony" || e.Object.Name != "desk" {
		t.Errorf("phone event %+v", e.Object.ObjectMeta)
	}
	if err := c.Notify(ctx, drivers.KindSoftphone, "telephony", "laptop"); err != nil {
		t.Fatal(err)
	}
	if e := <-c.Softphones; e.Object.Name != "laptop" {
		t.Errorf("softphone event %+v", e.Object.ObjectMeta)
	}
	if err := c.Notify(ctx, "Person", "telephony", "chris"); err == nil {
		t.Error("a Person was notified")
	}
	// A full channel blocks until ctx ends.
	if err := c.Notify(ctx, drivers.KindPhone, "telephony", "desk"); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := c.Notify(short, drivers.KindPhone, "telephony", "desk"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a full channel: %v, want the context's error", err)
	}
}
