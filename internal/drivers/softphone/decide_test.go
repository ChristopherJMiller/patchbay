package softphone_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

const (
	spName = "chris-laptop"
	spID   = "softphone-chris-laptop"
	vOld   = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	vNew   = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	bundle = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	stale  = 30 * time.Second
)

var settings = drivers.DecideSettings{Signalling: drivers.SignallingPJSIP, StaleAfter: stale, PBXAddress: "192.168.0.240"}

func at(d time.Duration) *metav1.Time {
	t := metav1.NewTime(t0.Add(d))
	return &t
}

func rotated(d time.Duration) *v1alpha1.LastAction {
	return &v1alpha1.LastAction{Kind: v1alpha1.ActionRotateToken, At: *at(d), Attempts: 1}
}

func fetched(version string, d time.Duration) *v1alpha1.ConfigFetch {
	return &v1alpha1.ConfigFetch{Version: version, At: *at(d)}
}

// device is the house softphone, compiled at vNew.
func device(mutate ...func(*drivers.Device)) drivers.Device {
	d := drivers.Device{
		Kind: drivers.KindSoftphone, Name: spName, ID: spID, Model: drivers.ModelSoftphone,
		Number: "150", Label: "Chris (laptop)", ConfigVersion: vNew,
	}
	for _, m := range mutate {
		m(&d)
	}
	return d
}

func withMemory(m drivers.Memory) func(*drivers.Device) {
	return func(d *drivers.Device) { d.Memory = m }
}

// observations is a healthy agent serving the latest bundle, seeing d.
func observations(d *agentapi.DeviceObservations) drivers.Observations {
	return drivers.Observations{
		AgentReachable:  true,
		PolledAt:        t0,
		Agent:           agentapi.AgentStatus{AppliedVersion: bundle, LastApplyVersion: bundle, AsteriskUp: true},
		Device:          d,
		CompiledVersion: bundle,
	}
}

func seen(reg *agentapi.Registration, fetches ...agentapi.Fetch) *agentapi.DeviceObservations {
	return &agentapi.DeviceObservations{Device: spID, Fetches: fetches, Registration: reg}
}

// fetch is a served fetch of the provisioning document.
func fetch(version string, d time.Duration) agentapi.Fetch {
	return agentapi.Fetch{File: softphone.ProvisioningFile(spID), Transport: agentapi.TransportHTTPDevice,
		Result: agentapi.FetchServed, At: t0.Add(d), Source: "192.168.0.50", Version: version}
}

// live is a contact expiring 10 minutes after t0.
func live() *agentapi.Registration {
	return &agentapi.Registration{Contact: "sip:150@192.168.0.51:5060", Address: "192.168.0.51",
		UserAgent: "Linphone-Desktop/6.0", CallID: "c1", ExpiresAt: t0.Add(10 * time.Minute)}
}

// ---- expected conditions ----

func cond(typ string, s metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return drivers.Condition(typ, s, reason, msg)
}

func ready(s metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return cond(v1alpha1.ConditionReady, s, reason, msg)
}

const (
	readyNew         = "waiting for Linphone to fetch its settings from its one-time URL"
	readyChanged     = "its settings changed since Linphone fetched them"
	readyReissued    = "a new provisioning token was requested; waiting for Linphone to fetch its settings"
	readySpent       = "Linphone fetched its settings, which spends its one-time URL; minting a new one"
	readyProvisioned = "Linphone fetched its settings; waiting for it to register"
	readyRegistered  = "registered from 192.168.0.51"
	notEvaluated     = "not evaluated while the softphone is invalid"
	agentGone        = "the PBX agent cannot be reached, or its observations are stale"
)

