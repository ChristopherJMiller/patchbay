package cisco79x5_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// roundTrip stores a Decision's memory the way the controller does (into
// the Phone's status, through JSON, back to a Device), with every time in
// UTC so it compares with reflect.DeepEqual. Status keeps times in whole
// seconds, which is what this exercises.
func roundTrip(t *testing.T, dev drivers.Device, m drivers.Memory, state drivers.State) drivers.Device {
	t.Helper()
	p := phone(dev.Name, v1alpha1.PhoneModel(dev.Model), dev.MAC, dev.Number, dev.Label)
	p.Spec.Firmware = dev.Firmware
	if dev.Request != "" {
		p.Annotations = map[string]string{v1alpha1.AnnotationResyncRequested: dev.Request}
	}
	m.ToPhoneStatus(&p.Status)
	p.Status.State = string(state)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back v1alpha1.Phone
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	out := drivers.PhoneDevice(&back)
	out.ConfigVersion, out.NumberConflict, out.NumberOutOfPlan, out.MACConflict, out.ClaimMessage =
		dev.ConfigVersion, dev.NumberConflict, dev.NumberOutOfPlan, dev.MACConflict, dev.ClaimMessage
	out.Memory = utcMemory(out.Memory)
	return out
}

func utcMemory(m drivers.Memory) drivers.Memory {
	fix := func(t *metav1.Time) {
		if t != nil {
			t.Time = t.UTC()
		}
	}
	fix(m.FirstSeen)
	fix(m.LastRegistered)
	fix(m.UpgradeStartedAt)
	if m.LastConfigFetch != nil {
		fix(&m.LastConfigFetch.At)
	}
	if m.LastAction != nil {
		fix(&m.LastAction.At)
	}
	return m
}

// TestLifecycle walks the Desk phone the way design §2 found it: SCCP
// firmware with its old CUCM's trust list, so it asks only for the signed
// config. It needs a factory reset; the reset downloads the staged SIP load
// while its web page is down, then it fetches its config and registers.
// Status is stored through JSON between polls, and the agent's times are
// sub-second, as on a real PBX. No step may tell the person to reset it
// again, restart it, or resync it.
func TestLifecycle(t *testing.T) {
	drv := cisco79x5.New()
	dev := desk()
	var state drivers.State
	ring := &agentapi.DeviceObservations{Device: "phone-desk", MAC: deskMAC}
	at := t0.Add
	add := func(fs ...agentapi.Fetch) { ring.Fetches = append(ring.Fetches, fs...) }
	ms := time.Millisecond

	step := func(name string, now time.Duration, wantState drivers.State, wantHuman string, wantFirmware metav1.ConditionStatus) {
		t.Helper()
		obs := obsWith(ring, withStore(staged()))
		obs.PolledAt = at(now)
		got := drv.Decide(dev, obs, settings, at(now))
		if got.State != wantState || got.HumanAction != wantHuman || len(got.Actions) != 0 {
			t.Fatalf("%s: state %s, human %q, actions %v; want %s, %q, none\nmemory %s",
				name, got.State, got.HumanAction, got.Actions, wantState, wantHuman, fmtMemory(got.StatusPatch))
		}
		i := slices.IndexFunc(got.Conditions, func(c metav1.Condition) bool { return c.Type == v1alpha1.ConditionFirmwareCurrent })
		if i < 0 || got.Conditions[i].Status != wantFirmware {
			t.Fatalf("%s: conditions %v; want FirmwareCurrent %s", name, got.Conditions, wantFirmware)
		}
		state = got.State
		stored := roundTrip(t, dev, got.StatusPatch, state)
		// Deciding again on the stored memory changes nothing: status is not
		// rewritten on every poll, and nothing flips once stored.
		again := drv.Decide(stored, obs, settings, at(now))
		if again.State != got.State || fmtMemory(again.StatusPatch) != fmtMemory(stored.Memory) {
			t.Fatalf("%s: deciding on the stored memory gives %s %s; first %s %s",
				name, again.State, fmtMemory(again.StatusPatch), got.State, fmtMemory(stored.Memory))
		}
		dev = stored
	}

	// It boots with its old trust list: CTL, ITL, then only the signed SEP.
	add(fetch("CTLSEPD0C78915E8C8.tlv", agentapi.FetchNotFound, at(200*ms)),
		fetch("ITLSEPD0C78915E8C8.tlv", agentapi.FetchNotFound, at(300*ms)),
		fetch(deskSEP+".sgn", agentapi.FetchNotFound, at(400*ms)))
	step("asked for the signed config", 10*time.Second+500*ms, drivers.StateRegistering, "", metav1.ConditionUnknown)

	// Its web page: SCCP, and a trust error in its log.
	ring.Web = &agentapi.WebScrape{
		At: at(45*time.Second + 200*ms), Address: deskIP,
		DeviceInformation:    &agentapi.DeviceInformation{VersionID: sccp45, AppLoadID: "jar45sccp.9-4-2SR4-3.sbn"},
		NetworkConfiguration: &agentapi.NetworkConfiguration{},
		DeviceLog:            []string{"Trust List Update Failed"},
	}
	step("its log shows the trust error", time.Minute+500*ms, drivers.StateNeedsTrustReset, p5, metav1.ConditionFalse)
	add(fetch(deskSEP+".sgn", agentapi.FetchNotFound, at(3*time.Minute+700*ms)))
	step("still asking for the signed config", 4*time.Minute, drivers.StateNeedsTrustReset, p5, metav1.ConditionFalse)

	// The factory reset: the phone boots what term45.default.loads names.
	// Its web page is down while it flashes; the agent still holds the last
	// successful scrape (old load, old log).
	add(fwFetch("term45.default.loads", at(5*time.Minute+100*ms)),
		fwFetch("apps45.9-4-2SR4-3.sbn", at(5*time.Minute+40*time.Second+700*ms)))
	ring.Web.At, ring.Web.Error = at(5*time.Minute+50*time.Second+300*ms), "connection refused"
	step("downloading the default load", 6*time.Minute+500*ms, drivers.StateUpgrading, p7, metav1.ConditionFalse)
	add(fwFetch("jar45sip.9-4-2SR4-3.sbn", at(6*time.Minute+50*time.Second+200*ms)))
	step("still downloading", 8*time.Minute+500*ms, drivers.StateUpgrading, p7, metav1.ConditionFalse)
	step("flashing, no fetch for 3 min", 9*time.Minute+51*time.Second, drivers.StateUpgrading, p7, metav1.ConditionFalse)

	// It boots SIP, fetches its config and registers. The web page has not
	// been read since: the remembered SCCP load is from before the upgrade.
	add(sepFetch(at(10*time.Minute+30*time.Second+600*ms), vNow))
	ring.Registration = contact(at(20*time.Minute + 30*time.Second))
	step("registered after the upgrade", 10*time.Minute+30*time.Second+900*ms, drivers.StateRegistered, "", metav1.ConditionUnknown)
	step("a poll later", 11*time.Minute+10*time.Second+100*ms, drivers.StateRegistered, "", metav1.ConditionUnknown)

	// The web page answers again.
	ring.Web = &agentapi.WebScrape{
		At: at(14*time.Minute + 800*ms), Address: deskIP,
		DeviceInformation:    &agentapi.DeviceInformation{VersionID: sip45, AppLoadID: jarSIP},
		NetworkConfiguration: &agentapi.NetworkConfiguration{},
		DeviceLog:            []string{"registered"},
	}
	step("its web page reports the SIP load", 15*time.Minute+500*ms, drivers.StateRegistered, "", metav1.ConditionTrue)

	// Its registration lapses (it locked up): Unreachable, not Registering.
	ring.Registration = nil
	step("contact gone", 16*time.Minute, drivers.StateUnreachable, "", metav1.ConditionTrue)
	step("past the grace", 26*time.Minute, drivers.StateUnreachable, p14, metav1.ConditionTrue)
}

