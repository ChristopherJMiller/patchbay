package devices_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/devices"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// registeredDesk creates Phone desk (100), compiles it, and walks it to
// Registered at its compiled version at t0+10s. It returns what the agent
// reports of it. Events and actor calls so far are discarded.
func registeredDesk(t *testing.T, f *fixture) agentapi.DeviceObservations {
	t.Helper()
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	ring := agentapi.DeviceObservations{
		Device: deskID, MAC: deskMAC,
		Fetches:      []agentapi.Fetch{configFetch(f.version(deskID), t0.Add(5*time.Second))},
		Registration: contact("100", deskIP, t0.Add(10*time.Minute)),
	}
	f.at(10 * time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	if st := f.phone("desk").Status.State; st != string(drivers.StateRegistered) {
		t.Fatalf("desk is %s; want Registered", st)
	}
	f.rec.take()
	f.actor.take()
	return ring
}

// relabel changes desk's label to "Desk Two" and recompiles: its config version changes.
func relabel(t *testing.T, f *fixture) string {
	t.Helper()
	old := f.version(deskID)
	edit(f, f.phone("desk"), func(p *v1alpha1.Phone) { p.Spec.Label = "Desk Two" })
	f.compile()
	if f.version(deskID) == old {
		t.Fatal("the label change did not change desk's config version")
	}
	return f.version(deskID)
}

// TestPhoneUnseen: a Phone nothing has seen is Unseen, with every condition
// the driver lists, each carrying the generation, P2, and a slow requeue.
func TestPhoneUnseen(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	f.see()
	res := f.reconcilePhone("desk")

	p := f.phone("desk")
	if p.Status.State != string(drivers.StateUnseen) || p.Status.ObservedGeneration != p.Generation {
		t.Errorf("state %q at generation %d; want Unseen at %d", p.Status.State, p.Status.ObservedGeneration, p.Generation)
	}
	want := "connect it to a VLAN 20 switch port; it will request SEP" + deskMAC + ".cnf.xml from " + pbxAddr
	if p.Status.HumanAction != want {
		t.Errorf("human action %q; want %q", p.Status.HumanAction, want)
	}
	types := []string{
		v1alpha1.ConditionReady, v1alpha1.ConditionFirmwareAvailable, v1alpha1.ConditionFirmwareCurrent,
		v1alpha1.ConditionProvisioned, v1alpha1.ConditionRegistered, v1alpha1.ConditionConfigCurrent,
		v1alpha1.ConditionNumberConflict, v1alpha1.ConditionMACConflict, v1alpha1.ConditionUnsupported,
		v1alpha1.ConditionLabelShortened,
	}
	if len(p.Status.Conditions) != len(types) {
		t.Errorf("%d conditions; want %d: %v", len(p.Status.Conditions), len(types), p.Status.Conditions)
	}
	at := metav1.NewTime(t0)
	for _, typ := range types {
		c := condition(t, p.Status.Conditions, typ)
		if c.ObservedGeneration != p.Generation || !c.LastTransitionTime.Equal(&at) {
			t.Errorf("%s: generation %d at %v; want %d at %v", typ, c.ObservedGeneration, c.LastTransitionTime, p.Generation, at)
		}
	}
	wantCondition(t, p.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, cisco79x5.ReasonUnseen, want)
	if p.Status.Observed != nil || p.Status.LastAction != nil || p.Status.HandledRequest != "" {
		t.Errorf("memory %+v %+v %q; want none", p.Status.Observed, p.Status.LastAction, p.Status.HandledRequest)
	}
	if res.RequeueAfter != 10*time.Minute {
		t.Errorf("requeue after %v; want 10m", res.RequeueAfter)
	}
}

// TestStatusWrittenOnlyWhenChanged: deciding again on unchanged input
// writes nothing (W1 carry-over: no status write -> watch -> reconcile
// loop), for an Unseen and a Registered phone; a change writes once.
func TestStatusWrittenOnlyWhenChanged(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	f.see()
	f.reconcilePhone("desk")
	if n := f.statusWrites.Load(); n != 1 {
		t.Fatalf("%d status writes for the first decision; want 1", n)
	}
	rv := f.phone("desk").ResourceVersion

	f.at(time.Minute) // Unseen is not time-dependent
	f.see()
	f.reconcilePhone("desk")
	if n, now := f.statusWrites.Load(), f.phone("desk").ResourceVersion; n != 1 || now != rv {
		t.Errorf("deciding again: %d status writes, resourceVersion %s -> %s; want 1 and unchanged", n, rv, now)
	}

	ring := agentapi.DeviceObservations{Device: deskID, MAC: deskMAC,
		Fetches:      []agentapi.Fetch{configFetch(f.version(deskID), t0.Add(70*time.Second))},
		Registration: contact("100", deskIP, t0.Add(20*time.Minute))}
	f.at(80 * time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	if n := f.statusWrites.Load(); n != 2 {
		t.Fatalf("%d status writes after it registered; want 2", n)
	}
	rv = f.phone("desk").ResourceVersion
	f.at(90 * time.Second) // within the minute LastRegistered is kept
	f.see(ring)
	f.reconcilePhone("desk")
	if n, now := f.statusWrites.Load(), f.phone("desk").ResourceVersion; n != 2 || now != rv {
		t.Errorf("deciding a Registered phone again: %d status writes, resourceVersion %s -> %s; want 2 and unchanged", n, rv, now)
	}
}

// TestNothingBeforeFirstPoll: until the poller's first attempt ends, no
// status is written (a restarted controller must not flip every phone).
func TestNothingBeforeFirstPoll(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	if res := f.reconcilePhone("desk"); res.RequeueAfter != 0 {
		t.Errorf("requeue after %v; want none (the first snapshot enqueues it)", res.RequeueAfter)
	}
	if st := f.phone("desk").Status; st.State != "" || len(st.Conditions) != 0 || f.statusWrites.Load() != 0 {
		t.Errorf("status %+v; want none before the first poll", st)
	}
	if e := f.rec.take(); len(e) != 0 {
		t.Errorf("events %v; want none", e)
	}
}

// TestNothingBeforeCompiled: no status while pbx-config does not exist, or
// while the device is not in its index; once it is, the device is decided.
func TestNothingBeforeCompiled(t *testing.T) {
	f := newFixture(t)
	f.see()
	f.newPhone("early", "0200E2E000E1", "101", "")
	f.reconcilePhone("early")
	if st := f.phone("early").Status; st.State != "" || f.statusWrites.Load() != 0 {
		t.Errorf("no pbx-config: status %+v; want none", st)
	}

	f.compile()
	f.newPhone("desk", deskMAC, "100", "Desk") // after the bundle
	f.reconcilePhone("desk")
	if st := f.phone("desk").Status; st.State != "" || f.statusWrites.Load() != 0 {
		t.Errorf("not in the index: status %+v; want none", st)
	}

	f.compile()
	f.reconcilePhone("desk")
	if st := f.phone("desk").Status; st.State != string(drivers.StateUnseen) {
		t.Errorf("compiled: state %q; want Unseen", st.State)
	}
}

// TestPhoneNotFound: a Phone deleted before its reconcile is done.
func TestPhoneNotFound(t *testing.T) {
	f := newFixture(t)
	f.see()
	f.reconcilePhone("gone")
	f.reconcileSoftphone("gone")
	if f.statusWrites.Load() != 0 {
		t.Error("a status was written for a missing resource")
	}
}

// TestClaimsFromResolveClaims (W1 carry-over): the conflicts come from
// compile.ResolveClaims over every resource, so the reconcilers report
// exactly the holders Compile routes.
func TestClaimsFromResolveClaims(t *testing.T) {
	f := newFixture(t)
	f.newPhone("a-older", "0200E2E000A1", "100", "")
	f.newPhone("b-newer", "0200E2E000B1", "100", "")
	f.newPhone("c-mac", "0200E2E000A1", "101", "")
	f.newPhone("d-plan", "0200E2E000D1", "200", "")
	f.newSoftphone("e-soft", "100", "")
	b := f.compile()
	f.see()
	for _, n := range []string{"a-older", "b-newer", "c-mac", "d-plan"} {
		f.reconcilePhone(n)
	}
	f.reconcileSoftphone("e-soft")

	wantInvalid := func(conds []metav1.Condition, reason, message string) {
		t.Helper()
		wantCondition(t, conds, v1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
	}
	if st := f.phone("a-older").Status.State; st != string(drivers.StateUnseen) {
		t.Errorf("a-older is %s; want Unseen (it holds 100)", st)
	}
	b2 := f.phone("b-newer").Status
	wantInvalid(b2.Conditions, cisco79x5.ReasonNumberConflict, "number 100 is held by Phone a-older, which is older")
	wantCondition(t, b2.Conditions, v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, cisco79x5.ReasonNumberConflict,
		"number 100 is held by Phone a-older, which is older")
	c := f.phone("c-mac").Status
	wantInvalid(c.Conditions, cisco79x5.ReasonMACConflict, "MAC 0200E2E000A1 is Phone a-older's, which is older")
	wantCondition(t, c.Conditions, v1alpha1.ConditionMACConflict, metav1.ConditionTrue, cisco79x5.ReasonMACConflict, "")
	wantInvalid(f.phone("d-plan").Status.Conditions, v1alpha1.ReasonNumberNotInPlan, "number 200 is not in the Phone range 1XX")
	wantInvalid(f.softphone("e-soft").Status.Conditions, "NumberConflict", "number 100 is held by Phone a-older, which is older")

	// The same resources compiled route exactly the holders.
	for _, n := range []string{"a-older", "b-newer", "c-mac", "d-plan"} {
		routed := b.Devices[drivers.DeviceID(drivers.KindPhone, n)].Number != ""
		if invalid := f.phone(n).Status.State == string(drivers.StateInvalid); routed == invalid {
			t.Errorf("Phone %s: routed %v, Invalid %v", n, routed, invalid)
		}
	}
	if b.Devices[drivers.DeviceID(drivers.KindSoftphone, "e-soft")].Number != "" {
		t.Error("e-soft is routed")
	}
}

// TestResyncSentAndRecorded: a registered phone whose config changed is
// resynced once, and the resync is remembered in lastAction.
func TestResyncSentAndRecorded(t *testing.T) {
	f := newFixture(t)
	ring := registeredDesk(t, f)
	v2 := relabel(t, f)
	f.at(time.Minute)
	f.see(ring)
	res := f.reconcilePhone("desk")

	calls := f.actor.take()
	if len(calls) != 1 || calls[0] != (actorCall{"resync", agentapi.ActionRequest{Device: deskID, Version: v2}}) {
		t.Fatalf("actions %v; want one resync of %s to %s", calls, deskID, v2)
	}
	st := f.phone("desk").Status
	want := v1alpha1.LastAction{Kind: v1alpha1.ActionResync, Version: v2, At: metav1.NewTime(t0.Add(time.Minute)), Attempts: 1}
	if st.LastAction == nil || st.LastAction.Kind != want.Kind || st.LastAction.Version != want.Version ||
		!st.LastAction.At.Equal(&want.At) || st.LastAction.Attempts != want.Attempts {
		t.Errorf("lastAction %+v; want %+v", st.LastAction, want)
	}
	if st.State != string(drivers.StateConfigStale) || res.RequeueAfter != 5*time.Minute {
		t.Errorf("state %s, requeue %v; want ConfigStale, 5m", st.State, res.RequeueAfter)
	}
	wantEvents(t, f.rec.take(),
		deskEvent(corev1.EventTypeNormal, "ActionSent", "Resync", "resync sent for version "+v2[:12]),
		deskEvent(corev1.EventTypeNormal, "StateChanged", "Decide", "Registered -> ConfigStale: resync sent (attempt 1)"))

	// Deciding again within 5 min sends nothing more.
	f.at(time.Minute + 30*time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	if calls := f.actor.take(); len(calls) != 0 {
		t.Errorf("a second reconcile sent %v; want nothing", calls)
	}
}

// TestResyncFailureKeepsLastAction (6.2.1): an action that does not go out
// records nothing: lastAction and handledRequest stay, so a requested
// resync stays pending; ActionFailed is a Warning; the device is decided
// again in 30 s, and the request is answered once the agent sends it.
func TestResyncFailureKeepsLastAction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resp   agentapi.ActionResponse
		err    error
		reason string
	}{
		{"not sent", agentapi.ActionResponse{Sent: false, Error: "phone-desk has no contact"}, nil, "phone-desk has no contact"},
		{"agent unreachable", agentapi.ActionResponse{}, errors.New("agent: connection refused"), "agent: connection refused"},
		{"not sent, no reason", agentapi.ActionResponse{}, nil, "the agent did not send it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ring := registeredDesk(t, f)
			edit(f, f.phone("desk"), func(p *v1alpha1.Phone) {
				p.Annotations = map[string]string{annResync: "2026-10-15T12:00:30Z"}
			})
			v2 := relabel(t, f)
			f.actor.answer(tc.resp, tc.err)
			f.at(time.Minute)
			f.see(ring)
			res := f.reconcilePhone("desk")

			if calls := f.actor.take(); len(calls) != 1 || calls[0].Action != "resync" {
				t.Fatalf("actions %v; want one resync", calls)
			}
			st := f.phone("desk").Status
			if st.LastAction != nil || st.HandledRequest != "" {
				t.Errorf("lastAction %+v, handledRequest %q; want both unchanged (none)", st.LastAction, st.HandledRequest)
			}
			if st.State != string(drivers.StateConfigStale) {
				t.Errorf("state %s; want ConfigStale (the rest of the decision is written)", st.State)
			}
			if res.RequeueAfter != devices.ActionRetryAfter {
				t.Errorf("requeue after %v; want %v", res.RequeueAfter, devices.ActionRetryAfter)
			}
			wantEvents(t, f.rec.take(),
				deskEvent(corev1.EventTypeWarning, "ActionFailed", "Resync",
					"resync for version "+v2[:12]+" not sent: "+tc.reason),
				deskEvent(corev1.EventTypeNormal, "StateChanged", "Decide", "Registered -> ConfigStale: resync sent on request"))

			f.actor.answer(agentapi.ActionResponse{Sent: true}, nil)
			f.at(time.Minute + 30*time.Second)
			f.see(ring)
			f.reconcilePhone("desk")
			st = f.phone("desk").Status
			if st.HandledRequest != "2026-10-15T12:00:30Z" || st.LastAction == nil || st.LastAction.Version != v2 {
				t.Errorf("after the retry: handledRequest %q, lastAction %+v; want the request answered", st.HandledRequest, st.LastAction)
			}
			if calls := f.actor.take(); len(calls) != 1 {
				t.Errorf("the retry sent %v; want one resync", calls)
			}
		})
	}
}

// TestRequestIgnored: a request the device's state cannot answer is marked
// handled without an action, with a RequestIgnored Event, once.
func TestRequestIgnored(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk", func(p *v1alpha1.Phone) {
		p.Annotations = map[string]string{annResync: "first"}
	})
	f.compile()
	f.see()
	f.reconcilePhone("desk")
	if st := f.phone("desk").Status; st.HandledRequest != "first" {
		t.Errorf("handledRequest %q; want first", st.HandledRequest)
	}
	wantEvents(t, f.rec.take(),
		deskEvent(corev1.EventTypeNormal, "StateChanged", "Decide",
			"None -> Unseen: connect it to a VLAN 20 switch port; it will request SEP"+deskMAC+".cnf.xml from "+pbxAddr),
		deskEvent(corev1.EventTypeNormal, "RequestIgnored", "Decide",
			annResync+" request first ignored: the device is Unseen"))
	if calls := f.actor.take(); len(calls) != 0 {
		t.Errorf("actions %v; want none", calls)
	}
	f.reconcilePhone("desk")
	if e := f.rec.take(); len(e) != 0 {
		t.Errorf("deciding again: events %v; want none", e)
	}
}