var (
	pNotFetched = cond(v1alpha1.ConditionProvisioned, metav1.ConditionFalse, softphone.ReasonNotFetched, "Linphone has not fetched its settings")
	pOld        = cond(v1alpha1.ConditionProvisioned, metav1.ConditionFalse, softphone.ReasonOldVersion,
		"fetched version a1a1a1a1a1a1; the current version is b2b2b2b2b2b2")
	rNone      = cond(v1alpha1.ConditionRegistered, metav1.ConditionFalse, softphone.ReasonNoContact, "no live registration")
	rContact   = cond(v1alpha1.ConditionRegistered, metav1.ConditionTrue, softphone.ReasonContact, "sip:150@192.168.0.51:5060, Linphone-Desktop/6.0")
	ccCurrent  = cond(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, softphone.ReasonCurrent, "version b2b2b2b2b2b2")
	ccNotFetch = cond(v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse, softphone.ReasonNotFetched, "Linphone has not fetched its settings")
	ccStale    = cond(v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse, softphone.ReasonStale,
		"Linphone has version a1a1a1a1a1a1; the current version is b2b2b2b2b2b2")
	ccPending = cond(v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse, v1alpha1.ReasonApplyPending, "the PBX does not serve the latest config yet")
	ncNo      = cond(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, softphone.ReasonNoConflict, "")
	invalidPR = []metav1.Condition{
		cond(v1alpha1.ConditionProvisioned, metav1.ConditionUnknown, softphone.ReasonInvalid, notEvaluated),
		cond(v1alpha1.ConditionRegistered, metav1.ConditionUnknown, softphone.ReasonInvalid, notEvaluated),
		cond(v1alpha1.ConditionConfigCurrent, metav1.ConditionUnknown, softphone.ReasonInvalid, notEvaluated),
	}
	rotate = []drivers.Action{{Kind: v1alpha1.ActionRotateToken}}
)

func pFetched(d time.Duration) metav1.Condition {
	return cond(v1alpha1.ConditionProvisioned, metav1.ConditionTrue, softphone.ReasonFetched,
		"fetched version b2b2b2b2b2b2 at "+t0.Add(d).Format(time.RFC3339))
}

func invalidConds(readyC, conflictC metav1.Condition) []metav1.Condition {
	return append(append([]metav1.Condition{readyC}, invalidPR...), conflictC)
}

func scan() string { return "scan `patchbay softphone qr chris-laptop` in Linphone" }
func rescan() string {
	return "its settings changed: scan `patchbay softphone qr chris-laptop` in Linphone again"
}

func offlineMsg(d time.Duration) string {
	return "last registered " + t0.Add(d).Format(time.RFC3339) + "; an app asleep or away from home is normal"
}

// ---- shared memories ----

// provisionedMem: fetched vNew at fetchAt (from .50), token rotated then.
func provisionedMem(fetchAt time.Duration) drivers.Memory {
	return drivers.Memory{FirstSeen: at(-2 * time.Hour), Address: "192.168.0.50",
		LastConfigFetch: fetched(vNew, fetchAt), LastAction: rotated(fetchAt)}
}

// registeredMem: provisioned an hour ago, registered 30 s ago from .51.
func registeredMem() drivers.Memory {
	m := provisionedMem(-time.Hour)
	m.Address, m.LastRegistered = "192.168.0.51", at(-30*time.Second)
	return m
}

type decideCase struct {
	name string
	dev  drivers.Device
	obs  drivers.Observations
	now  time.Duration // since t0
	want drivers.Decision
}

func TestDecide(t *testing.T) {
	rows := []struct {
		row   string
		cases []decideCase
	}{
		{"S1-Invalid", invalidCases()},
		{"S2-Unknown", unknownCases()},
		{"S3-Reissue", reissueCases()},
		{"S4-TokenSpent", spentCases()},
		{"S5-Issued", issuedCases()},
		{"S6-Provisioned", provisionedCases()},
		{"S7-Registered", registeredCases()},
		{"S8-Offline", offlineCases()},
	}
	for _, r := range rows {
		t.Run(r.row, func(t *testing.T) {
			for _, tc := range r.cases {
				t.Run(tc.name, func(t *testing.T) {
					before := memString(tc.dev.Memory)
					got := softphone.New().Decide(tc.dev, tc.obs, settings, t0.Add(tc.now))
					assertDecision(t, got, tc.want)
					if memString(tc.dev.Memory) != before {
						t.Error("Decide changed its input's memory")
					}
				})
			}
		})
	}
}

