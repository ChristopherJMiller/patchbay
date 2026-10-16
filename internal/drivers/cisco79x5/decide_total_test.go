package cisco79x5_test

import (
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// ages are the offsets the grid draws times from: every limit of the rule
// table just before, at and after it, and some in between.
var ages = []time.Duration{
	0, time.Second, 59 * time.Second, time.Minute,
	3*time.Minute - time.Second, 3 * time.Minute, 3*time.Minute + time.Second,
	5*time.Minute - time.Second, 5 * time.Minute, 5*time.Minute + time.Second,
	10*time.Minute - time.Second, 10 * time.Minute, 10*time.Minute + time.Second,
	20*time.Minute - time.Second, 20 * time.Minute, 20*time.Minute + time.Second,
	25 * time.Minute, time.Hour,
}

// gen draws random but plausible Decide inputs.
type gen struct{ r *rand.Rand }

func (g gen) pct(p int) bool       { return g.r.IntN(100) < p }
func (g gen) at() time.Time        { return ago(ages[g.r.IntN(len(ages))]) }
func pick[T any](g gen, xs ...T) T { return xs[g.r.IntN(len(xs))] }

// seen is a time the agent reports: one of the ages, often between whole
// seconds. Memory, read back from status, is always whole seconds (at).
func (g gen) seen() time.Time {
	return g.at().Add(-pick(g, time.Duration(0), 0, 250*time.Millisecond, 999*time.Millisecond))
}

// now is when Decide runs: usually t0, sometimes between seconds.
func (g gen) now() time.Time          { return pick(g, t0, t0, t0.Add(500*time.Millisecond)) }
func (g gen) maybeTime() *metav1.Time { return g.timeIf(50) }
func (g gen) timeIf(p int) *metav1.Time {
	if g.pct(p) {
		return mt(g.at())
	}
	return nil
}
func (g gen) version() string { return pick(g, vNow, vOld) }
func (g gen) firmware() string {
	return pick(g, "", "", sip45, sccp45, jarSIP, "jar45sccp.9-4-2SR4-3.sbn", "jar45sip.9-4-2ES26.sbn")
}
func (g gen) request() (req, handled string) {
	return pick(g, "", "", req1), pick(g, "", "", req1, "older")
}

func (g gen) driver() *cisco79x5.Driver {
	switch g.r.IntN(5) {
	case 0:
		return pendingDriver()
	case 1:
		return cisco79x5.New().WithTweak(func(p *drivers.Profile) { p.Provisioning.ResyncByNotify = false })
	case 2:
		return cisco79x5.New().WithTweak(func(p *drivers.Profile) { p.Endpoint.QualifyFrequency = time.Minute })
	default:
		return cisco79x5.New()
	}
}

func (g gen) device() drivers.Device {
	d := desk()
	if g.pct(5) {
		d.Model = pick(g, "cisco-7965", "cisco-7975", "cisco-7940")
	}
	if g.pct(5) {
		d.Firmware = pick(g, cisco79x5.DefaultLoad, "SIP45.9-4-2SR3-1S")
	}
	d.NumberConflict, d.MACConflict, d.NumberOutOfPlan = g.pct(3), g.pct(3), g.pct(3)
	d.Label = pick(g, "Desk", "Front Door", `R&D; "Lab" <1> Küche`)
	d.LastState = pick(g, "", drivers.StateRegistered, drivers.StateUpgrading)
	var handled string
	d.Request, handled = g.request()
	d.Memory = drivers.Memory{
		FirstSeen:        g.maybeTime(),
		Firmware:         g.firmware(),
		Address:          pick(g, "", deskIP),
		LastRegistered:   g.maybeTime(),
		UpgradeStartedAt: g.timeIf(25),
		HandledRequest:   handled,
	}
	if g.pct(60) {
		d.Memory.LastConfigFetch = cfgFetch(g.version(), g.at())
	}
	if g.pct(40) {
		d.Memory.LastAction = resyncAction(g.version(), g.at(), int32(g.r.IntN(4)))
	}
	return d
}

func (g gen) observations() drivers.Observations {
	o := healthy()
	o.AgentReachable = !g.pct(3)
	if g.pct(3) {
		o.PolledAt = ago(time.Minute)
	}
	o.Agent.AsteriskUp = !g.pct(3)
	switch g.r.IntN(6) {
	case 0:
		o.Agent.AppliedVersion, o.Agent.LastApplyVersion = "bundle-6", "bundle-6"
	case 1:
		o.Agent.AppliedVersion, o.Agent.LastApplyVersion, o.Agent.LastApplyError = "bundle-6", bundle, "bad"
	}
	switch g.r.IntN(3) {
	case 0:
		o.Firmware = staged()
	case 1:
		o.Firmware = staged(ciscoFiles[:g.r.IntN(len(ciscoFiles))]...)
	}
	if g.pct(15) {
		return o
	}
	d := &agentapi.DeviceObservations{Device: "phone-desk", MAC: deskMAC, Calls: pick(g, 0, 0, 0, 1)}
	for range g.r.IntN(6) {
		var f agentapi.Fetch
		switch g.r.IntN(9) {
		case 0, 1:
			f = sepFetch(g.seen(), g.version())
		case 2:
			f = fetch(deskSEP, agentapi.FetchNotFound, g.seen())
		case 3:
			f = fetch(deskSEP+".sgn", agentapi.FetchNotFound, g.seen())
		case 4:
			f = fetch("CTLSEPD0C78915E8C8.tlv", agentapi.FetchNotFound, g.seen())
		case 5:
			f = fetch(pick(g, "SIP45.9-4-2SR4-3S.loads", "apps45.9-4-2SR4-3.sbn", "term45.default.loads"),
				pick(g, agentapi.FetchServed, agentapi.FetchNotFound), g.seen())
		case 6:
			f = fwFetch("jar45sccp.9-4-2SR4-3.sbn", g.seen())
		default:
			f = fwFetch(pick(g, "cnu45.9-4-2SR4-3.sbn", "dsp45.9-4-2SR4-3.sbn"), g.seen())
		}
		d.Fetches = append(d.Fetches, f)
	}
	switch g.r.IntN(4) {
	case 0:
		d.Registration = contact(t0.Add(pick(g, -time.Second, 0, time.Second, 5*time.Minute)))
	case 1:
		d.Registration = contact(t0.Add(5 * time.Minute))
		d.Registration.Qualify = pick(g, agentapi.QualifyReachable, agentapi.QualifyUnreachable)
	}
	switch g.r.IntN(6) {
	case 0:
		d.Web = web(g.firmware(), g.firmware(), pick(g, "registering", "Trust List Update Failed", "Load Auth Failed CTL"))
	case 1:
		d.Web = web(g.firmware(), "")
		d.Web.NetworkConfiguration.AdminVLAN = pick(g, "", "20")
	case 2:
		d.Web = &agentapi.WebScrape{At: t0, Error: "timeout"}
	case 3:
		// A failed scrape still carries the last success's fields.
		d.Web = web(g.firmware(), "", "Trust List Update Failed")
		d.Web.At, d.Web.Error = t0, "timeout"
	}
	if d.Web != nil && d.Web.Error == "" {
		d.Web.At = g.seen()
	}
	o.Device = d
	return o
}

// rowOf is the row (1 to 10) a Decision came from.
func rowOf(dec drivers.Decision, agentProblem bool) int {
	if dec.State == drivers.StateInvalid {
		return 1
	}
	if agentProblem {
		return 2
	}
	for i, s := range cisco79x5.RuleStates() {
		if s == dec.State {
			return i + 1
		}
	}
	return 0
}

// ceil is t rounded up to a whole second.
func ceil(t time.Time) time.Time {
	if f := t.Truncate(time.Second); f.Before(t) {
		return f.Add(time.Second)
	}
	return t
}

// TestDecideTotal: over a seeded grid of agent health, observations, memory
// and timing (20 000 inputs, the agent's times often between seconds),
// Decide never reaches the defensive branch, always lists Ready first in
// the fixed order, acts only in ConfigStale and Registered, never resyncs
// during a call or before the config is live, answers a request at most
// once, and is deterministic. Its memory is stored in status unchanged, and
// deciding again on the stored memory with the same observations changes
// nothing: status is not rewritten on every poll.
func TestDecideTotal(t *testing.T) {
	g := gen{rand.New(rand.NewPCG(2026, 1014))}
	order := []string{
		v1alpha1.ConditionReady, v1alpha1.ConditionFirmwareAvailable, v1alpha1.ConditionFirmwareCurrent,
		v1alpha1.ConditionProvisioned, v1alpha1.ConditionRegistered, v1alpha1.ConditionConfigCurrent,
		v1alpha1.ConditionNumberConflict, v1alpha1.ConditionMACConflict, v1alpha1.ConditionUnsupported,
		v1alpha1.ConditionLabelShortened,
	}
	reached := map[int]int{}
	for i := range 20000 {
		drv, dev, obs, now := g.driver(), g.device(), g.observations(), g.now()
		before := fmtMemory(dev.Memory)
		dec := drv.Decide(dev, obs, settings, now)
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("input %d (now t0%+v): "+format+"\ndevice %+v\nmemory %s\nobs %+v\ndevice obs %+v\ndecision %+v",
				append(append([]any{i, now.Sub(t0)}, args...), dev, before, obs, obs.Device, dec)...)
		}
		if fmtMemory(dev.Memory) != before {
			fail("Decide changed dev.Memory")
		}
		if again := drv.Decide(dev, obs, settings, now); !reflect.DeepEqual(again, dec) {
			fail("Decide is not deterministic")
		}
		if len(dec.Conditions) == 0 || dec.Conditions[0].Type != v1alpha1.ConditionReady {
			fail("Ready is not the first condition")
		}
		if dec.Conditions[0].Reason == cisco79x5.ReasonNoRuleMatched {
			fail("reached the defensive branch")
		}
		problem := obs.AgentProblem(now, settings.StaleAfter) != ""
		row := rowOf(dec, problem)
		if rows := drv.MatchingRows(dev, obs, settings, now); row == 0 || len(rows) == 0 || rows[0] != row {
			fail("decided by row %d, but the first matching row is %v", row, rows)
		}
		reached[row]++
		var types []string
		for _, c := range dec.Conditions {
			types = append(types, c.Type)
			if c.Reason == "" {
				fail("condition %s has no reason", c.Type)
			}
		}
		switch {
		case row == 2 && !reflect.DeepEqual(types, []string{v1alpha1.ConditionReady, v1alpha1.ConditionRegistered}):
			fail("Unknown lists %v", types)
		case row != 2 && !reflect.DeepEqual(types, order):
			fail("conditions %v, want %v", types, order)
		}
		if row != 8 && row != 9 && len(dec.Actions) > 0 {
			fail("row %d returned actions %v", row, dec.Actions)
		}
		for _, a := range dec.Actions {
			if a.Kind != v1alpha1.ActionResync || a.Version != dev.ConfigVersion {
				fail("action %+v", a)
			}
			if obs.Device == nil || obs.Device.Calls > 0 || !obs.ConfigLive() {
				fail("resync during a call or before the config is live")
			}
			if la := dec.StatusPatch.LastAction; la == nil || !la.At.Equal(mt(ceil(now))) || la.Version != a.Version {
				fail("the resync is not recorded in lastAction: %+v", la)
			}
		}
		if h := dec.StatusPatch.HandledRequest; h != dev.Memory.HandledRequest {
			if row == 2 || h != dev.Request || dev.Request == dev.Memory.HandledRequest {
				fail("handledRequest %q from %q (request %q)", h, dev.Memory.HandledRequest, dev.Request)
			}
		}
		if row == 2 && !reflect.DeepEqual(dec.StatusPatch, dev.Memory) {
			fail("Unknown changed memory")
		}
		if dec.StatusPatch.UpgradeStartedAt != nil && row != 1 && row != 2 && row != 6 {
			fail("row %d kept upgradeStartedAt", row)
		}
		if row == 6 && dec.StatusPatch.UpgradeStartedAt == nil {
			fail("Upgrading without upgradeStartedAt")
		}
		if dec.RequeueAfter < 0 {
			fail("negative requeue")
		}

		// Stored in status, the memory is unchanged; decided again on it,
		// nothing changes.
		stored := roundTrip(t, dev, dec.StatusPatch, dec.State)
		if got := fmtMemory(stored.Memory); got != fmtMemory(dec.StatusPatch) {
			fail("storing the memory changed it: %s", got)
		}
		if again := drv.Decide(stored, obs, settings, now); !reflect.DeepEqual(again.StatusPatch, stored.Memory) {
			fail("deciding on the stored memory changed it: %s", fmtMemory(again.StatusPatch))
		}
	}
	for row := 1; row <= 10; row++ {
		if reached[row] == 0 {
			t.Errorf("the grid never reaches row %d", row)
		}
	}
	t.Logf("rows reached: %v", reached)
}
