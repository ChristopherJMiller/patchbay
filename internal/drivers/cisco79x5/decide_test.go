package cisco79x5_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// settings are the house's DecideSettings.
var settings = drivers.DecideSettings{Signalling: drivers.SignallingPJSIP, StaleAfter: 30 * time.Second, PBXAddress: pbx}

const bundle = "bundle-7"

// healthy is an agent polled just now, with Asterisk up and the latest
// bundle live.
func healthy() drivers.Observations {
	return drivers.Observations{
		AgentReachable:  true,
		PolledAt:        t0,
		Agent:           agentapi.AgentStatus{AppliedVersion: bundle, LastApplyVersion: bundle, AsteriskUp: true},
		CompiledVersion: bundle,
	}
}

const (
	cT = metav1.ConditionTrue
	cF = metav1.ConditionFalse
	cU = metav1.ConditionUnknown
)

func cond(t string, s metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return drivers.Condition(t, s, reason, msg)
}

func short(v string) string { return v[:12] }

// Expected conditions of the desk phone.
var (
	faPending = cond(v1alpha1.ConditionFirmwareAvailable, cF, cisco79x5.ReasonManifestPending,
		"the manifest has no SHA-256 yet for "+strings.Join(ciscoFiles, ", "))
	faStaged      = cond(v1alpha1.ConditionFirmwareAvailable, cT, cisco79x5.ReasonStaged, cisco79x5.DefaultLoad)
	fcNotReported = cond(v1alpha1.ConditionFirmwareCurrent, cU, cisco79x5.ReasonNotReported, "it has not reported its load yet")
	fcRunning     = cond(v1alpha1.ConditionFirmwareCurrent, cT, cisco79x5.ReasonRunning, cisco79x5.DefaultLoad)
	provNone      = cond(v1alpha1.ConditionProvisioned, cF, cisco79x5.ReasonNotFetched, "it has not fetched "+deskSEP)
	provNow       = cond(v1alpha1.ConditionProvisioned, cT, cisco79x5.ReasonFetched, "fetched version "+short(vNow))
	provOld       = cond(v1alpha1.ConditionProvisioned, cF, cisco79x5.ReasonOldVersion,
		"fetched version "+short(vOld)+", current "+short(vNow))
	regNone    = cond(v1alpha1.ConditionRegistered, cF, cisco79x5.ReasonNoContact, "no SIP registration")
	regLive    = cond(v1alpha1.ConditionRegistered, cT, cisco79x5.ReasonContact, deskContact+", "+deskUA)
	regQualify = cond(v1alpha1.ConditionRegistered, cF, cisco79x5.ReasonQualifyFailed, deskContact+" does not answer OPTIONS")
	cfgNone    = cond(v1alpha1.ConditionConfigCurrent, cF, cisco79x5.ReasonNotFetched, "it has not fetched "+deskSEP)
	cfgNow     = cond(v1alpha1.ConditionConfigCurrent, cT, cisco79x5.ReasonCurrent, "version "+short(vNow))
	cfgPending = cond(v1alpha1.ConditionConfigCurrent, cF, v1alpha1.ReasonApplyPending, "the PBX agent has not applied the latest config yet")
	asWritten  = cond(v1alpha1.ConditionLabelShortened, cF, cisco79x5.ReasonAsWritten, "")
)

func faMissing(files ...string) metav1.Condition {
	return cond(v1alpha1.ConditionFirmwareAvailable, cF, cisco79x5.ReasonNotStaged, "missing: "+strings.Join(files, ", "))
}

func fcOther(running string) metav1.Condition {
	return cond(v1alpha1.ConditionFirmwareCurrent, cF, cisco79x5.ReasonOtherLoad, "running "+running+", target "+cisco79x5.DefaultLoad)
}

func cfgFailed(msg string) metav1.Condition {
	return cond(v1alpha1.ConditionConfigCurrent, cF, v1alpha1.ReasonApplyFailed, msg)
}

func cfgStale(suffix string) metav1.Condition {
	return cond(v1alpha1.ConditionConfigCurrent, cF, cisco79x5.ReasonStale,
		"running version "+short(vOld)+", current "+short(vNow)+suffix)
}

func ready(s metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return cond(v1alpha1.ConditionReady, s, reason, msg)
}

// conds is the standard condition list in the driver's order; label
// replaces LabelShortened's default (False AsWritten).
func conds(rd, fa, fc, prov, reg, cfg metav1.Condition, label ...metav1.Condition) []metav1.Condition {
	ls := asWritten
	if len(label) > 0 {
		ls = label[0]
	}
	return []metav1.Condition{
		rd, fa, fc, prov, reg, cfg,
		cond(v1alpha1.ConditionNumberConflict, cF, cisco79x5.ReasonNoConflict, ""),
		cond(v1alpha1.ConditionMACConflict, cF, cisco79x5.ReasonNoConflict, ""),
		cond(v1alpha1.ConditionUnsupported, cF, cisco79x5.ReasonAllSupported, ""),
		ls,
	}
}

// invalidConds is Invalid's condition list.
func invalidConds(reason, msg string, numberConflict, macConflict metav1.Condition) []metav1.Condition {
	u := func(t string) metav1.Condition { return cond(t, cU, cisco79x5.ReasonInvalid, "") }
	return []metav1.Condition{
		ready(cF, reason, msg),
		u(v1alpha1.ConditionFirmwareAvailable), u(v1alpha1.ConditionFirmwareCurrent), u(v1alpha1.ConditionProvisioned),
		u(v1alpha1.ConditionRegistered), u(v1alpha1.ConditionConfigCurrent),
		numberConflict, macConflict,
		cond(v1alpha1.ConditionUnsupported, cF, cisco79x5.ReasonAllSupported, ""),
		asWritten,
	}
}

var (
	noNumberConflict = cond(v1alpha1.ConditionNumberConflict, cF, cisco79x5.ReasonNoConflict, "")
	noMACConflict    = cond(v1alpha1.ConditionMACConflict, cF, cisco79x5.ReasonNoConflict, "")
)

// Human-action texts (plan §4 T1).
var (
	p2 = "connect it to a VLAN 20 switch port; it will request SEPD0C78915E8C8.cnf.xml from 192.168.0.240"
	p3 = func(files ...string) string {
		return "run `patchbay firmware load <zip>` with " + cisco + "; missing: " + strings.Join(files, ", ")
	}
	p4 = "the cisco79x5 firmware manifest has no SHA-256 yet for " + strings.Join(ciscoFiles, ", ") +
		": put each file's sha256sum from " + cisco + " into internal/drivers/cisco79x5/firmware.go, " +
		"release patchbay with it, then run `patchbay firmware load <zip>`"
	p5  = "factory reset: hold # while powering on, then 123456789*0#"
	p6  = "stage firmware first: run `patchbay firmware load <zip>` with " + cisco + "; a factory reset with nothing to download leaves the phone stuck"
	p7  = "upgrading: do not unplug"
	p8  = "upgrade stuck: check the phone's screen"
	p9  = "its Admin. VLAN ID is 20 but its switch port is untagged; clear it with a factory reset (its settings menu is restricted): hold # while powering on, then 123456789*0#"
	p9b = "its Admin. VLAN ID is 20 but its switch port is untagged; stage firmware first with `patchbay firmware load <zip>` (" + cisco + "), then factory-reset it: hold # while powering on, then 123456789*0#"
	p10 = "it runs SCCP45.9-4-2SR4-3S, not SIP45.9-4-2SR4-3S: power-cycle it so it fetches its config and upgrades"
	p11 = "it has not fetched SEPD0C78915E8C8.cnf.xml from 192.168.0.240: check its TFTP server (DHCP option 66)"
	p12 = func(min int, tail string) string {
		return fmt.Sprintf("fetched its config %d min ago but has not registered; %s", min, tail)
	}
	p13  = "press Settings then **#**, or power-cycle it"
	p14  = "not responding; power-cycle it"
	req1 = "2026-10-14T11:59:00Z"
)

const (
	sip45  = "SIP45.9-4-2SR4-3S"
	sccp45 = "SCCP45.9-4-2SR4-3S"
	jarSIP = "jar45sip.9-4-2SR4-3.sbn"
)

func cfgFetch(v string, at time.Time) *v1alpha1.ConfigFetch {
	return &v1alpha1.ConfigFetch{Version: v, At: *mt(at)}
}

func resyncAction(v string, at time.Time, attempts int32) *v1alpha1.LastAction {
	return &v1alpha1.LastAction{Kind: v1alpha1.ActionResync, Version: v, At: *mt(at), Attempts: attempts}
}