func invalidCases() []decideCase {
	held := "number 150 is held by Softphone old-laptop, which is older"
	modelMsg := `model "cisco-7945" is not served by the softphone driver`
	return []decideCase{
		{
			name: "unknown model",
			dev:  device(func(d *drivers.Device) { d.Model = "cisco-7945" }),
			obs:  observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions:  invalidConds(ready(metav1.ConditionFalse, softphone.ReasonUnknownModel, modelMsg), ncNo),
				HumanAction: modelMsg},
		},
		{
			name: "number conflict, with the claim message",
			dev:  device(func(d *drivers.Device) { d.NumberConflict, d.ClaimMessage = true, held }),
			obs:  observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions: invalidConds(ready(metav1.ConditionFalse, softphone.ReasonNumberConflict, held),
					cond(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, softphone.ReasonNumberConflict, held)),
				HumanAction: held},
		},
		{
			name: "number conflict without a claim message",
			dev:  device(func(d *drivers.Device) { d.NumberConflict = true }),
			obs:  observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions: invalidConds(ready(metav1.ConditionFalse, softphone.ReasonNumberConflict, "number 150 is held by an older resource"),
					cond(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, softphone.ReasonNumberConflict, "number 150 is held by an older resource")),
				HumanAction: "number 150 is held by an older resource"},
		},
		{
			name: "number out of plan",
			dev: device(func(d *drivers.Device) {
				d.Number, d.NumberOutOfPlan, d.ClaimMessage = "1500", true, "number 1500 is not in the device range 1XX"
			}),
			obs: observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions:  invalidConds(ready(metav1.ConditionFalse, v1alpha1.ReasonNumberNotInPlan, "number 1500 is not in the device range 1XX"), ncNo),
				HumanAction: "number 1500 is not in the device range 1XX"},
		},
		{
			name: "number out of plan without a claim message",
			dev:  device(func(d *drivers.Device) { d.Number, d.NumberOutOfPlan = "1500", true }),
			obs:  observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions: invalidConds(ready(metav1.ConditionFalse, v1alpha1.ReasonNumberNotInPlan,
					"number 1500 is not in the number plan's device range"), ncNo),
				HumanAction: "number 1500 is not in the number plan's device range"},
		},
		{
			name: "MAC conflict (never set for a softphone, still Invalid)",
			dev:  device(func(d *drivers.Device) { d.MACConflict = true }),
			obs:  observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions:  invalidConds(ready(metav1.ConditionFalse, v1alpha1.ConditionMACConflict, "its MAC is held by an older resource"), ncNo),
				HumanAction: "its MAC is held by an older resource"},
		},
		{
			name: "two reasons: the unknown model is Ready's, the conflict still shows",
			dev: device(func(d *drivers.Device) {
				d.Model, d.NumberConflict, d.ClaimMessage = "cisco-7945", true, held
			}),
			obs: observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions: invalidConds(ready(metav1.ConditionFalse, softphone.ReasonUnknownModel, modelMsg),
					cond(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, softphone.ReasonNumberConflict, held)),
				HumanAction: modelMsg},
		},
		{
			name: "two reasons: the conflict before out of plan",
			dev: device(func(d *drivers.Device) {
				d.NumberConflict, d.NumberOutOfPlan, d.ClaimMessage = true, true, held
			}),
			obs: observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions: invalidConds(ready(metav1.ConditionFalse, softphone.ReasonNumberConflict, held),
					cond(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, softphone.ReasonNumberConflict, held)),
				HumanAction: held},
		},
		{
			name: "a pending reissue is answered with no action",
			dev: device(func(d *drivers.Device) {
				d.NumberConflict, d.ClaimMessage, d.Request = true, held, "2026-10-14T11:59:00Z"
				d.Memory.HandledRequest = "2026-10-01T00:00:00Z"
			}),
			obs: observations(nil),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions: invalidConds(ready(metav1.ConditionFalse, softphone.ReasonNumberConflict, held),
					cond(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, softphone.ReasonNumberConflict, held)),
				StatusPatch: drivers.Memory{HandledRequest: "2026-10-14T11:59:00Z"},
				HumanAction: held},
		},
		{
			name: "Invalid outranks an unreachable agent, and memory is folded",
			dev:  device(func(d *drivers.Device) { d.NumberConflict, d.ClaimMessage = true, held }),
			obs: func() drivers.Observations {
				o := observations(seen(nil, fetch(vNew, -time.Minute)))
				o.AgentReachable = false
				return o
			}(),
			want: drivers.Decision{State: drivers.StateInvalid,
				Conditions: invalidConds(ready(metav1.ConditionFalse, softphone.ReasonNumberConflict, held),
					cond(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, softphone.ReasonNumberConflict, held)),
				StatusPatch: drivers.Memory{FirstSeen: at(-time.Minute), Address: "192.168.0.50", LastConfigFetch: fetched(vNew, -time.Minute)},
				HumanAction: held},
		},
	}
}

