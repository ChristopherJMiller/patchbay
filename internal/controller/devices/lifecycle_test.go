package devices_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// step is one poll of a lifecycle: the time, then what the Phone's status
// must say after its reconcile.
type step struct {
	name  string
	at    time.Duration
	state drivers.State
	// human is a prefix of status.humanAction ("" means it must be empty).
	human string
	// conditions are checked by status and reason.
	conditions map[string][2]string
}

// walk reconciles Phone desk once at the step's time and checks its status.
// It returns the status. Every condition must carry the generation.
func walk(t *testing.T, f *fixture, s step, dev ...agentapi.DeviceObservations) v1alpha1.PhoneStatus {
	t.Helper()
	f.at(s.at)
	f.see(dev...)
	f.reconcilePhone("desk")
	p := f.phone("desk")
	st := p.Status
	if drivers.State(st.State) != s.state {
		t.Fatalf("%s: state %s (Ready %v, human %q); want %s", s.name, st.State,
			condition(t, st.Conditions, v1alpha1.ConditionReady), st.HumanAction, s.state)
	}
	switch {
	case s.human == "" && st.HumanAction != "":
		t.Errorf("%s: human action %q; want none", s.name, st.HumanAction)
	case s.human != "" && !strings.HasPrefix(st.HumanAction, s.human):
		t.Errorf("%s: human action %q; want %q...", s.name, st.HumanAction, s.human)
	}
	for typ, want := range s.conditions {
		c := condition(t, st.Conditions, typ)
		if string(c.Status) != want[0] || c.Reason != want[1] {
			t.Errorf("%s: %s is %s %s (%q); want %s %s", s.name, typ, c.Status, c.Reason, c.Message, want[0], want[1])
		}
	}
	for _, c := range st.Conditions {
		if c.ObservedGeneration != p.Generation {
			t.Errorf("%s: %s has observedGeneration %d; want %d", s.name, c.Type, c.ObservedGeneration, p.Generation)
		}
	}
	if st.ObservedGeneration != p.Generation {
		t.Errorf("%s: observedGeneration %d; want %d", s.name, st.ObservedGeneration, p.Generation)
	}
	return st
}