func resyncNow() []drivers.Action {
	return []drivers.Action{{Kind: v1alpha1.ActionResync, Version: vNow}}
}

// decideCase is one Decide input and the whole Decision it must give.
type decideCase struct {
	name string
	drv  *cisco79x5.Driver // nil: cisco79x5.New()
	dev  drivers.Device
	obs  drivers.Observations
	want drivers.Decision
}

// pinned is a driver whose manifest has digests, so the load can be staged.
func pinned() *cisco79x5.Driver { return cisco79x5.NewWithLoads(pinnedLoads()) }

// deskWith is the desk phone with memory m (and the changes in mods).
func deskWith(m drivers.Memory, mods ...func(*drivers.Device)) drivers.Device {
	d := desk()
	d.Memory = m
	for _, f := range mods {
		f(&d)
	}
	return d
}

func obsWith(d *agentapi.DeviceObservations, mods ...func(*drivers.Observations)) drivers.Observations {
	o := healthy()
	o.Device = d
	for _, f := range mods {
		f(&o)
	}
	return o
}

func withStore(s agentapi.FirmwareStore) func(*drivers.Observations) {
	return func(o *drivers.Observations) { o.Firmware = s }
}

func withRequest(r string) func(*drivers.Device) {
	return func(d *drivers.Device) { d.Request = r }
}

// groups are the TestDecide groups, one per row (1 to 10); a case expects
// its group's row unless its name names another ("→ row N").
var groups = []struct {
	name  string
	row   int
	cases func() []decideCase
}{
	{"1 Invalid", 1, invalidCases},
	{"2 Unknown", 2, unknownCases},
	{"3 Unseen", 3, unseenCases},
	{"4 NeedsFirmware", 4, needsFirmwareCases},
	{"5 NeedsTrustReset", 5, needsTrustResetCases},
	{"6 Upgrading", 6, upgradingCases},
	{"7 Registering", 7, registeringCases},
	{"8 ConfigStale", 8, configStaleCases},
	{"9 Registered", 9, registeredCases},
	{"10 Unreachable", 10, unreachableCases},
}

// TestDecide asserts the whole Decision for every row of the rule table,
// every branch of its outcome and every timeout just before, at and after
// its limit (plan §4 T1). Each case also checks that the row the table
// picks is the first whose predicate holds, and the test checks that every
// row is reached.
func TestDecide(t *testing.T) {
	reached := map[int]bool{}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			for _, c := range g.cases() {
				t.Run(c.name, func(t *testing.T) {
					drv := c.drv
					if drv == nil {
						drv = cisco79x5.New()
					}
					before := fmtMemory(c.dev.Memory)
					got := drv.Decide(c.dev, c.obs, settings, t0)
					if diff := diffDecision(got, c.want); diff != "" {
						t.Errorf("Decision differs:\n%s", diff)
					}
					if fmtMemory(c.dev.Memory) != before {
						t.Error("Decide changed dev.Memory")
					}
					row := g.row
					if i := strings.Index(c.name, "→ row "); i >= 0 {
						if _, err := fmt.Sscanf(c.name[i+len("→ row "):], "%d", &row); err != nil {
							t.Fatal(err)
						}
					}
					rows := drv.MatchingRows(c.dev, c.obs, settings, t0)
					if len(rows) == 0 || rows[0] != row {
						t.Errorf("matching rows %v; want row %d first", rows, row)
					}
					if len(rows) > 0 {
						reached[rows[0]] = true
					}
				})
			}
		})
	}
	for row := 1; row <= 10; row++ {
		if !reached[row] {
			t.Errorf("no case reaches row %d", row)
		}
	}
}

// TestRuleOrder: the rule table is design §8.3's rows, in its order.
func TestRuleOrder(t *testing.T) {
	want := []drivers.State{
		drivers.StateInvalid, drivers.StateUnknown, drivers.StateUnseen, drivers.StateNeedsFirmware,
		drivers.StateNeedsTrustReset, drivers.StateUpgrading, drivers.StateRegistering,
		drivers.StateConfigStale, drivers.StateRegistered, drivers.StateUnreachable,
	}
	if got := cisco79x5.RuleStates(); !slices.Equal(got, want) {
		t.Errorf("rule order = %v, want %v", got, want)
	}
}

func invalidCases() []decideCase {
	claimMAC := "MAC D0C78915E8C8 is held by Phone kitchen, which is older"
	claimNumber := "number 100 is held by Softphone chris-laptop, which is older"
	macTrue := cond(v1alpha1.ConditionMACConflict, cT, cisco79x5.ReasonMACConflict, claimMAC)
	numberTrue := func(msg string) metav1.Condition {
		return cond(v1alpha1.ConditionNumberConflict, cT, cisco79x5.ReasonNumberConflict, msg)
	}
	unknownModel := `cisco79x5 does not serve model "cisco-7940"`
	p1 := "no firmware load SIP45.9-4-2SR3-1S for cisco-7945 in the cisco79x5 manifest"
	p1for7975 := "no firmware load SIP75.9-4-2SR4-3S for cisco-7975 in the cisco79x5 manifest"
	return []decideCase{
		{
			name: "unknown model",
			dev:  deskWith(drivers.Memory{}, func(d *drivers.Device) { d.Model = "cisco-7940" }),
			obs:  healthy(),
			want: drivers.Decision{
				State:       drivers.StateInvalid,
				Conditions:  invalidConds(cisco79x5.ReasonUnknownModel, unknownModel, noNumberConflict, noMACConflict),
				HumanAction: unknownModel,
			},
		},
		{
			name: "MAC conflict, never seen (the loser has no observations)",
			dev: deskWith(drivers.Memory{}, func(d *drivers.Device) {
				d.MACConflict, d.ClaimMessage = true, claimMAC
			}),
			obs: healthy(),
			want: drivers.Decision{
				State:       drivers.StateInvalid,
				Conditions:  invalidConds(cisco79x5.ReasonMACConflict, claimMAC, noNumberConflict, macTrue),
				HumanAction: claimMAC,
			},
		},
		{
			name: "number conflict",
			dev: deskWith(drivers.Memory{}, func(d *drivers.Device) {
				d.NumberConflict, d.ClaimMessage = true, claimNumber
			}),
			obs: healthy(),
			want: drivers.Decision{
				State:       drivers.StateInvalid,
				Conditions:  invalidConds(cisco79x5.ReasonNumberConflict, claimNumber, numberTrue(claimNumber), noMACConflict),
				HumanAction: claimNumber,
			},
		},
		{
			name: "number out of plan",
			dev: deskWith(drivers.Memory{}, func(d *drivers.Device) {
				d.Number, d.NumberOutOfPlan, d.ClaimMessage = "600", true, "600 is the echo test"
			}),
			obs: healthy(),
			want: drivers.Decision{
				State:       drivers.StateInvalid,
				Conditions:  invalidConds(v1alpha1.ReasonNumberNotInPlan, "600 is the echo test", noNumberConflict, noMACConflict),
				HumanAction: "600 is the echo test",
			},
		},
		{
			name: "number out of plan without a claim message",
			dev: deskWith(drivers.Memory{}, func(d *drivers.Device) {
				d.Number, d.NumberOutOfPlan = "1500", true
			}),
			obs: healthy(),
			want: drivers.Decision{
				State: drivers.StateInvalid,
				Conditions: invalidConds(v1alpha1.ReasonNumberNotInPlan, "number 1500 is not in the number plan's device range",
					noNumberConflict, noMACConflict),
				HumanAction: "number 1500 is not in the number plan's device range",
			},
		},
		{
			name: "firmware not in the manifest",
			dev:  deskWith(drivers.Memory{}, func(d *drivers.Device) { d.Firmware = "SIP45.9-4-2SR3-1S" }),
			obs:  healthy(),
			want: drivers.Decision{
				State:       drivers.StateInvalid,
				Conditions:  invalidConds(cisco79x5.ReasonUnknownFirmware, p1, noNumberConflict, noMACConflict),
				HumanAction: p1,
			},
		},
		{
			name: "7975: its default load is not in the manifest",
			dev:  deskWith(drivers.Memory{}, func(d *drivers.Device) { d.Model = string(v1alpha1.PhoneModelCisco7975) }),
			obs:  healthy(),
			want: drivers.Decision{
				State:       drivers.StateInvalid,
				Conditions:  invalidConds(cisco79x5.ReasonUnknownFirmware, p1for7975, noNumberConflict, noMACConflict),
				HumanAction: p1for7975,
			},
		},
		{
			name: "two reasons: MAC before number, both conditions True",
			dev: deskWith(drivers.Memory{}, func(d *drivers.Device) {
				d.MACConflict, d.NumberConflict, d.Firmware = true, true, "SIP45.9-4-2SR3-1S"
				d.ClaimMessage = claimMAC
			}),
			obs: healthy(),
			want: drivers.Decision{
				State:       drivers.StateInvalid,
				Conditions:  invalidConds(cisco79x5.ReasonMACConflict, claimMAC, numberTrue(claimMAC), macTrue),
				HumanAction: claimMAC,
			},
		},
		{
			name: "conflict without a claim message",
			dev:  deskWith(drivers.Memory{}, func(d *drivers.Device) { d.NumberConflict = true }),
			obs:  healthy(),
			want: drivers.Decision{
				State: drivers.StateInvalid,
				Conditions: invalidConds(cisco79x5.ReasonNumberConflict, "an older resource holds number 100",
					numberTrue(""), noMACConflict),
				HumanAction: "an older resource holds number 100",
			},
		},
		{
			name: "a pending request is marked handled, no action",
			dev: deskWith(drivers.Memory{HandledRequest: "older"}, withRequest(req1), func(d *drivers.Device) {
				d.NumberConflict, d.ClaimMessage = true, claimNumber
			}),
			obs: healthy(),
			want: drivers.Decision{
				State:       drivers.StateInvalid,
				Conditions:  invalidConds(cisco79x5.ReasonNumberConflict, claimNumber, numberTrue(claimNumber), noMACConflict),
				StatusPatch: drivers.Memory{HandledRequest: req1},
				HumanAction: claimNumber,
			},
		},
		{
			name: "before Unknown, and memory is folded (upgrade kept)",
			dev: deskWith(drivers.Memory{UpgradeStartedAt: mt(ago(time.Hour))}, func(d *drivers.Device) {
				d.NumberConflict, d.ClaimMessage = true, claimNumber
			}),
			obs: obsWith(seen(sepFetch(ago(2*time.Hour), vNow)), func(o *drivers.Observations) {
				o.AgentReachable = false
			}),
			want: drivers.Decision{
				State:      drivers.StateInvalid,
				Conditions: invalidConds(cisco79x5.ReasonNumberConflict, claimNumber, numberTrue(claimNumber), noMACConflict),
				StatusPatch: drivers.Memory{
					FirstSeen: mt(ago(2 * time.Hour)), Address: deskIP,
					LastConfigFetch: cfgFetch(vNow, ago(2*time.Hour)), UpgradeStartedAt: mt(ago(time.Hour)),
				},
				HumanAction: claimNumber,
			},
		},
	}
}