// TestRegisteredInTheFetchSecond: the phone fetches its config and the
// controller polls it registered within one second. Stored in status (whole
// seconds), the registration must still come after the fetch: when the
// contact lapses the phone is Unreachable, not Registering.
func TestRegisteredInTheFetchSecond(t *testing.T) {
	drv := cisco79x5.New()
	ring := &agentapi.DeviceObservations{
		Device: "phone-desk", MAC: deskMAC,
		Fetches:      []agentapi.Fetch{sepFetch(t0.Add(10*time.Second+200*time.Millisecond), vNow)},
		Registration: contact(t0.Add(10 * time.Minute)),
	}
	poll := func(dev drivers.Device, now time.Time) (drivers.Decision, drivers.Device) {
		obs := obsWith(ring)
		obs.PolledAt = now
		dec := drv.Decide(dev, obs, settings, now)
		return dec, roundTrip(t, dev, dec.StatusPatch, dec.State)
	}
	first, stored := poll(desk(), t0.Add(10*time.Second+500*time.Millisecond))
	if first.State != drivers.StateRegistered {
		t.Fatalf("poll 1: %s", first.State)
	}
	second, _ := poll(stored, t0.Add(40*time.Second+500*time.Millisecond))
	if second.State != drivers.StateRegistered || fmtMemory(second.StatusPatch) != fmtMemory(stored.Memory) {
		t.Errorf("poll 2: %s %s; stored %s", second.State, fmtMemory(second.StatusPatch), fmtMemory(stored.Memory))
	}
	ring.Registration = nil
	if gone, _ := poll(stored, t0.Add(40*time.Second+500*time.Millisecond)); gone.State != drivers.StateUnreachable {
		t.Errorf("contact gone: %s, want Unreachable", gone.State)
	}
}