func unknownCases() []decideCase {
	mem := registeredMem()
	pending := func(d *drivers.Device) {
		d.LastState, d.Request = drivers.StateRegistered, "2026-10-14T12:00:00Z"
		d.Memory = mem
	}
	gone := []metav1.Condition{
		ready(metav1.ConditionUnknown, v1alpha1.ReasonAgentUnreachable, agentGone),
		cond(v1alpha1.ConditionRegistered, metav1.ConditionUnknown, v1alpha1.ReasonAgentUnreachable, agentGone),
	}
	withObs := func(f func(*drivers.Observations)) drivers.Observations {
		o := observations(seen(live(), fetch(vNew, -time.Hour), fetch(vNew, -time.Second)))
		f(&o)
		return o
	}
	return []decideCase{
		{
			name: "polled exactly StaleAfter ago is not stale (the reissue is acted on)",
			dev:  device(pending),
			obs:  withObs(func(o *drivers.Observations) { o.PolledAt = t0.Add(-stale) }),
			want: func() drivers.Decision {
				m := mem
				m.HandledRequest, m.LastAction, m.LastConfigFetch = "2026-10-14T12:00:00Z", rotated(0), nil
				m.LastRegistered = at(0) // the fetch at -1s is after the registration at -30s
				return drivers.Decision{State: drivers.StateIssued,
					Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyReissued),
						pNotFetched, rContact, ccNotFetch, ncNo},
					Actions: rotate, StatusPatch: m, HumanAction: scan()}
			}(),
		},
		{
			name: "polled StaleAfter+1ns ago: last state kept, memory and request untouched",
			dev:  device(pending),
			obs:  withObs(func(o *drivers.Observations) { o.PolledAt = t0.Add(-stale - time.Nanosecond) }),
			want: drivers.Decision{State: drivers.StateRegistered, Conditions: gone, StatusPatch: mem},
		},
		{
			name: "the latest poll failed",
			dev:  device(pending),
			obs:  withObs(func(o *drivers.Observations) { o.AgentReachable = false }),
			want: drivers.Decision{State: drivers.StateRegistered, Conditions: gone, StatusPatch: mem},
		},
		{
			name: "Asterisk down: Registered False, PBXDown",
			dev:  device(pending),
			obs:  withObs(func(o *drivers.Observations) { o.Agent.AsteriskUp = false }),
			want: drivers.Decision{State: drivers.StateRegistered, StatusPatch: mem, Conditions: []metav1.Condition{
				ready(metav1.ConditionFalse, v1alpha1.ReasonPBXDown, "Asterisk is not running"),
				cond(v1alpha1.ConditionRegistered, metav1.ConditionFalse, v1alpha1.ReasonPBXDown, "Asterisk is not running"),
			}},
		},
		{
			name: "no last state: Unknown",
			dev:  device(),
			obs:  withObs(func(o *drivers.Observations) { o.AgentReachable = false }),
			want: drivers.Decision{State: drivers.StateUnknown, Conditions: gone},
		},
	}
}