func unknownCases() []decideCase {
	mem := drivers.Memory{
		FirstSeen: mt(ago(time.Hour)), Firmware: sip45, Address: deskIP,
		LastConfigFetch: cfgFetch(vNow, ago(time.Hour)), LastRegistered: mt(ago(5 * time.Minute)),
		LastAction: resyncAction(vNow, ago(time.Hour), 1), HandledRequest: "older",
	}
	unreachable := []metav1.Condition{
		ready(cU, v1alpha1.ReasonAgentUnreachable, "cannot reach the PBX agent; registration unknown"),
		cond(v1alpha1.ConditionRegistered, cU, v1alpha1.ReasonAgentUnreachable, "cannot reach the PBX agent; registration unknown"),
	}
	registered := func(d *drivers.Device) { d.LastState = drivers.StateRegistered }
	return []decideCase{
		{
			name: "polled exactly StaleAfter ago is fresh → row 3",
			dev:  desk(),
			obs:  obsWith(nil, func(o *drivers.Observations) { o.PolledAt = ago(30 * time.Second) }),
			want: drivers.Decision{
				State:        drivers.StateUnseen,
				Conditions:   conds(ready(cF, cisco79x5.ReasonUnseen, p2), faPending, fcNotReported, provNone, regNone, cfgNone),
				HumanAction:  p2,
				RequeueAfter: 10 * time.Minute,
			},
		},
		{
			name: "polled StaleAfter+1ns ago is stale: last state kept",
			dev:  deskWith(mem, registered),
			obs:  obsWith(nil, func(o *drivers.Observations) { o.PolledAt = ago(30*time.Second + time.Nanosecond) }),
			want: drivers.Decision{State: drivers.StateRegistered, Conditions: unreachable, StatusPatch: mem},
		},
		{
			name: "agent unreachable",
			dev:  deskWith(mem, registered),
			obs:  obsWith(nil, func(o *drivers.Observations) { o.AgentReachable = false }),
			want: drivers.Decision{State: drivers.StateRegistered, Conditions: unreachable, StatusPatch: mem},
		},
		{
			name: "Asterisk down: PBXDown, Ready and Registered False",
			dev:  deskWith(mem, registered),
			obs:  obsWith(nil, func(o *drivers.Observations) { o.Agent.AsteriskUp = false }),
			want: drivers.Decision{
				State: drivers.StateRegistered,
				Conditions: []metav1.Condition{
					ready(cF, v1alpha1.ReasonPBXDown, "Asterisk is down"),
					cond(v1alpha1.ConditionRegistered, cF, v1alpha1.ReasonPBXDown, "Asterisk is down"),
				},
				StatusPatch: mem,
			},
		},
		{
			name: "no last state: Unknown",
			dev:  deskWith(mem),
			obs:  obsWith(nil, func(o *drivers.Observations) { o.AgentReachable = false }),
			want: drivers.Decision{State: drivers.StateUnknown, Conditions: unreachable, StatusPatch: mem},
		},
		{
			name: "memory and request untouched by what the stale agent shows",
			dev:  deskWith(mem, registered, withRequest(req1)),
			obs: obsWith(&agentapi.DeviceObservations{
				Fetches:      []agentapi.Fetch{sepFetch(ago(time.Second), vOld)},
				Registration: contact(t0.Add(time.Minute)),
				Web:          web(sccp45, ""),
			}, func(o *drivers.Observations) { o.PolledAt = ago(time.Hour) }),
			want: drivers.Decision{State: drivers.StateRegistered, Conditions: unreachable, StatusPatch: mem},
		},
	}
}

func unseenCases() []decideCase {
	unseen := drivers.Decision{
		State:        drivers.StateUnseen,
		Conditions:   conds(ready(cF, cisco79x5.ReasonUnseen, p2), faPending, fcNotReported, provNone, regNone, cfgNone),
		HumanAction:  p2,
		RequeueAfter: 10 * time.Minute,
	}
	agentRestarted := unseen
	agentRestarted.State = drivers.StateRegistering
	agentRestarted.Conditions = conds(ready(cF, cisco79x5.ReasonRegistering, "waiting for it to register"),
		faPending, fcNotReported, provNone, regNone, cfgNone)
	agentRestarted.StatusPatch = drivers.Memory{FirstSeen: mt(ago(2 * time.Minute))}
	agentRestarted.HumanAction = ""
	agentRestarted.RequeueAfter = 3 * time.Minute
	handled := unseen
	handled.StatusPatch = drivers.Memory{HandledRequest: req1}
	return []decideCase{
		{name: "no observations (the agent saw only other MACs)", dev: desk(), obs: healthy(), want: unseen},
		{
			name: "observed only by a failed scrape",
			dev:  desk(),
			obs:  obsWith(&agentapi.DeviceObservations{Device: "phone-desk", Web: &agentapi.WebScrape{At: t0, Error: "no address"}}),
			want: unseen,
		},
		{
			name: "first seen but no observations (agent restarted) → row 7",
			dev:  deskWith(drivers.Memory{FirstSeen: mt(ago(2 * time.Minute))}),
			obs:  healthy(),
			want: agentRestarted,
		},
		{name: "a pending request is dropped", dev: deskWith(drivers.Memory{}, withRequest(req1)), obs: healthy(), want: handled},
	}
}

// fetchedConfig is the desk phone's memory after fetching vNow 10 minutes
// ago, not registered.
func fetchedConfig() drivers.Memory {
	return drivers.Memory{FirstSeen: mt(ago(10 * time.Minute)), Address: deskIP, LastConfigFetch: cfgFetch(vNow, ago(10*time.Minute))}
}