// TestPhoneLifecycleDeskReset walks the Desk phone's own path (design §2,
// plan §9 "Desk walks to Registered", the row added in the W1 review)
// through the reconciler and a real API server: status is stored between
// polls, so everything the state machine remembers must survive the round
// trip. The phone boots with its old CUCM's trust list and asks only for
// its signed config; the default load is not staged (its digests are
// pending in this build), so the human action is P6, not P5 (6.2.2). The
// factory reset downloads the default load while its web page is down;
// from then on it is never NeedsTrustReset again, even when the agent
// restarts mid-upgrade and its fetch log empties; then it fetches its config
// and registers, with FirmwareCurrent Unknown and no resync.
//
// The firmware fetches are scripted: the controller hands the fetch log to
// the driver as the agent reports it, served or not.
func TestPhoneLifecycleDeskReset(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	dv := f.version(deskID)
	sep := cisco79x5.ConfigFileName(deskMAC)
	ring := agentapi.DeviceObservations{Device: deskID, MAC: deskMAC}
	at := t0.Add

	walk(t, f, step{name: "created", at: 0, state: drivers.StateUnseen, human: "connect it to a VLAN 20 switch port",
		conditions: map[string][2]string{
			v1alpha1.ConditionReady:             {"False", cisco79x5.ReasonUnseen},
			v1alpha1.ConditionFirmwareCurrent:   {"Unknown", cisco79x5.ReasonNotReported},
			v1alpha1.ConditionFirmwareAvailable: {"False", cisco79x5.ReasonManifestPending},
		}})

	// It boots with its old trust list: CTL, ITL, then only the signed SEP.
	ring.Fetches = []agentapi.Fetch{
		notFound("CTLSEP"+deskMAC+".tlv", at(5*time.Second)),
		notFound("ITLSEP"+deskMAC+".tlv", at(5*time.Second+100*time.Millisecond)),
		notFound(sep+".sgn", at(5*time.Second+200*time.Millisecond)),
	}
	st := walk(t, f, step{name: "asked for the signed config", at: 10 * time.Second, state: drivers.StateRegistering}, ring)
	if st.Observed == nil || st.Observed.FirstSeen == nil || !st.Observed.FirstSeen.Equal(&metav1.Time{Time: at(5 * time.Second)}) {
		t.Errorf("firstSeen %v; want %v", st.Observed, at(5*time.Second))
	}

	walk(t, f, step{name: "still only the signed config, 3 min on", at: 3*time.Minute + 10*time.Second,
		state: drivers.StateNeedsTrustReset, human: "stage firmware first",
		conditions: map[string][2]string{v1alpha1.ConditionReady: {"False", cisco79x5.ReasonNeedsTrustReset}}}, ring)

	// The factory reset: the default load downloads while its web page is
	// down.
	ring.Fetches = append(ring.Fetches,
		served("term45.default.loads", at(3*time.Minute+30*time.Second)),
		served("apps45.9-4-2SR4-3.sbn", at(3*time.Minute+50*time.Second)))
	ring.Web = &agentapi.WebScrape{At: at(3*time.Minute + 55*time.Second), Address: deskIP, Error: "connection refused"}
	st = walk(t, f, step{name: "downloading the default load", at: 4 * time.Minute,
		state: drivers.StateUpgrading, human: "upgrading: do not unplug"}, ring)
	start := metav1.NewTime(at(3*time.Minute + 30*time.Second))
	if st.Observed == nil || st.Observed.UpgradeStartedAt == nil || !st.Observed.UpgradeStartedAt.Equal(&start) {
		t.Fatalf("upgradeStartedAt %v; want %v", st.Observed, start)
	}

	// Flashing: no fetch for 3 min. Only the stored upgradeStartedAt keeps
	// it Upgrading; the old .sgn requests are before the download.
	walk(t, f, step{name: "flashing", at: 7 * time.Minute, state: drivers.StateUpgrading, human: "upgrading: do not unplug"}, ring)

	// The agent restarts: its fetch log is empty. Still Upgrading, from
	// status alone.
	f.agent.StartedAt = at(7*time.Minute + 30*time.Second)
	ring = agentapi.DeviceObservations{Device: deskID, MAC: deskMAC}
	walk(t, f, step{name: "flashing, the agent restarted", at: 8 * time.Minute, state: drivers.StateUpgrading,
		human: "upgrading: do not unplug"}, ring)

	// It boots SIP and fetches its config.
	ring.Fetches = []agentapi.Fetch{configFetch(dv, at(8*time.Minute+20*time.Second))}
	st = walk(t, f, step{name: "fetched its config", at: 8*time.Minute + 30*time.Second, state: drivers.StateRegistering,
		conditions: map[string][2]string{v1alpha1.ConditionProvisioned: {"True", cisco79x5.ReasonFetched}}}, ring)
	if st.Observed.UpgradeStartedAt != nil {
		t.Errorf("upgradeStartedAt %v after the upgrade ended; want none", st.Observed.UpgradeStartedAt)
	}

	// It registers.
	ring.Registration = contact("100", deskIP, at(18*time.Minute))
	st = walk(t, f, step{name: "registered", at: 8*time.Minute + 40*time.Second, state: drivers.StateRegistered,
		conditions: map[string][2]string{
			v1alpha1.ConditionReady:           {"True", cisco79x5.ReasonRegistered},
			v1alpha1.ConditionRegistered:      {"True", cisco79x5.ReasonContact},
			v1alpha1.ConditionConfigCurrent:   {"True", cisco79x5.ReasonCurrent},
			v1alpha1.ConditionFirmwareCurrent: {"Unknown", cisco79x5.ReasonNotReported},
		}}, ring)
	if st.LastAction != nil {
		t.Errorf("lastAction %+v; want none (no resync before a successful scrape)", st.LastAction)
	}
	if calls := f.actor.take(); len(calls) != 0 {
		t.Errorf("the agent was asked for %v; want nothing", calls)
	}
	for _, e := range f.rec.take() {
		if e.Reason == "StateChanged" && strings.Contains(e.Note, "-> NeedsTrustReset") && !strings.HasPrefix(e.Note, "Registering") {
			t.Errorf("NeedsTrustReset again: %s", e)
		}
	}
}

// TestPhoneNeedsFirmware: a phone that asks for a file of its load that is
// not staged needs firmware; with the default load's digests pending, the
// human action says to fill them in (P4).
func TestPhoneNeedsFirmware(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	ring := agentapi.DeviceObservations{Device: deskID, MAC: deskMAC, Fetches: []agentapi.Fetch{
		notFound(cisco79x5.DefaultLoad+".loads", t0.Add(5*time.Second)),
	}}
	walk(t, f, step{name: "asked for its load", at: 10 * time.Second, state: drivers.StateNeedsFirmware,
		human: "the cisco79x5 firmware manifest has no SHA-256 yet",
		conditions: map[string][2]string{
			v1alpha1.ConditionReady:             {"False", cisco79x5.ReasonNeedsFirmware},
			v1alpha1.ConditionFirmwareAvailable: {"False", cisco79x5.ReasonManifestPending},
		}}, ring)
}