func reissueCases() []decideCase {
	const req = "2026-10-14T12:00:00Z"
	afterReissue := func() drivers.Memory {
		m := registeredMem()
		m.HandledRequest, m.LastAction, m.LastConfigFetch = req, rotated(0), nil
		return m
	}
	return []decideCase{
		{
			name: "while registered: back to Issued, Registered stays True",
			dev: device(withMemory(registeredMem()), func(d *drivers.Device) {
				d.Request = req
				d.Memory.HandledRequest = "2026-10-01T00:00:00Z"
			}),
			obs: observations(seen(live(), fetch(vNew, -time.Hour))),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyReissued),
					pNotFetched, rContact, ccNotFetch, ncNo},
				Actions: rotate, StatusPatch: afterReissue(), HumanAction: scan()},
		},
		{
			name: "the next Decide, with the pre-reissue fetch still observed, stays Issued and does not rotate again",
			dev:  device(withMemory(afterReissue()), func(d *drivers.Device) { d.Request = req }),
			obs:  observations(seen(live(), fetch(vNew, -time.Hour))),
			now:  5 * time.Second,
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyNew),
					pNotFetched, rContact, ccNotFetch, ncNo},
				StatusPatch: afterReissue(), HumanAction: scan()},
		},
		{
			name: "a fetch after the reissue spends the new token",
			dev:  device(withMemory(afterReissue()), func(d *drivers.Device) { d.Request = req }),
			obs: func() drivers.Observations {
				o := observations(seen(live(), fetch(vNew, -time.Hour), fetch(vNew, 30*time.Second)))
				o.PolledAt = t0.Add(35 * time.Second)
				return o
			}(),
			now: 35 * time.Second,
			want: func() drivers.Decision {
				m := afterReissue()
				m.Address = "192.168.0.51"
				m.LastConfigFetch, m.LastAction, m.LastRegistered = fetched(vNew, 30*time.Second), rotated(35*time.Second), at(35*time.Second)
				return drivers.Decision{State: drivers.StateProvisioned,
					Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readySpent),
						pFetched(30 * time.Second), rContact, ccCurrent, ncNo},
					Actions: rotate, StatusPatch: m}
			}(),
		},
		{
			name: "never provisioned",
			dev:  device(func(d *drivers.Device) { d.Request = req }),
			obs:  observations(nil),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyReissued),
					pNotFetched, rNone, ccNotFetch, ncNo},
				Actions: rotate, StatusPatch: drivers.Memory{HandledRequest: req, LastAction: rotated(0)}, HumanAction: scan()},
		},
		{
			name: "between seconds the rotation is recorded at the next whole second",
			dev:  device(func(d *drivers.Device) { d.Request = req }),
			obs:  observations(nil),
			now:  400 * time.Millisecond,
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyReissued),
					pNotFetched, rNone, ccNotFetch, ncNo},
				Actions: rotate, StatusPatch: drivers.Memory{HandledRequest: req, LastAction: rotated(time.Second)}, HumanAction: scan()},
		},
	}
}

func spentCases() []decideCase {
	return []decideCase{
		{
			name: "no rotation yet: the first fetch spends the token",
			dev:  device(),
			obs:  observations(seen(nil, fetch(vNew, -10*time.Second))),
			want: drivers.Decision{State: drivers.StateProvisioned,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readySpent),
					pFetched(-10 * time.Second), rNone, ccCurrent, ncNo},
				Actions: rotate,
				StatusPatch: drivers.Memory{FirstSeen: at(-10 * time.Second), Address: "192.168.0.50",
					LastConfigFetch: fetched(vNew, -10*time.Second), LastAction: rotated(0)},
				RequeueAfter: 5*time.Minute - 10*time.Second},
		},
		{
			name: "a fetch at the last rotation's time is not news (S6)",
			dev:  device(withMemory(provisionedMem(-time.Minute))),
			obs:  observations(seen(nil, fetch(vNew, -time.Minute))),
			want: drivers.Decision{State: drivers.StateProvisioned,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readyProvisioned),
					pFetched(-time.Minute), rNone, ccCurrent, ncNo},
				StatusPatch: provisionedMem(-time.Minute), RequeueAfter: 4 * time.Minute},
		},
		{
			name: "a fetch 1ns after the last rotation spends the token",
			dev:  device(withMemory(provisionedMem(-time.Minute))),
			obs:  observations(seen(nil, fetch(vNew, -time.Minute+time.Nanosecond))),
			want: func() drivers.Decision {
				m := provisionedMem(-time.Minute)
				m.LastAction = rotated(0)
				return drivers.Decision{State: drivers.StateProvisioned,
					Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readySpent),
						pFetched(-time.Minute), rNone, ccCurrent, ncNo},
					Actions: rotate, StatusPatch: m, RequeueAfter: 4 * time.Minute}
			}(),
		},
		{
			name: "while registered",
			dev:  device(),
			obs:  observations(seen(live(), fetch(vNew, -10*time.Second))),
			want: drivers.Decision{State: drivers.StateProvisioned,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readySpent),
					pFetched(-10 * time.Second), rContact, ccCurrent, ncNo},
				Actions: rotate,
				StatusPatch: drivers.Memory{FirstSeen: at(-10 * time.Second), Address: "192.168.0.51",
					LastConfigFetch: fetched(vNew, -10*time.Second), LastAction: rotated(0), LastRegistered: at(0)}},
		},
		{
			name: "a fetch of an older version spends the token too",
			dev:  device(),
			obs:  observations(seen(nil, fetch(vOld, -10*time.Second))),
			want: drivers.Decision{State: drivers.StateProvisioned,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readySpent),
					pOld, rNone, ccStale, ncNo},
				Actions: rotate,
				StatusPatch: drivers.Memory{FirstSeen: at(-10 * time.Second), Address: "192.168.0.50",
					LastConfigFetch: fetched(vOld, -10*time.Second), LastAction: rotated(0)}},
		},
		{
			name: "a fetch that was not served spends nothing (S5)",
			dev:  device(),
			obs: observations(seen(nil, agentapi.Fetch{File: softphone.ProvisioningFile(spID), Result: agentapi.FetchError,
				At: t0.Add(-10 * time.Second), Source: "192.168.0.50", Error: "connection reset"})),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyNew),
					pNotFetched, rNone, ccNotFetch, ncNo},
				StatusPatch: drivers.Memory{FirstSeen: at(-10 * time.Second), Address: "192.168.0.50"},
				HumanAction: scan()},
		},
		{
			name: "a served fetch of another file spends nothing (S5)",
			dev:  device(),
			obs: observations(seen(nil, agentapi.Fetch{File: "http/directory.xml", Result: agentapi.FetchServed,
				At: t0.Add(-10 * time.Second), Source: "192.168.0.50"})),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyNew),
					pNotFetched, rNone, ccNotFetch, ncNo},
				StatusPatch: drivers.Memory{FirstSeen: at(-10 * time.Second), Address: "192.168.0.50"},
				HumanAction: scan()},
		},
	}
}