func needsFirmwareCases() []decideCase {
	withFirmware := func(fw string) drivers.Memory {
		m := fetchedConfig()
		m.Firmware = fw
		return m
	}
	notFound := func(file string, at time.Time) agentapi.Fetch { return fetch(file, agentapi.FetchNotFound, at) }
	return []decideCase{
		{
			name: "SCCP versionID, manifest pending",
			dev:  desk(),
			obs:  obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(10*time.Minute), vNow)}, Web: web(sccp45, "jar45sccp.9-4-2SR4-3.sbn")}),
			want: drivers.Decision{
				State:       drivers.StateNeedsFirmware,
				Conditions:  conds(ready(cF, cisco79x5.ReasonNeedsFirmware, p4), faPending, fcOther(sccp45), provNow, regNone, cfgNow),
				StatusPatch: withFirmware(sccp45),
				HumanAction: p4,
			},
		},
		{
			name: "SCCP appLoadID, store empty: lists all 8 files",
			drv:  pinned(),
			dev:  desk(),
			obs:  obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(10*time.Minute), vNow)}, Web: web("", "jar45sccp.9-4-2SR4-3.sbn")}),
			want: drivers.Decision{
				State: drivers.StateNeedsFirmware,
				Conditions: conds(ready(cF, cisco79x5.ReasonNeedsFirmware, p3(ciscoFiles...)), faMissing(ciscoFiles...),
					fcOther("jar45sccp.9-4-2SR4-3.sbn"), provNow, regNone, cfgNow),
				StatusPatch: withFirmware("jar45sccp.9-4-2SR4-3.sbn"),
				HumanAction: p3(ciscoFiles...),
			},
		},
		{
			name: "web never read, the load requested after the config fetch",
			dev:  desk(),
			obs:  obsWith(seen(sepFetch(ago(10*time.Minute), vNow), notFound("SIP45.9-4-2SR4-3S.loads", ago(9*time.Minute)))),
			want: drivers.Decision{
				State:       drivers.StateNeedsFirmware,
				Conditions:  conds(ready(cF, cisco79x5.ReasonNeedsFirmware, p4), faPending, fcNotReported, provNow, regNone, cfgNow),
				StatusPatch: fetchedConfig(),
				HumanAction: p4,
			},
		},
		{
			name: "the load requested at the config fetch's instant",
			dev:  desk(),
			obs:  obsWith(seen(sepFetch(ago(10*time.Minute), vNow), notFound("SIP45.9-4-2SR4-3S.loads", ago(10*time.Minute)))),
			want: drivers.Decision{
				State:       drivers.StateNeedsFirmware,
				Conditions:  conds(ready(cF, cisco79x5.ReasonNeedsFirmware, p4), faPending, fcNotReported, provNow, regNone, cfgNow),
				StatusPatch: fetchedConfig(),
				HumanAction: p4,
			},
		},
		{
			name: "the load requested before the config fetch → row 7",
			dev:  desk(),
			obs:  obsWith(seen(notFound("SIP45.9-4-2SR4-3S.loads", ago(11*time.Minute)), sepFetch(ago(10*time.Minute), vNow))),
			want: drivers.Decision{
				State: drivers.StateRegistering,
				Conditions: conds(ready(cF, cisco79x5.ReasonRegistering, p12(10, "its web page has not been read yet")),
					faPending, fcNotReported, provNow, regNone, cfgNow),
				StatusPatch: drivers.Memory{
					FirstSeen: mt(ago(11 * time.Minute)), Address: deskIP, LastConfigFetch: cfgFetch(vNow, ago(10*time.Minute)),
				},
				HumanAction: p12(10, "its web page has not been read yet"),
			},
		},
		{
			name: "term45.default.loads requested (a factory reset)",
			dev:  desk(),
			obs:  obsWith(seen(sepFetch(ago(10*time.Minute), vNow), notFound("term45.default.loads", ago(5*time.Minute)))),
			want: drivers.Decision{
				State:       drivers.StateNeedsFirmware,
				Conditions:  conds(ready(cF, cisco79x5.ReasonNeedsFirmware, p4), faPending, fcNotReported, provNow, regNone, cfgNow),
				StatusPatch: fetchedConfig(),
				HumanAction: p4,
			},
		},
		{
			name: "partially staged: lists the rest",
			drv:  pinned(),
			dev:  desk(),
			obs: obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(10*time.Minute), vNow)}, Web: web(sccp45, "")},
				withStore(staged(ciscoFiles[:3]...))),
			want: drivers.Decision{
				State: drivers.StateNeedsFirmware,
				Conditions: conds(ready(cF, cisco79x5.ReasonNeedsFirmware, p3(ciscoFiles[3:]...)), faMissing(ciscoFiles[3:]...),
					fcOther(sccp45), provNow, regNone, cfgNow),
				StatusPatch: withFirmware(sccp45),
				HumanAction: p3(ciscoFiles[3:]...),
			},
		},
		{
			name: "staged: not this row → row 7",
			drv:  pinned(),
			dev:  desk(),
			obs: obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(10*time.Minute), vNow)}, Web: web(sccp45, "")},
				withStore(staged())),
			want: drivers.Decision{
				State:       drivers.StateRegistering,
				Conditions:  conds(ready(cF, cisco79x5.ReasonRegistering, p10), faStaged, fcOther(sccp45), provNow, regNone, cfgNow),
				StatusPatch: withFirmware(sccp45),
				HumanAction: p10,
			},
		},
		{
			name: "it reports the target's image: not this row → row 7",
			dev:  desk(),
			obs:  obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(10*time.Minute), vNow)}, Web: web("", jarSIP)}),
			want: drivers.Decision{
				State: drivers.StateRegistering,
				Conditions: conds(ready(cF, cisco79x5.ReasonRegistering, p12(10, "its log is empty")),
					faPending, fcRunning, provNow, regNone, cfgNow),
				StatusPatch: withFirmware(jarSIP),
				HumanAction: p12(10, "its log is empty"),
			},
		},
	}
}

func needsTrustResetCases() []decideCase {
	sgn := func(at time.Time) agentapi.Fetch { return fetch(deskSEP+".sgn", agentapi.FetchNotFound, at) }
	// The phone asked only for the signed config, at `at`.
	sgnOnly := func(at time.Time) (drivers.Device, drivers.Observations, drivers.Memory) {
		return desk(), obsWith(seen(sgn(at))), drivers.Memory{FirstSeen: mt(at), Address: deskIP}
	}
	trust := func(human string, m drivers.Memory, prov, cfg metav1.Condition, fa metav1.Condition) drivers.Decision {
		return drivers.Decision{
			State:       drivers.StateNeedsTrustReset,
			Conditions:  conds(ready(cF, cisco79x5.ReasonNeedsTrustReset, human), fa, fcNotReported, prov, regNone, cfg),
			StatusPatch: m,
			HumanAction: human,
		}
	}
	var cases []decideCase

	dev, obs, m := sgnOnly(ago(3*time.Minute - time.Second))
	cases = append(cases, decideCase{
		name: "sgn 3m-1s ago → row 7", dev: dev, obs: obs,
		want: drivers.Decision{
			State: drivers.StateRegistering,
			Conditions: conds(ready(cF, cisco79x5.ReasonRegistering, "waiting for it to register"),
				faPending, fcNotReported, provNone, regNone, cfgNone),
			StatusPatch:  m,
			RequeueAfter: time.Second,
		},
	})
	for _, d := range []time.Duration{3 * time.Minute, 3*time.Minute + time.Second} {
		dev, obs, m := sgnOnly(ago(d))
		cases = append(cases, decideCase{
			name: fmt.Sprintf("sgn %s ago, default load not staged", d), dev: dev, obs: obs,
			want: trust(p6, m, provNone, cfgNone, faPending),
		})
	}
	dev, obs, m = sgnOnly(ago(3 * time.Minute))
	obs.Firmware = staged()
	cases = append(cases, decideCase{
		name: "default load staged", drv: pinned(), dev: dev, obs: obs,
		want: trust(p5, m, provNone, cfgNone, faStaged),
	})

	served := obsWith(seen(sgn(ago(10*time.Minute)), sepFetch(ago(9*time.Minute), vNow)))
	cases = append(cases, decideCase{
		name: "served its config after the sgn → row 7", dev: desk(), obs: served,
		want: drivers.Decision{
			State: drivers.StateRegistering,
			Conditions: conds(ready(cF, cisco79x5.ReasonRegistering, p12(9, "its web page has not been read yet")),
				faPending, fcNotReported, provNow, regNone, cfgNow),
			StatusPatch: drivers.Memory{
				FirstSeen: mt(ago(10 * time.Minute)), Address: deskIP, LastConfigFetch: cfgFetch(vNow, ago(9*time.Minute)),
			},
			HumanAction: p12(9, "its web page has not been read yet"),
		},
	})

	withContact := obsWith(&agentapi.DeviceObservations{
		Fetches:      []agentapi.Fetch{sepFetch(ago(20*time.Minute), vNow), sgn(ago(10 * time.Minute))},
		Registration: contact(t0.Add(5 * time.Minute)),
	})
	cases = append(cases, decideCase{
		name: "a live contact → row 9", dev: desk(), obs: withContact,
		want: drivers.Decision{
			State:      drivers.StateRegistered,
			Conditions: conds(ready(cT, cisco79x5.ReasonRegistered, ""), faPending, fcNotReported, provNow, regLive, cfgNow),
			StatusPatch: drivers.Memory{
				FirstSeen: mt(ago(20 * time.Minute)), Address: deskIP,
				LastConfigFetch: cfgFetch(vNow, ago(20*time.Minute)), LastRegistered: mt(t0),
			},
			RequeueAfter: 10 * time.Minute,
		},
	})

	logged := func(lines ...string) drivers.Observations {
		w := web("", "", lines...)
		w.DeviceInformation = nil
		return obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(time.Minute), vNow)}, Web: w})
	}
	oneMinute := drivers.Memory{FirstSeen: mt(ago(time.Minute)), Address: deskIP, LastConfigFetch: cfgFetch(vNow, ago(time.Minute))}
	cases = append(cases,
		decideCase{
			name: "trust error in the log: at once", dev: desk(),
			obs:  logged("SEPD0C78915E8C8.cnf.xml.sgn not found", "Trust List Update Failed"),
			want: trust(p6, oneMinute, provNow, cfgNow, faPending),
		},
		decideCase{
			name: "config verification error in the log", dev: desk(),
			obs:  logged("Config file verification failed"),
			want: trust(p6, oneMinute, provNow, cfgNow, faPending),
		},
		decideCase{
			name: "Load Authentication Failed is not a trust error → row 7", dev: desk(),
			obs: logged("Load Authentication Failed", "Load Auth Failed: SIP45.9-4-2SR4-3S.loads CTL"),
			want: drivers.Decision{
				State: drivers.StateRegistering,
				Conditions: conds(ready(cF, cisco79x5.ReasonRegistering, "waiting for it to register"),
					faPending, fcNotReported, provNow, regNone, cfgNow),
				StatusPatch:  oneMinute,
				RequeueAfter: 4 * time.Minute,
			},
		},
	)
	return cases
}

