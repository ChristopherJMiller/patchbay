package devices_test

import (
	"strings"
	"testing"
	"time"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

// hostile is text a VLAN-20 host controls (its web page, its SIP user
// agent) or an agent error carries: far over the CRD's 32768-byte
// condition message, with a terminal escape and a carriage return.
func hostile(prefix string) string {
	return prefix + strings.Repeat("A", 40000) + "\x1b[2J\rspoofed\u009b"
}

// boundedText fails unless s fits limit and holds no control character.
func boundedText(t *testing.T, what, s string, limit int) {
	t.Helper()
	if len(s) > limit || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		t.Errorf("%s: %d bytes (limit %d), control character %v", what, len(s), limit, strings.IndexFunc(s, unicode.IsControl) >= 0)
	}
}

// boundedStatus checks every condition message and the human action.
func boundedStatus(t *testing.T, conds []metav1.Condition, human string) {
	t.Helper()
	for _, c := range conds {
		boundedText(t, c.Type+" message", c.Message, install.MaxStatusMessage)
	}
	boundedText(t, "humanAction", human, install.MaxStatusMessage)
}

// boundedNotes checks every Event note against events.k8s.io's limit.
func boundedNotes(t *testing.T, events []recorded) {
	t.Helper()
	if len(events) == 0 {
		t.Error("no Events")
	}
	for _, e := range events {
		boundedText(t, e.Reason+" note", e.Note, install.MaxEventNote)
	}
}

// TestHostileTextBounded: what a device sends reaches status bounded and
// without control characters, so the API server never refuses the status
// write (a Phone whose status could not be written would show a stale
// state for good) and `patchbay status` never prints an escape sequence.
// Its web page's load ID (observed.firmware, FirmwareCurrent's message),
// its log, its SIP user agent (Registered's message) and an agent's apply
// error (ConfigCurrent, the ApplyFailed note) are each 40 kB here.
func TestHostileTextBounded(t *testing.T) {
	f := newFixture(t)
	ring := registeredDesk(t, f)
	ring.Registration.UserAgent = hostile("Cisco-CP7945G/")
	ring.Web = &agentapi.WebScrape{At: t0.Add(20 * time.Second), Address: deskIP,
		DeviceInformation:    &agentapi.DeviceInformation{VersionID: hostile("SIP45.")},
		NetworkConfiguration: &agentapi.NetworkConfiguration{},
		DeviceLog:            []string{hostile("10:02:18a 7: "), hostile("10:02:19a 7: ")}}
	f.at(30 * time.Second)
	f.see(ring)
	f.reconcilePhone("desk")
	st := f.phone("desk").Status
	if st.Observed == nil || !strings.HasPrefix(st.Observed.Firmware, "SIP45.AAAA") {
		t.Fatalf("observed %+v; want the scraped load ID, cut", st.Observed)
	}
	boundedText(t, "observed.firmware", st.Observed.Firmware, install.MaxStatusValue)
	boundedStatus(t, st.Conditions, st.HumanAction)
	if c := condition(t, st.Conditions, v1alpha1.ConditionFirmwareCurrent); !strings.HasPrefix(c.Message, "running SIP45.AAAA") {
		t.Errorf("FirmwareCurrent %q; want the running load named", c.Message)
	}
	boundedNotes(t, f.rec.take())

	// The agent refuses the next version with a 40 kB error.
	v0 := f.bundle.Version
	relabel(t, f)
	f.agent.AppliedVersion, f.agent.LastApplyVersion, f.agent.LastApplyError = v0, f.bundle.Version, hostile("verify: ")
	f.at(time.Minute)
	f.see(ring)
	f.reconcilePhone("desk")
	st = f.phone("desk").Status
	wantCondition(t, st.Conditions, v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse, v1alpha1.ReasonApplyFailed, "")
	boundedStatus(t, st.Conditions, st.HumanAction)
	events := f.rec.take()
	boundedNotes(t, events)
	reasons := map[string]bool{}
	for _, e := range events {
		reasons[e.Reason] = true
	}
	if !reasons["ApplyFailed"] {
		t.Errorf("events %v; want ApplyFailed", events)
	}
}