func issuedCases() []decideCase {
	staleMem := func() drivers.Memory {
		m := registeredMem()
		m.LastConfigFetch.Version = vOld
		return m
	}
	return []decideCase{
		{
			name: "never seen",
			dev:  device(),
			obs:  observations(nil),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyNew),
					pNotFetched, rNone, ccNotFetch, ncNo},
				HumanAction: scan()},
		},
		{
			name: "registered on an older version (its password changed): stale, scan again",
			dev:  device(withMemory(staleMem())),
			obs:  observations(seen(live(), fetch(vOld, -time.Hour))),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyChanged),
					pOld, rContact, ccStale, ncNo},
				StatusPatch: staleMem(), HumanAction: rescan()},
		},
		{
			name: "the bundle is not live yet: still asked to scan",
			dev:  device(),
			obs: func() drivers.Observations {
				o := observations(nil)
				o.CompiledVersion = "d4"
				return o
			}(),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyNew),
					pNotFetched, rNone, ccPending, ncNo},
				HumanAction: scan()},
		},
		{
			name: "the bundle was refused: nobody is asked to scan",
			dev:  device(withMemory(staleMem())),
			obs: func() drivers.Observations {
				o := observations(seen(live(), fetch(vOld, -time.Hour)))
				o.CompiledVersion, o.Agent.LastApplyVersion, o.Agent.LastApplyError = "d4", "d4", "pjsip: endpoint softphone-chris-laptop missing"
				return o
			}(),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyChanged),
					pOld, rContact,
					cond(v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse, v1alpha1.ReasonApplyFailed,
						"pjsip: endpoint softphone-chris-laptop missing"), ncNo},
				StatusPatch: staleMem()},
		},
		{
			name: "an older bundle's error does not count",
			dev:  device(),
			obs: func() drivers.Observations {
				o := observations(nil)
				o.CompiledVersion, o.Agent.LastApplyError = "d4", "old trouble"
				return o
			}(),
			want: drivers.Decision{State: drivers.StateIssued,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonIssued, readyNew),
					pNotFetched, rNone, ccPending, ncNo},
				HumanAction: scan()},
		},
	}
}