func upgradingCases() []decideCase {
	// An SCCP phone with the SIP load staged, which fetched its config 10
	// minutes ago.
	base := func() drivers.Memory {
		m := fetchedConfig()
		m.FirstSeen = mt(ago(time.Hour))
		m.Firmware = sccp45
		return m
	}
	obs := func(fetches ...agentapi.Fetch) drivers.Observations {
		return obsWith(&agentapi.DeviceObservations{
			Fetches: append([]agentapi.Fetch{sepFetch(ago(10*time.Minute), vNow)}, fetches...),
			Web:     web(sccp45, ""),
		}, withStore(staged()))
	}
	upgrading := func(human string, m drivers.Memory, requeue time.Duration) drivers.Decision {
		return drivers.Decision{
			State:        drivers.StateUpgrading,
			Conditions:   conds(ready(cF, cisco79x5.ReasonUpgrading, human), faStaged, fcOther(sccp45), provNow, regNone, cfgNow),
			StatusPatch:  m,
			HumanAction:  human,
			RequeueAfter: requeue,
		}
	}
	started := func(at time.Time) drivers.Memory {
		m := base()
		m.UpgradeStartedAt = mt(at)
		return m
	}
	loads := fwFetch("SIP45.9-4-2SR4-3S.loads", ago(5*time.Minute))
	var cases []decideCase

	cases = append(cases,
		decideCase{
			name: "firmware fetched 3m-1s ago; first sight sets UpgradeStartedAt",
			drv:  pinned(), dev: deskWith(base()),
			obs:  obs(loads, fwFetch("apps45.9-4-2SR4-3.sbn", ago(3*time.Minute-time.Second))),
			want: upgrading(p7, started(ago(5*time.Minute)), time.Second),
		},
		decideCase{
			name: "firmware fetched 3m ago, upgrade in progress",
			drv:  pinned(), dev: deskWith(started(ago(5 * time.Minute))),
			obs:  obs(loads, fwFetch("apps45.9-4-2SR4-3.sbn", ago(3*time.Minute))),
			want: upgrading(p7, started(ago(5*time.Minute)), 15*time.Minute),
		},
		decideCase{
			name: "firmware fetched 3m ago, no upgrade recorded → row 7",
			drv:  pinned(), dev: deskWith(base()),
			obs: obs(loads, fwFetch("apps45.9-4-2SR4-3.sbn", ago(3*time.Minute))),
			want: drivers.Decision{
				State:       drivers.StateRegistering,
				Conditions:  conds(ready(cF, cisco79x5.ReasonRegistering, p10), faStaged, fcOther(sccp45), provNow, regNone, cfgNow),
				StatusPatch: base(),
				HumanAction: p10,
			},
		},
		decideCase{
			name: "config served after the last firmware fetch: the upgrade ended → row 7",
			drv:  pinned(), dev: deskWith(started(ago(5 * time.Minute))),
			obs: obs(fwFetch("apps45.9-4-2SR4-3.sbn", ago(4*time.Minute)), sepFetch(ago(2*time.Minute), vNow)),
			want: drivers.Decision{
				State: drivers.StateRegistering,
				Conditions: conds(ready(cF, cisco79x5.ReasonRegistering, "waiting for it to register"),
					faStaged, fcOther(sccp45), provNow, regNone, cfgNow),
				StatusPatch: func() drivers.Memory {
					m := base()
					m.LastConfigFetch = cfgFetch(vNow, ago(2*time.Minute))
					return m
				}(),
				RequeueAfter: 3 * time.Minute,
			},
		},
		decideCase{
			name: "first sight: the earliest firmware fetch of the last 20 min",
			drv:  pinned(), dev: deskWith(base()),
			obs: obs(fwFetch("SIP45.9-4-2SR4-3S.loads", ago(25*time.Minute)), fwFetch("apps45.9-4-2SR4-3.sbn", ago(2*time.Minute)),
				fwFetch("cnu45.9-4-2SR4-3.sbn", ago(time.Minute))),
			want: upgrading(p7, started(ago(2*time.Minute)), 2*time.Minute),
		},
		decideCase{
			name: "a request is dropped",
			drv:  pinned(), dev: deskWith(started(ago(5*time.Minute)), withRequest(req1)),
			obs: obs(loads, fwFetch("apps45.9-4-2SR4-3.sbn", ago(time.Minute))),
			want: func() drivers.Decision {
				m := started(ago(5 * time.Minute))
				m.HandledRequest = req1
				return upgrading(p7, m, 2*time.Minute)
			}(),
		},
		decideCase{
			name: "agent restarted mid-upgrade: no fetches, a config fetch from before",
			drv:  pinned(), dev: deskWith(started(ago(8 * time.Minute))),
			obs:  obsWith(nil, withStore(staged())),
			want: upgrading(p7, started(ago(8*time.Minute)), 12*time.Minute),
		},
	)
	// The 20-minute mark, with no firmware fetch in the last 3 minutes.
	for _, c := range []struct {
		age     time.Duration
		human   string
		requeue time.Duration
	}{
		{20*time.Minute - time.Second, p7, time.Second},
		{20 * time.Minute, p8, 0},
		{20*time.Minute + time.Second, p8, 0},
	} {
		m := started(ago(c.age))
		m.LastConfigFetch = cfgFetch(vNow, ago(25*time.Minute))
		cases = append(cases, decideCase{
			name: fmt.Sprintf("upgrade started %s ago", c.age),
			drv:  pinned(), dev: deskWith(m),
			obs: obsWith(&agentapi.DeviceObservations{
				Fetches: []agentapi.Fetch{sepFetch(ago(25*time.Minute), vNow), fwFetch("apps45.9-4-2SR4-3.sbn", ago(19*time.Minute))},
				Web:     web(sccp45, ""),
			}, withStore(staged())),
			want: drivers.Decision{
				State:        drivers.StateUpgrading,
				Conditions:   conds(ready(cF, cisco79x5.ReasonUpgrading, c.human), faStaged, fcOther(sccp45), provNow, regNone, cfgNow),
				StatusPatch:  m,
				HumanAction:  c.human,
				RequeueAfter: c.requeue,
			},
		})
	}
	return cases
}