// TestRequestedResyncNotIgnored: a Registered phone answers a request with
// a resync, so no RequestIgnored.
func TestRequestedResyncNotIgnored(t *testing.T) {
	f := newFixture(t)
	ring := registeredDesk(t, f)
	edit(f, f.phone("desk"), func(p *v1alpha1.Phone) { p.Annotations = map[string]string{annResync: "please"} })
	f.at(20 * time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	if calls := f.actor.take(); len(calls) != 1 || calls[0].Req.Version != f.version(deskID) {
		t.Errorf("actions %v; want one resync to the current version", calls)
	}
	wantEvents(t, f.rec.take(),
		deskEvent(corev1.EventTypeNormal, "ActionSent", "Resync", "resync sent for version "+f.version(deskID)[:12]))
	if st := f.phone("desk").Status; st.HandledRequest != "please" || st.State != string(drivers.StateRegistered) {
		t.Errorf("handledRequest %q, state %s; want please, Registered", st.HandledRequest, st.State)
	}
}

// TestApplyFailedControllerHalf (§9 rollback, the controller's half): the
// agent reports that it refused the compiled version: ConfigCurrent False
// ApplyFailed with the agent's message and one ApplyFailed Event, not
// repeated; an error about another version is not an ApplyFailed.
func TestApplyFailedControllerHalf(t *testing.T) {
	const agentError = "verify: endpoint phone-desk is missing (100% of 1)"
	t.Run("the compiled version", func(t *testing.T) {
		f := newFixture(t)
		ring := registeredDesk(t, f)
		v0 := f.bundle.Version
		relabel(t, f)
		broken := f.bundle.Version
		f.agent.AppliedVersion, f.agent.LastApplyVersion, f.agent.LastApplyError = v0, broken, agentError
		f.at(time.Minute)
		f.see(ring)
		f.reconcilePhone("desk")

		st := f.phone("desk").Status
		wantCondition(t, st.Conditions, v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse, v1alpha1.ReasonApplyFailed, agentError)
		wantCondition(t, st.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, cisco79x5.ReasonConfigStale,
			"the PBX agent refused the new config: "+agentError)
		if calls := f.actor.take(); len(calls) != 0 {
			t.Errorf("actions %v; want none: the new config is not live", calls)
		}
		wantEvents(t, f.rec.take(),
			deskEvent(corev1.EventTypeNormal, "StateChanged", "Decide",
				"Registered -> ConfigStale: the PBX agent refused the new config: "+agentError),
			deskEvent(corev1.EventTypeWarning, "ApplyFailed", "Decide",
				"config version "+broken[:12]+" was not applied: "+agentError))

		f.at(time.Minute + 5*time.Second)
		f.see(ring)
		f.reconcilePhone("desk")
		if e := f.rec.take(); len(e) != 0 {
			t.Errorf("deciding again: events %v; want none", e)
		}
	})
	t.Run("another version", func(t *testing.T) {
		f := newFixture(t)
		ring := registeredDesk(t, f)
		v0 := f.bundle.Version
		relabel(t, f)
		f.agent.AppliedVersion, f.agent.LastApplyVersion, f.agent.LastApplyError = v0, v0, agentError
		f.at(time.Minute)
		f.see(ring)
		f.reconcilePhone("desk")
		wantCondition(t, f.phone("desk").Status.Conditions, v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse,
			v1alpha1.ReasonApplyPending, "")
		for _, e := range f.rec.take() {
			if e.Reason == "ApplyFailed" {
				t.Errorf("event %s; want no ApplyFailed for an older version", e)
			}
		}
	})
}

// TestNoResyncDuringCall (Review Focus 5): a stale phone in a call is not
// resynced until the call ends, then exactly once.
func TestNoResyncDuringCall(t *testing.T) {
	f := newFixture(t)
	ring := registeredDesk(t, f)
	v2 := relabel(t, f)
	ring.Calls = 1
	f.at(time.Minute)
	f.see(ring)
	res := f.reconcilePhone("desk")
	if calls := f.actor.take(); len(calls) != 0 {
		t.Fatalf("during a call: actions %v; want none", calls)
	}
	st := f.phone("desk").Status
	c := condition(t, st.Conditions, v1alpha1.ConditionConfigCurrent)
	if st.State != string(drivers.StateConfigStale) || c.Status != metav1.ConditionFalse || c.Reason != cisco79x5.ReasonStale ||
		!strings.Contains(c.Message, "waiting for the call to end") {
		t.Errorf("during a call: %s, ConfigCurrent %s %s %q; want ConfigStale, False Stale waiting for the call", st.State, c.Status, c.Reason, c.Message)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("requeue after %v; want 30s", res.RequeueAfter)
	}

	ring.Calls = 0
	f.at(time.Minute + 30*time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	f.at(time.Minute + 35*time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	if calls := f.actor.take(); len(calls) != 1 || calls[0].Req != (agentapi.ActionRequest{Device: deskID, Version: v2}) {
		t.Errorf("after the call: actions %v; want exactly one resync", calls)
	}
}

// TestUnknownKeepsState: when the agent cannot be trusted the last state is
// kept for display, Ready and Registered say why, and nothing else
// changes.
func TestUnknownKeepsState(t *testing.T) {
	f := newFixture(t)
	ring := registeredDesk(t, f)
	before := f.phone("desk").Status

	f.unreachable()
	f.at(15 * time.Second)
	f.reconcilePhone("desk")
	st := f.phone("desk").Status
	if st.State != string(drivers.StateRegistered) {
		t.Errorf("state %s; want Registered kept", st.State)
	}
	wantCondition(t, st.Conditions, v1alpha1.ConditionReady, metav1.ConditionUnknown, v1alpha1.ReasonAgentUnreachable, "")
	wantCondition(t, st.Conditions, v1alpha1.ConditionRegistered, metav1.ConditionUnknown, v1alpha1.ReasonAgentUnreachable, "")
	for _, typ := range []string{v1alpha1.ConditionConfigCurrent, v1alpha1.ConditionProvisioned, v1alpha1.ConditionFirmwareCurrent} {
		if b, a := condition(t, before.Conditions, typ), condition(t, st.Conditions, typ); b != a {
			t.Errorf("%s changed: %+v -> %+v", typ, b, a)
		}
	}
	if !st.Observed.LastRegistered.Equal(before.Observed.LastRegistered) {
		t.Errorf("lastRegistered %v -> %v; want it kept", before.Observed.LastRegistered, st.Observed.LastRegistered)
	}

	// Asterisk down: Ready False PBXDown.
	f.agent.AsteriskUp = false
	f.at(20 * time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	wantCondition(t, f.phone("desk").Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonPBXDown, "")
}

// TestStateChangedEvent: each state change is one Event, old -> new with
// Ready's message; the first from None.
func TestStateChangedEvent(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	f.see()
	f.reconcilePhone("desk")
	ring := agentapi.DeviceObservations{Device: deskID, MAC: deskMAC,
		Fetches: []agentapi.Fetch{configFetch(f.version(deskID), t0.Add(5*time.Second))}}
	f.at(10 * time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	wantEvents(t, f.rec.take(),
		deskEvent(corev1.EventTypeNormal, "StateChanged", "Decide",
			"None -> Unseen: connect it to a VLAN 20 switch port; it will request SEP"+deskMAC+".cnf.xml from "+pbxAddr),
		deskEvent(corev1.EventTypeNormal, "StateChanged", "Decide", "Unseen -> Registering: waiting for it to register"))
}
