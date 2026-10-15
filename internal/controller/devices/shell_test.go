package devices_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/devices"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

// scripted is the cisco79x5 driver (so Compile renders as usual) with a
// Decide that returns a scripted Decision: the shell's handling of actions
// no shipped driver emits yet (restart, several actions, an unknown kind).
type scripted struct {
	drivers.Driver
	mu sync.Mutex
	d  drivers.Decision
}

func (s *scripted) set(d drivers.Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = d
}

func (s *scripted) Decide(dev drivers.Device, _ drivers.Observations, _ drivers.DecideSettings, _ time.Time) drivers.Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.d
	d.StatusPatch = dev.Memory
	if len(d.Actions) > 0 {
		d.StatusPatch.LastAction = &v1alpha1.LastAction{Kind: d.Actions[0].Kind, Version: d.Actions[0].Version,
			At: metav1.NewTime(t0), Attempts: 1}
	}
	return d
}

func withScripted(t *testing.T) (func(*fixture), *scripted) {
	s := &scripted{Driver: cisco79x5.New()}
	return func(f *fixture) {
		reg, err := drivers.NewRegistry(s, softphone.New())
		if err != nil {
			t.Fatal(err)
		}
		f.reg = reg
	}, s
}

func scriptedDecision(requeue time.Duration, actions ...drivers.Action) drivers.Decision {
	return drivers.Decision{
		State:        drivers.StateRegistered,
		Conditions:   []metav1.Condition{drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionTrue, "Scripted", "")},
		Actions:      actions,
		RequeueAfter: requeue,
	}
}

// TestRestartSent: a restart goes to the agent's restart action with the
// device and version, and is remembered.
func TestRestartSent(t *testing.T) {
	opt, drv := withScripted(t)
	f := newFixture(t, opt)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	v := f.version(deskID)
	drv.set(scriptedDecision(0, drivers.Action{Kind: v1alpha1.ActionRestart, Version: v}))
	f.see()
	res := f.reconcilePhone("desk")
	if calls := f.actor.take(); len(calls) != 1 || calls[0] != (actorCall{"restart", agentapi.ActionRequest{Device: deskID, Version: v}}) {
		t.Errorf("actions %v; want one restart", calls)
	}
	if la := f.phone("desk").Status.LastAction; la == nil || la.Kind != v1alpha1.ActionRestart {
		t.Errorf("lastAction %+v; want the restart", la)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("requeue after %v; want none", res.RequeueAfter)
	}
	events := f.rec.take()
	if len(events) == 0 || events[0] != deskEvent(corev1.EventTypeNormal, "ActionSent", "Restart", "restart sent") {
		t.Errorf("events %v; want ActionSent restart first", events)
	}
}

// TestFailedActionStops: after a failed action the Decision's later
// actions are not attempted, and a Decision's requeue sooner than
// ActionRetryAfter wins.
func TestFailedActionStops(t *testing.T) {
	opt, drv := withScripted(t)
	f := newFixture(t, opt)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	v := f.version(deskID)
	drv.set(scriptedDecision(10*time.Second,
		drivers.Action{Kind: v1alpha1.ActionResync, Version: v}, drivers.Action{Kind: v1alpha1.ActionRestart, Version: v}))
	f.actor.answer(agentapi.ActionResponse{}, errors.New("agent: timeout"))
	f.see()
	res := f.reconcilePhone("desk")
	if calls := f.actor.take(); len(calls) != 1 || calls[0].Action != "resync" {
		t.Errorf("actions %v; want the resync only", calls)
	}
	if res.RequeueAfter != 10*time.Second {
		t.Errorf("requeue after %v; want the Decision's 10s", res.RequeueAfter)
	}
	if la := f.phone("desk").Status.LastAction; la != nil {
		t.Errorf("lastAction %+v; want none", la)
	}
}

// TestUnknownActionFails: an action kind the controller cannot take is a
// failed action, never a crash.
func TestUnknownActionFails(t *testing.T) {
	opt, drv := withScripted(t)
	f := newFixture(t, opt)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	drv.set(scriptedDecision(0, drivers.Action{Kind: "dance"}))
	f.see()
	f.reconcilePhone("desk")
	events := f.rec.take()
	want := deskEvent(corev1.EventTypeWarning, "ActionFailed", "Decide", `dance not sent: the controller cannot take action "dance"`)
	if len(events) == 0 || events[0] != want {
		t.Errorf("events %v; want %v first", events, want)
	}
}

// TestNoDriver: a model no driver serves is logged and not decided.
func TestNoDriver(t *testing.T) {
	f := newFixture(t, func(f *fixture) {
		reg, err := drivers.NewRegistry(softphone.New())
		if err != nil {
			t.Fatal(err)
		}
		f.reg = reg
	})
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	f.see()
	f.reconcilePhone("desk")
	if st := f.phone("desk").Status; st.State != "" || f.statusWrites.Load() != 0 {
		t.Errorf("status %+v; want none", st)
	}
	if !strings.Contains(f.logs.text(), "no driver serves the device's model") {
		t.Errorf("logs:\n%s\nwant the missing driver", f.logs.text())
	}
}

// TestConfigValidated: each reconciler refuses an incomplete Config, and
// Setup refuses one without Changes channels, before touching the manager.
func TestConfigValidated(t *testing.T) {
	_, err := devices.NewPhoneReconciler(devices.Config{})
	for _, want := range []string{"no client", "no namespace", "fullname", "settings", "no driver registry", "no observer",
		"no actor", "no event recorder"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v; want it to say %q", err, want)
		}
	}
	if _, err := devices.NewSoftphoneReconciler(devices.Config{}); err == nil {
		t.Error("NewSoftphoneReconciler accepted an empty Config")
	}
	if err := devices.Setup(nil, devices.Config{}); err == nil || !strings.Contains(err.Error(), "Changes") {
		t.Errorf("Setup without Changes: %v", err)
	}
	if err := devices.Setup(nil, devices.Config{Changes: agentview.NewChanges(1)}); err == nil || !strings.Contains(err.Error(), "no client") {
		t.Errorf("Setup with an empty Config: %v", err)
	}
}

// TestReadErrors: a pbx-config whose index does not decode, or a list that
// fails, is an error (requeued with backoff), and nothing is written.
func TestReadErrors(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	f.see()

	f.failList.Store(true)
	if _, err := f.phones.Reconcile(f.ctx, reqFor(f, "desk")); err == nil || !strings.Contains(err.Error(), "listing") {
		t.Errorf("a failed list: error %v", err)
	}
	f.failList.Store(false)

	var cm corev1.ConfigMap
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: f.names.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[compile.IndexKey] = "{not json"
	if err := env.Client.Update(f.ctx, &cm); err != nil {
		t.Fatal(err)
	}
	if _, err := f.phones.Reconcile(f.ctx, reqFor(f, "desk")); err == nil {
		t.Error("an undecodable index: no error")
	}
	if _, err := f.softphones.Reconcile(f.ctx, reqFor(f, "desk")); err != nil {
		t.Errorf("a missing Softphone: %v", err)
	}
	if f.statusWrites.Load() != 0 {
		t.Error("a status was written")
	}
}

func reqFor(f *fixture, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: name}}
}