func registeringCases() []decideCase {
	// A SIP phone that fetched its config `age` ago and has not registered.
	at := func(age time.Duration, w *agentapi.WebScrape) (drivers.Device, drivers.Observations, drivers.Memory) {
		m := drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastConfigFetch: cfgFetch(vNow, ago(age))}
		if w != nil && w.DeviceInformation != nil {
			m.Firmware = w.DeviceInformation.VersionID
		}
		return deskWith(drivers.Memory{FirstSeen: mt(ago(time.Hour))}),
			obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(age), vNow)}, Web: w}), m
	}
	registering := func(human string, m drivers.Memory, fa, fc metav1.Condition, requeue time.Duration) drivers.Decision {
		rd := human
		if rd == "" {
			rd = "waiting for it to register"
		}
		return drivers.Decision{
			State:        drivers.StateRegistering,
			Conditions:   conds(ready(cF, cisco79x5.ReasonRegistering, rd), fa, fc, provNow, regNone, cfgNow),
			StatusPatch:  m,
			HumanAction:  human,
			RequeueAfter: requeue,
		}
	}
	logs := web(sip45, "", "line 1", "line 2", "line 3", "line 4")
	saying := "its log says: line 2 | line 3 | line 4"
	var cases []decideCase

	dev, obs, m := at(5*time.Minute-time.Second, logs)
	cases = append(cases, decideCase{name: "fetched 5m-1s ago", dev: dev, obs: obs,
		want: registering("", m, faPending, fcRunning, time.Second)})
	dev, obs, m = at(5*time.Minute, logs)
	cases = append(cases, decideCase{name: "fetched 5m ago: its log", dev: dev, obs: obs,
		want: registering(p12(5, saying), m, faPending, fcRunning, 0)})
	dev, obs, m = at(5*time.Minute+time.Second, logs)
	cases = append(cases, decideCase{name: "fetched 5m+1s ago", dev: dev, obs: obs,
		want: registering(p12(5, saying), m, faPending, fcRunning, 0)})
	dev, obs, m = at(12*time.Minute, web(sip45, "", "only line"))
	cases = append(cases, decideCase{name: "fewer than three log lines", dev: dev, obs: obs,
		want: registering(p12(12, "its log says: only line"), m, faPending, fcRunning, 0)})
	dev, obs, m = at(6*time.Minute, web(sip45, ""))
	cases = append(cases, decideCase{name: "an empty log", dev: dev, obs: obs,
		want: registering(p12(6, "its log is empty"), m, faPending, fcRunning, 0)})
	failed := &agentapi.WebScrape{At: t0, Address: deskIP, Error: "context deadline exceeded"}
	dev, obs, m = at(6*time.Minute, failed)
	cases = append(cases, decideCase{name: "web page could not be read", dev: dev, obs: obs,
		want: registering(p12(6, "its web page could not be read: context deadline exceeded"), m, faPending, fcNotReported, 0)})
	dev, obs, m = at(6*time.Minute, nil)
	cases = append(cases, decideCase{name: "web page not read yet", dev: dev, obs: obs,
		want: registering(p12(6, "its web page has not been read yet"), m, faPending, fcNotReported, 0)})

	vlan := func(w *agentapi.WebScrape) *agentapi.WebScrape {
		w.NetworkConfiguration.AdminVLAN = "20"
		return w
	}
	dev, obs, m = at(6*time.Minute, vlan(web(sip45, "", "line 1")))
	obs.Firmware = staged()
	cases = append(cases, decideCase{name: "Admin VLAN, default load staged", drv: pinned(), dev: dev, obs: obs,
		want: registering(p9, m, faStaged, fcRunning, 0)})
	dev, obs, m = at(6*time.Minute, vlan(web(sip45, "", "line 1")))
	cases = append(cases, decideCase{name: "Admin VLAN, default load not staged", dev: dev, obs: obs,
		want: registering(p9b, m, faPending, fcRunning, 0)})
	dev, obs, m = at(6*time.Minute, vlan(web(sccp45, "", "line 1")))
	obs.Firmware = staged()
	cases = append(cases, decideCase{name: "wrong firmware before Admin VLAN", drv: pinned(), dev: dev, obs: obs,
		want: registering(p10, m, faStaged, fcOther(sccp45), 0)})

	// Never fetched its config: it was first seen asking for its trust list
	// 6 minutes ago. P11 comes first even with an Admin VLAN.
	neverObs := obsWith(&agentapi.DeviceObservations{
		Fetches: []agentapi.Fetch{fetch("CTLSEPD0C78915E8C8.tlv", agentapi.FetchNotFound, ago(6*time.Minute))},
		Web:     vlan(web(sip45, "")),
	})
	cases = append(cases, decideCase{name: "never fetched its config, Admin VLAN set", dev: desk(), obs: neverObs,
		want: drivers.Decision{
			State: drivers.StateRegistering,
			Conditions: conds(ready(cF, cisco79x5.ReasonRegistering, p11), faPending, fcRunning, provNone, regNone,
				cfgNone),
			StatusPatch: drivers.Memory{FirstSeen: mt(ago(6 * time.Minute)), Address: deskIP, Firmware: sip45},
			HumanAction: p11,
		}})

	// Registered before, then fetched its config again (it rebooted).
	rebooted := drivers.Memory{
		FirstSeen: mt(ago(time.Hour)), Address: deskIP, Firmware: sip45,
		LastConfigFetch: cfgFetch(vNow, ago(10*time.Minute)), LastRegistered: mt(ago(20 * time.Minute)),
	}
	cases = append(cases, decideCase{name: "registered before the last config fetch (rebooted)", dev: deskWith(rebooted),
		obs:  obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(10*time.Minute), vNow)}, Web: web(sip45, "", "line 1")}),
		want: registering(p12(10, "its log says: line 1"), rebooted, faPending, fcRunning, 0)})

	// An upgrade that ended is cleared.
	ended := rebooted
	ended.UpgradeStartedAt = mt(ago(15 * time.Minute))
	cases = append(cases, decideCase{name: "clears an ended upgrade", dev: deskWith(ended),
		obs:  obsWith(&agentapi.DeviceObservations{Fetches: []agentapi.Fetch{sepFetch(ago(10*time.Minute), vNow)}, Web: web(sip45, "", "line 1")}),
		want: registering(p12(10, "its log says: line 1"), rebooted, faPending, fcRunning, 0)})
	return cases
}

// registeredMem is the desk phone registered 30 s ago, last fetched cfg.
func registeredMem(cfg *v1alpha1.ConfigFetch) drivers.Memory {
	return drivers.Memory{
		FirstSeen: mt(ago(time.Hour)), Address: deskIP, Firmware: sip45,
		LastConfigFetch: cfg, LastRegistered: mt(ago(30 * time.Second)),
	}
}

// registeredObs is what the agent sees of it.
func registeredObs(v string, calls int, mods ...func(*drivers.Observations)) drivers.Observations {
	return obsWith(&agentapi.DeviceObservations{
		Fetches:      []agentapi.Fetch{sepFetch(ago(30*time.Minute), v)},
		Registration: contact(t0.Add(5 * time.Minute)),
		Web:          web(sip45, jarSIP),
		Calls:        calls,
	}, mods...)
}