func provisionedCases() []decideCase {
	waiting := func(fetchAt time.Duration, human string, requeue time.Duration) decideCase {
		return decideCase{
			dev: device(withMemory(provisionedMem(fetchAt))),
			obs: observations(seen(nil, fetch(vNew, fetchAt))),
			want: drivers.Decision{State: drivers.StateProvisioned,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readyProvisioned),
					pFetched(fetchAt), rNone, ccCurrent, ncNo},
				StatusPatch: provisionedMem(fetchAt), HumanAction: human, RequeueAfter: requeue},
		}
	}
	named := func(name string, c decideCase) decideCase { c.name = name; return c }
	notRegistering := func(n int) string {
		return fmt.Sprintf("Linphone fetched its settings %d min ago but has not registered: is it on the LAN?", n)
	}
	equal := provisionedMem(-time.Hour)
	equal.LastRegistered = at(-time.Hour)
	return []decideCase{
		named("fetched 5m-1s ago: wait 1 s", waiting(-5*time.Minute+time.Second, "", time.Second)),
		named("fetched 5m ago: ask", waiting(-5*time.Minute, notRegistering(5), 0)),
		named("fetched 5m+1s ago: ask", waiting(-5*time.Minute-time.Second, notRegistering(5), 0)),
		named("fetched an hour ago", waiting(-time.Hour, notRegistering(60), 0)),
		{
			name: "registered at the same second as the fetch is not after it (S6, not S8)",
			dev:  device(withMemory(equal)),
			obs:  observations(seen(nil, fetch(vNew, -time.Hour))),
			want: drivers.Decision{State: drivers.StateProvisioned,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readyProvisioned),
					pFetched(-time.Hour), rNone, ccCurrent, ncNo},
				StatusPatch: equal, HumanAction: notRegistering(60)},
		},
		{
			name: "the agent restarted and lost its fetch log: memory carries it",
			dev:  device(withMemory(provisionedMem(-time.Minute))),
			obs:  observations(nil),
			want: drivers.Decision{State: drivers.StateProvisioned,
				Conditions: []metav1.Condition{ready(metav1.ConditionFalse, softphone.ReasonProvisioned, readyProvisioned),
					pFetched(-time.Minute), rNone, ccCurrent, ncNo},
				StatusPatch: provisionedMem(-time.Minute), RequeueAfter: 4 * time.Minute},
		},
	}
}

func registeredCases() []decideCase {
	return []decideCase{
		{
			name: "registered",
			dev:  device(withMemory(registeredMem())),
			obs:  observations(seen(live(), fetch(vNew, -time.Hour))),
			want: drivers.Decision{State: drivers.StateRegistered,
				Conditions: []metav1.Condition{ready(metav1.ConditionTrue, softphone.ReasonRegistered, readyRegistered),
					pFetched(-time.Hour), rContact, ccCurrent, ncNo},
				StatusPatch: registeredMem(), RequeueAfter: 10 * time.Minute},
		},
		{
			name: "first registration after provisioning",
			dev:  device(withMemory(provisionedMem(-time.Minute))),
			obs:  observations(seen(live(), fetch(vNew, -time.Minute))),
			want: func() drivers.Decision {
				m := provisionedMem(-time.Minute)
				m.Address, m.LastRegistered = "192.168.0.51", at(0)
				return drivers.Decision{State: drivers.StateRegistered,
					Conditions: []metav1.Condition{ready(metav1.ConditionTrue, softphone.ReasonRegistered, readyRegistered),
						pFetched(-time.Minute), rContact, ccCurrent, ncNo},
					StatusPatch: m, RequeueAfter: 10 * time.Minute}
			}(),
		},
		{
			name: "a newer bundle is not live yet: still Ready",
			dev:  device(withMemory(registeredMem())),
			obs: func() drivers.Observations {
				o := observations(seen(live(), fetch(vNew, -time.Hour)))
				o.CompiledVersion = "d4"
				return o
			}(),
			want: drivers.Decision{State: drivers.StateRegistered,
				Conditions: []metav1.Condition{ready(metav1.ConditionTrue, softphone.ReasonRegistered, readyRegistered),
					pFetched(-time.Hour), rContact, ccPending, ncNo},
				StatusPatch: registeredMem(), RequeueAfter: 10 * time.Minute},
		},
		{
			name: "no User-Agent",
			dev:  device(withMemory(registeredMem())),
			obs: func() drivers.Observations {
				r := live()
				r.UserAgent = ""
				return observations(seen(r, fetch(vNew, -time.Hour)))
			}(),
			want: drivers.Decision{State: drivers.StateRegistered,
				Conditions: []metav1.Condition{ready(metav1.ConditionTrue, softphone.ReasonRegistered, readyRegistered),
					pFetched(-time.Hour),
					cond(v1alpha1.ConditionRegistered, metav1.ConditionTrue, softphone.ReasonContact, "sip:150@192.168.0.51:5060"),
					ccCurrent, ncNo},
				StatusPatch: registeredMem(), RequeueAfter: 10 * time.Minute},
		},
	}
}

