//go:build e2e

package devices_test

import (
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// e2eLoad is the cisco79x5 load only e2e builds carry: the one load whose
// digests are genuine, so it can be staged.
func e2eLoad(t *testing.T) drivers.FirmwareLoad {
	t.Helper()
	for _, l := range cisco79x5.New().Firmware() {
		if l.Name != cisco79x5.DefaultLoad && len(l.PendingFiles()) == 0 {
			return l
		}
	}
	t.Fatal("no stageable load in the cisco79x5 manifest of this build")
	return drivers.FirmwareLoad{}
}

// TestPhoneLifecycleFirmware walks a phone on SCCP firmware to Registered
// through the reconciler and a real API server (plan §9 "Desk walks to
// Registered"): Unseen, NeedsFirmware until its load is staged, Upgrading
// (also once the download window has passed, from the stored
// upgradeStartedAt), Registering on its first config fetch after the
// upgrade, Registered with every condition True. Only the e2e build's
// manifest has a load that can be staged, so `make test` (envtest untagged)
// does not run it: `go test -tags e2e ./internal/controller/devices/` does.
func TestPhoneLifecycleFirmware(t *testing.T) {
	load := e2eLoad(t)
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk", func(p *v1alpha1.Phone) { p.Spec.Firmware = load.Name })
	f.compile()
	dv := f.version(deskID)
	at := t0.Add
	ring := agentapi.DeviceObservations{Device: deskID, MAC: deskMAC}

	walk(t, f, step{name: "created", at: 0, state: drivers.StateUnseen, human: "connect it to a VLAN 20 switch port"})

	// SCCP firmware: it fetches its config, its web page says SCCP, and it
	// asks for its target load, which is not staged.
	ring.Fetches = []agentapi.Fetch{
		configFetch(dv, at(5*time.Second)),
		notFound(load.Name+".loads", at(7*time.Second)),
	}
	ring.Web = &agentapi.WebScrape{At: at(6 * time.Second), Address: deskIP,
		DeviceInformation:    &agentapi.DeviceInformation{VersionID: "SCCP45.9-4-2SR4-3S", AppLoadID: "jar45sccp.9-4-2SR4-3.sbn"},
		NetworkConfiguration: &agentapi.NetworkConfiguration{}}
	walk(t, f, step{name: "needs its load", at: 10 * time.Second, state: drivers.StateNeedsFirmware,
		human: "run `patchbay firmware load <zip>`",
		conditions: map[string][2]string{
			v1alpha1.ConditionFirmwareAvailable: {"False", cisco79x5.ReasonNotStaged},
			v1alpha1.ConditionFirmwareCurrent:   {"False", cisco79x5.ReasonOtherLoad},
		}}, ring)

	// The load is staged; the phone downloads it.
	for _, file := range load.Files {
		f.store.Files = append(f.store.Files, agentapi.FirmwareFileStatus{Load: load.Name, Name: file.Name, Present: true})
	}
	for i, file := range load.Files {
		ring.Fetches = append(ring.Fetches, served(file.Name, at(40*time.Second+time.Duration(i)*time.Second)))
	}
	st := walk(t, f, step{name: "downloading", at: 45 * time.Second, state: drivers.StateUpgrading,
		human:      "upgrading: do not unplug",
		conditions: map[string][2]string{v1alpha1.ConditionFirmwareAvailable: {"True", cisco79x5.ReasonStaged}}}, ring)
	if st.Observed == nil || st.Observed.UpgradeStartedAt == nil {
		t.Fatalf("observed %+v; want upgradeStartedAt", st.Observed)
	}
	walk(t, f, step{name: "flashing", at: 4 * time.Minute, state: drivers.StateUpgrading, human: "upgrading: do not unplug"}, ring)

	// It boots the new load and fetches its config; its web page answers.
	ring.Fetches = append(ring.Fetches, configFetch(dv, at(4*time.Minute+5*time.Second)))
	ring.Web = &agentapi.WebScrape{At: at(4*time.Minute + 10*time.Second), Address: deskIP,
		DeviceInformation:    &agentapi.DeviceInformation{VersionID: load.Name},
		NetworkConfiguration: &agentapi.NetworkConfiguration{}}
	walk(t, f, step{name: "booted", at: 4*time.Minute + 15*time.Second, state: drivers.StateRegistering,
		conditions: map[string][2]string{
			v1alpha1.ConditionProvisioned:     {"True", cisco79x5.ReasonFetched},
			v1alpha1.ConditionFirmwareCurrent: {"True", cisco79x5.ReasonRunning},
		}}, ring)

	ring.Registration = contact("100", deskIP, at(14*time.Minute))
	walk(t, f, step{name: "registered", at: 4*time.Minute + 20*time.Second, state: drivers.StateRegistered,
		conditions: map[string][2]string{
			v1alpha1.ConditionReady:             {"True", cisco79x5.ReasonRegistered},
			v1alpha1.ConditionFirmwareAvailable: {"True", cisco79x5.ReasonStaged},
			v1alpha1.ConditionFirmwareCurrent:   {"True", cisco79x5.ReasonRunning},
			v1alpha1.ConditionProvisioned:       {"True", cisco79x5.ReasonFetched},
			v1alpha1.ConditionRegistered:        {"True", cisco79x5.ReasonContact},
			v1alpha1.ConditionConfigCurrent:     {"True", cisco79x5.ReasonCurrent},
		}}, ring)
	if calls := f.actor.take(); len(calls) != 0 {
		t.Errorf("the agent was asked for %v; want nothing", calls)
	}
}