func configStaleCases() []decideCase {
	old := cfgFetch(vOld, ago(30*time.Minute))
	stale := func(m drivers.Memory, readyMsg, human string, cfg metav1.Condition, actions []drivers.Action, requeue time.Duration) drivers.Decision {
		return drivers.Decision{
			State:        drivers.StateConfigStale,
			Conditions:   conds(ready(cF, cisco79x5.ReasonConfigStale, readyMsg), faPending, fcRunning, provOld, regLive, cfg),
			Actions:      actions,
			StatusPatch:  m,
			HumanAction:  human,
			RequeueAfter: requeue,
		}
	}
	mem := func(mods ...func(*drivers.Memory)) drivers.Memory {
		m := registeredMem(old)
		for _, f := range mods {
			f(&m)
		}
		return m
	}
	action := func(a *v1alpha1.LastAction) func(*drivers.Memory) {
		return func(m *drivers.Memory) { m.LastAction = a }
	}
	handled := func(r string) func(*drivers.Memory) {
		return func(m *drivers.Memory) { m.HandledRequest = r }
	}
	// applied: the agent still serves bundle-6; its last apply was of last.
	applied := func(last, err string) func(*drivers.Observations) {
		return func(o *drivers.Observations) {
			o.Agent.AppliedVersion, o.Agent.LastApplyVersion, o.Agent.LastApplyError = "bundle-6", last, err
		}
	}
	const applyErr = "pjsip.conf: endpoint phone-desk: unknown option"
	waitMsg := "resync sent; waiting for it to fetch its config"
	var cases []decideCase
	cases = append(cases,
		decideCase{
			name: "(i) new config not live yet",
			dev:  deskWith(mem()),
			obs:  registeredObs(vOld, 0, applied("bundle-6", "")),
			want: stale(mem(), "waiting for the PBX agent to apply the new config", "", cfgPending, nil, 15*time.Second),
		},
		decideCase{
			name: "(i) new config refused",
			dev:  deskWith(mem()),
			obs:  registeredObs(vOld, 0, applied(bundle, applyErr)),
			want: stale(mem(), "the PBX agent refused the new config: "+applyErr, "", cfgFailed(applyErr), nil, 0),
		},
		decideCase{
			name: "(i) a refusal of an older bundle is not this one's",
			dev:  deskWith(mem()),
			obs:  registeredObs(vOld, 0, applied("bundle-5", applyErr)),
			want: stale(mem(), "waiting for the PBX agent to apply the new config", "", cfgPending, nil, 15*time.Second),
		},
		decideCase{
			name: "(i) a pending request waits for the config",
			dev:  deskWith(mem(), withRequest(req1)),
			obs:  registeredObs(vOld, 0, applied("bundle-6", "")),
			want: stale(mem(), "waiting for the PBX agent to apply the new config", "", cfgPending, nil, 15*time.Second),
		},
		decideCase{
			name: "(ii) in a call",
			dev:  deskWith(mem()),
			obs:  registeredObs(vOld, 1),
			want: stale(mem(), "waiting for the call to end before a resync", "", cfgStale("; waiting for the call to end"), nil, 30*time.Second),
		},
		decideCase{
			name: "(ii) in a call, a request stays pending",
			dev:  deskWith(mem(), withRequest(req1)),
			obs:  registeredObs(vOld, 1),
			want: stale(mem(), "waiting for the call to end before a resync", "", cfgStale("; waiting for the call to end"), nil, 30*time.Second),
		},
		decideCase{
			name: "(iii) a request: resync, not counted",
			dev:  deskWith(mem(), withRequest(req1)),
			obs:  registeredObs(vOld, 0),
			want: stale(mem(handled(req1), action(resyncAction(vNow, t0, 0))), "resync sent on request", "",
				cfgStale(""), resyncNow(), 5*time.Minute),
		},
		decideCase{
			name: "(iii) a request inside the rate limit: attempts carried",
			dev:  deskWith(mem(action(resyncAction(vNow, ago(time.Minute), 1))), withRequest(req1)),
			obs:  registeredObs(vOld, 0),
			want: stale(mem(handled(req1), action(resyncAction(vNow, t0, 1))), "resync sent on request", "",
				cfgStale("; resyncs sent: 1"), resyncNow(), 5*time.Minute),
		},
		decideCase{
			name: "(iii) a request already handled is no request",
			dev:  deskWith(mem(handled(req1)), withRequest(req1)),
			obs:  registeredObs(vOld, 0),
			want: stale(mem(handled(req1), action(resyncAction(vNow, t0, 1))), "resync sent (attempt 1)", "",
				cfgStale("; resyncs sent: 1"), resyncNow(), 5*time.Minute),
		},
		decideCase{
			name: "(iv) the phone does not resync by NOTIFY",
			drv:  cisco79x5.New().WithTweak(func(p *drivers.Profile) { p.Provisioning.ResyncByNotify = false }),
			dev:  deskWith(mem()),
			obs:  registeredObs(vOld, 0),
			want: stale(mem(), p13, p13, cfgStale(""), nil, 0),
		},
	)
	for _, c := range []struct {
		name     string
		last     *v1alpha1.LastAction
		readyMsg string
		human    string
		cfg      metav1.Condition
		next     *v1alpha1.LastAction // nil: unchanged
		requeue  time.Duration
	}{
		{"(v) attempt 1, 5m-1s ago: wait", resyncAction(vNow, ago(5*time.Minute-time.Second), 1), waitMsg, "",
			cfgStale("; resyncs sent: 1"), nil, time.Second},
		{"(v) attempt 1, 5m ago: attempt 2", resyncAction(vNow, ago(5*time.Minute), 1), "resync sent (attempt 2)", "",
			cfgStale("; resyncs sent: 2"), resyncAction(vNow, t0, 2), 5 * time.Minute},
		{"(v) attempt 2, 5m-1s ago: wait", resyncAction(vNow, ago(5*time.Minute-time.Second), 2), waitMsg, "",
			cfgStale("; resyncs sent: 2"), nil, time.Second},
		{"(v) attempt 2, 5m ago: a human", resyncAction(vNow, ago(5*time.Minute), 2), p13, p13,
			cfgStale("; resyncs sent: 2"), nil, 0},
		{"(v) attempt 2, an hour ago: still a human", resyncAction(vNow, ago(time.Hour), 2), p13, p13,
			cfgStale("; resyncs sent: 2"), nil, 0},
		{"(v) only a requested resync, 5m-1s ago: wait", resyncAction(vNow, ago(5*time.Minute-time.Second), 0), waitMsg, "",
			cfgStale(""), nil, time.Second},
		{"(v) only a requested resync, 5m ago: attempt 1", resyncAction(vNow, ago(5*time.Minute), 0), "resync sent (attempt 1)", "",
			cfgStale("; resyncs sent: 1"), resyncAction(vNow, t0, 1), 5 * time.Minute},
		{"(v) an older version's resync: a fresh attempt 1", resyncAction(vOld, ago(time.Minute), 2), "resync sent (attempt 1)", "",
			cfgStale("; resyncs sent: 1"), resyncAction(vNow, t0, 1), 5 * time.Minute},
		{"(vi) first resync", nil, "resync sent (attempt 1)", "",
			cfgStale("; resyncs sent: 1"), resyncAction(vNow, t0, 1), 5 * time.Minute},
	} {
		next := c.last
		var actions []drivers.Action
		if c.next != nil {
			next, actions = c.next, resyncNow()
		}
		cases = append(cases, decideCase{
			name: c.name,
			dev:  deskWith(mem(action(c.last))),
			obs:  registeredObs(vOld, 0),
			want: stale(mem(action(next)), c.readyMsg, c.human, c.cfg, actions, c.requeue),
		})
	}

	// Config current, but running another SIP load with the target staged:
	// a resync makes it re-read loadInformation and upgrade.
	other := registeredMem(cfgFetch(vNow, ago(30*time.Minute)))
	other.Firmware = "jar45sip.9-4-2ES26.sbn"
	next := other
	next.LastAction = resyncAction(vNow, t0, 1)
	otherObs := registeredObs(vNow, 0, withStore(staged()))
	otherObs.Device.Web = web("", "jar45sip.9-4-2ES26.sbn")
	cases = append(cases, decideCase{
		name: "config current, another load, target staged",
		drv:  pinned(),
		dev:  deskWith(other),
		obs:  otherObs,
		want: drivers.Decision{
			State: drivers.StateConfigStale,
			Conditions: conds(ready(cF, cisco79x5.ReasonConfigStale, "resync sent (attempt 1)"), faStaged,
				fcOther("jar45sip.9-4-2ES26.sbn"), provNow, regLive, cfgNow),
			Actions:      resyncNow(),
			StatusPatch:  next,
			RequeueAfter: 5 * time.Minute,
		},
	})
	return cases
}