func offlineCases() []decideCase {
	oneSecondAfter := provisionedMem(-time.Hour)
	oneSecondAfter.Address, oneSecondAfter.LastRegistered = "192.168.0.51", at(-time.Hour+time.Second)
	off := func(m drivers.Memory, lastRegistered time.Duration) drivers.Decision {
		return drivers.Decision{State: drivers.StateOffline,
			Conditions: []metav1.Condition{ready(metav1.ConditionTrue, v1alpha1.ReasonOffline, offlineMsg(lastRegistered)),
				pFetched(-time.Hour),
				cond(v1alpha1.ConditionRegistered, metav1.ConditionFalse, v1alpha1.ReasonOffline, offlineMsg(lastRegistered)),
				ccCurrent, ncNo},
			StatusPatch: m}
	}
	return []decideCase{
		{
			name: "registered 1 s after the fetch, no contact now",
			dev:  device(withMemory(oneSecondAfter)),
			obs:  observations(seen(nil, fetch(vNew, -time.Hour))),
			want: off(oneSecondAfter, -time.Hour+time.Second),
		},
		{
			name: "the contact expires exactly now",
			dev:  device(withMemory(registeredMem())),
			obs: func() drivers.Observations {
				r := live()
				r.ExpiresAt = t0
				return observations(seen(r, fetch(vNew, -time.Hour)))
			}(),
			want: off(registeredMem(), -30*time.Second),
		},
		{
			name: "no observations at all (the agent restarted)",
			dev:  device(withMemory(registeredMem())),
			obs:  observations(nil),
			want: off(registeredMem(), -30*time.Second),
		},
	}
}

// ---- comparing decisions ----

func assertDecision(t *testing.T, got, want drivers.Decision) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	if got.State != want.State {
		t.Errorf("State = %s, want %s", got.State, want.State)
	}
	if !reflect.DeepEqual(got.Conditions, want.Conditions) {
		t.Errorf("Conditions:\n got %s\nwant %s", condString(got.Conditions), condString(want.Conditions))
	}
	if !reflect.DeepEqual(got.Actions, want.Actions) {
		t.Errorf("Actions = %+v, want %+v", got.Actions, want.Actions)
	}
	if !reflect.DeepEqual(got.StatusPatch, want.StatusPatch) {
		t.Errorf("StatusPatch:\n got %s\nwant %s", memString(got.StatusPatch), memString(want.StatusPatch))
	}
	if got.HumanAction != want.HumanAction {
		t.Errorf("HumanAction = %q, want %q", got.HumanAction, want.HumanAction)
	}
	if got.RequeueAfter != want.RequeueAfter {
		t.Errorf("RequeueAfter = %s, want %s", got.RequeueAfter, want.RequeueAfter)
	}
}

func condString(cs []metav1.Condition) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "\n    %s=%s %s %q", c.Type, c.Status, c.Reason, c.Message)
	}
	return b.String()
}

// memString prints a memory with every time to the nanosecond (metav1.Time's
// JSON keeps only seconds, which would hide a rounding bug).
func memString(m drivers.Memory) string {
	ts := func(t *metav1.Time) string {
		if t == nil {
			return "nil"
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	s := fmt.Sprintf("FirstSeen=%s Address=%q LastRegistered=%s UpgradeStartedAt=%s Firmware=%q HandledRequest=%q",
		ts(m.FirstSeen), m.Address, ts(m.LastRegistered), ts(m.UpgradeStartedAt), m.Firmware, m.HandledRequest)
	if c := m.LastConfigFetch; c != nil {
		s += fmt.Sprintf(" LastConfigFetch={%s %s}", c.Version, ts(&c.At))
	}
	if a := m.LastAction; a != nil {
		s += fmt.Sprintf(" LastAction={%s %q %s %d}", a.Kind, a.Version, ts(&a.At), a.Attempts)
	}
	return s
}

// TestRenderDecideAgree: Render refuses with ErrInvalidSpec exactly when
// Decide says Invalid (conflicts aside, which Compile resolves before
// Render).
func TestRenderDecideAgree(t *testing.T) {
	d := softphone.New()
	devs := []drivers.Device{device(), device(func(x *drivers.Device) { x.Model = "cisco-7945" }),
		device(func(x *drivers.Device) { x.Model = "" })}
	for _, tc := range renderCases() {
		devs = append(devs, tc.dev)
	}
	for _, dev := range devs {
		_, err := d.Render(dev, houseCtx(dev.ID))
		got := d.Decide(dev, observations(nil), settings, t0)
		if errors.Is(err, drivers.ErrInvalidSpec) != (got.State == drivers.StateInvalid) {
			t.Errorf("%s (model %q): Render error %v, Decide state %s", dev.Name, dev.Model, err, got.State)
		}
	}
}