func registeredCases() []decideCase {
	cur := cfgFetch(vNow, ago(30*time.Minute))
	registered := func(m drivers.Memory, fc, cfg metav1.Condition, actions []drivers.Action, label ...metav1.Condition) drivers.Decision {
		return drivers.Decision{
			State:        drivers.StateRegistered,
			Conditions:   conds(ready(cT, cisco79x5.ReasonRegistered, ""), faPending, fc, provNow, regLive, cfg, label...),
			Actions:      actions,
			StatusPatch:  m,
			RequeueAfter: 10 * time.Minute,
		}
	}
	plain := registeredMem(cur)
	notReported := registeredMem(cur)
	notReported.Firmware = ""
	notReportedObs := registeredObs(vNow, 0)
	notReportedObs.Device.Web = nil
	requested := registeredMem(cur)
	requested.HandledRequest = req1
	requested.LastAction = resyncAction(vNow, t0, 0)
	jarOnly := registeredMem(cur)
	jarOnly.Firmware = jarSIP
	jarOnlyObs := registeredObs(vNow, 0)
	jarOnlyObs.Device.Web = web("", jarSIP)
	advanced := registeredMem(cur)
	advanced.LastRegistered = mt(ago(time.Minute))
	advancedWant := registeredMem(cur)
	advancedWant.LastRegistered = mt(t0)
	upgradeEnded := registeredMem(cur)
	upgradeEnded.UpgradeStartedAt = mt(ago(40 * time.Minute))
	return []decideCase{
		{name: "plain", dev: deskWith(plain), obs: registeredObs(vNow, 0), want: registered(plain, fcRunning, cfgNow, nil)},
		{name: "firmware not reported", dev: deskWith(notReported), obs: notReportedObs, want: registered(notReported, fcNotReported, cfgNow, nil)},
		{
			name: "label shortened",
			dev:  deskWith(plain, func(d *drivers.Device) { d.Label = "Front Door" }),
			obs:  registeredObs(vNow, 0),
			want: registered(plain, fcRunning, cfgNow, nil,
				cond(v1alpha1.ConditionLabelShortened, cT, cisco79x5.ReasonShortened, `shows "FrontDoor" on screen and "Front Door" on its line`)),
		},
		{
			name: "a request: resync",
			dev:  deskWith(plain, withRequest(req1)),
			obs:  registeredObs(vNow, 0),
			want: registered(requested, fcRunning, cfgNow, resyncNow()),
		},
		{
			name: "a request during a call stays pending",
			dev:  deskWith(plain, withRequest(req1)),
			obs:  registeredObs(vNow, 1),
			want: registered(plain, fcRunning, cfgNow, nil),
		},
		{
			name: "a request while the new bundle is not live stays pending",
			dev:  deskWith(plain, withRequest(req1)),
			obs: registeredObs(vNow, 0, func(o *drivers.Observations) {
				o.Agent.AppliedVersion, o.Agent.LastApplyVersion = "bundle-6", "bundle-6"
			}),
			want: registered(plain, fcRunning, cfgPending, nil),
		},
		{
			name: "upgraded, reporting only appLoadID",
			dev:  deskWith(registeredMem(cur)),
			obs:  jarOnlyObs,
			want: registered(jarOnly, fcRunning, cfgNow, nil),
		},
		{name: "last registered advances after a minute", dev: deskWith(advanced), obs: registeredObs(vNow, 0), want: registered(advancedWant, fcRunning, cfgNow, nil)},
		{name: "clears an ended upgrade", dev: deskWith(upgradeEnded), obs: registeredObs(vNow, 0), want: registered(plain, fcRunning, cfgNow, nil)},
	}
}

func unreachableCases() []decideCase {
	mem := func(lastRegistered time.Time) drivers.Memory {
		m := registeredMem(cfgFetch(vNow, ago(30*time.Minute)))
		m.LastRegistered = mt(lastRegistered)
		return m
	}
	gone := obsWith(&agentapi.DeviceObservations{
		Fetches: []agentapi.Fetch{sepFetch(ago(30*time.Minute), vNow)},
		Web:     web(sip45, ""),
	})
	unreachable := func(m drivers.Memory, readyMsg, human string, reg metav1.Condition, requeue time.Duration) drivers.Decision {
		return drivers.Decision{
			State:        drivers.StateUnreachable,
			Conditions:   conds(ready(cF, cisco79x5.ReasonUnreachable, readyMsg), faPending, fcRunning, provNow, reg, cfgNow),
			StatusPatch:  m,
			HumanAction:  human,
			RequeueAfter: requeue,
		}
	}
	grace := "its registration lapsed; waiting out the 10 min grace"
	expired := obsWith(&agentapi.DeviceObservations{
		Fetches:      []agentapi.Fetch{sepFetch(ago(30*time.Minute), vNow)},
		Registration: contact(ago(time.Second)),
		Web:          web(sip45, ""),
	})
	qualify := registeredObs(vNow, 0)
	qualify.Device.Registration.Qualify = agentapi.QualifyUnreachable
	qualify.Device.Web = web(sip45, "")
	withReq := mem(ago(11 * time.Minute))
	withReq.HandledRequest = req1
	return []decideCase{
		{name: "registered 10m-1s ago", dev: deskWith(mem(ago(10*time.Minute - time.Second))), obs: gone,
			want: unreachable(mem(ago(10*time.Minute-time.Second)), grace, "", regNone, time.Second)},
		{name: "registered 10m ago", dev: deskWith(mem(ago(10 * time.Minute))), obs: gone,
			want: unreachable(mem(ago(10*time.Minute)), p14, p14, regNone, 0)},
		{name: "registered 10m+1s ago", dev: deskWith(mem(ago(10*time.Minute + time.Second))), obs: gone,
			want: unreachable(mem(ago(10*time.Minute+time.Second)), p14, p14, regNone, 0)},
		{name: "an expired contact", dev: deskWith(mem(ago(11 * time.Minute))), obs: expired,
			want: unreachable(mem(ago(11*time.Minute)), p14, p14, regNone, 0)},
		{
			name: "contact lost via qualify",
			drv:  cisco79x5.New().WithTweak(func(p *drivers.Profile) { p.Endpoint.QualifyFrequency = time.Minute }),
			dev:  deskWith(mem(ago(11 * time.Minute))), obs: qualify,
			want: unreachable(mem(ago(11*time.Minute)), p14, p14, regQualify, 0),
		},
		{name: "a request is dropped", dev: deskWith(mem(ago(11*time.Minute)), withRequest(req1)), obs: gone,
			want: unreachable(withReq, p14, p14, regNone, 0)},
	}
}

// diffDecision describes how got differs from want, field by field.
func diffDecision(got, want drivers.Decision) string {
	var b strings.Builder
	if got.State != want.State {
		fmt.Fprintf(&b, "State: got %s, want %s\n", got.State, want.State)
	}
	if !reflect.DeepEqual(got.Conditions, want.Conditions) {
		for i := range max(len(got.Conditions), len(want.Conditions)) {
			var g, w metav1.Condition
			if i < len(got.Conditions) {
				g = got.Conditions[i]
			}
			if i < len(want.Conditions) {
				w = want.Conditions[i]
			}
			if g != w {
				fmt.Fprintf(&b, "Conditions[%d]:\n  got  %s %s %s %q\n  want %s %s %s %q\n", i,
					g.Type, g.Status, g.Reason, g.Message, w.Type, w.Status, w.Reason, w.Message)
			}
		}
	}
	if !reflect.DeepEqual(got.Actions, want.Actions) {
		fmt.Fprintf(&b, "Actions: got %v, want %v\n", got.Actions, want.Actions)
	}
	if !reflect.DeepEqual(got.StatusPatch, want.StatusPatch) {
		fmt.Fprintf(&b, "StatusPatch:\n  got  %s\n  want %s\n", fmtMemory(got.StatusPatch), fmtMemory(want.StatusPatch))
	}
	if got.HumanAction != want.HumanAction {
		fmt.Fprintf(&b, "HumanAction:\n  got  %q\n  want %q\n", got.HumanAction, want.HumanAction)
	}
	if got.RequeueAfter != want.RequeueAfter {
		fmt.Fprintf(&b, "RequeueAfter: got %s, want %s\n", got.RequeueAfter, want.RequeueAfter)
	}
	return b.String()
}

// fmtMemory prints memory with times relative to t0.
func fmtMemory(m drivers.Memory) string {
	rel := func(t *metav1.Time) string {
		if t == nil {
			return "nil"
		}
		return "t0" + t.Sub(t0).String()
	}
	var parts []string
	parts = append(parts, "firstSeen="+rel(m.FirstSeen), "firmware="+m.Firmware, "address="+m.Address)
	if f := m.LastConfigFetch; f != nil {
		parts = append(parts, fmt.Sprintf("lastConfigFetch={%.12s %s}", f.Version, rel(&f.At)))
	} else {
		parts = append(parts, "lastConfigFetch=nil")
	}
	parts = append(parts, "lastRegistered="+rel(m.LastRegistered), "upgradeStartedAt="+rel(m.UpgradeStartedAt))
	if a := m.LastAction; a != nil {
		parts = append(parts, fmt.Sprintf("lastAction={%s %.12s %s %d}", a.Kind, a.Version, rel(&a.At), a.Attempts))
	} else {
		parts = append(parts, "lastAction=nil")
	}
	parts = append(parts, "handledRequest="+m.HandledRequest)
	return strings.Join(parts, " ")
}
