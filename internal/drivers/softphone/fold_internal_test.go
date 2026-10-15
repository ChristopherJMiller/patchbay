package softphone

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

const (
	testID = "softphone-chris-laptop"
	vOld   = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	vNew   = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

func mt(d time.Duration) *metav1.Time {
	t := metav1.NewTime(t0.Add(d))
	return &t
}

func served(d time.Duration, version string) agentapi.Fetch {
	return agentapi.Fetch{File: ProvisioningFile(testID), Transport: agentapi.TransportHTTPDevice,
		Result: agentapi.FetchServed, At: t0.Add(d), Source: "192.168.0.50", Version: version}
}

func contact(expires time.Duration) *agentapi.Registration {
	return &agentapi.Registration{Contact: "sip:150@192.168.0.51:5060", Address: "192.168.0.51",
		UserAgent: "Linphone-Desktop/6.0", ExpiresAt: t0.Add(expires)}
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

func TestFold(t *testing.T) {
	now := t0
	rotate := func(at time.Duration) *v1alpha1.LastAction {
		return &v1alpha1.LastAction{Kind: v1alpha1.ActionRotateToken, At: *mt(at), Attempts: 1}
	}
	for _, tc := range []struct {
		name    string
		mem     drivers.Memory
		dev     *agentapi.DeviceObservations
		now     time.Time
		profile func(*drivers.Profile)
		want    drivers.Memory
	}{
		{
			name: "nothing observed: memory unchanged",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", HandledRequest: "r1"},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", HandledRequest: "r1"},
		},
		{
			name: "FirstSeen is the earliest fetch, rounded down",
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{
				served(-2*time.Minute+300*time.Millisecond, ""), served(-5*time.Minute+700*time.Millisecond, "")}},
			want: drivers.Memory{FirstSeen: mt(-5 * time.Minute), Address: "192.168.0.50"},
		},
		{
			name: "FirstSeen is now with only a contact",
			dev:  &agentapi.DeviceObservations{Device: testID, Registration: contact(time.Minute)},
			want: drivers.Memory{FirstSeen: mt(0), Address: "192.168.0.51", LastRegistered: mt(0)},
		},
		{
			name: "FirstSeen stays nil with only an expired contact",
			dev:  &agentapi.DeviceObservations{Device: testID, Registration: contact(-drivers.RegistrationGrace)},
			want: drivers.Memory{},
		},
		{
			name: "FirstSeen is kept once set",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour)},
			dev:  &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-time.Minute, "")}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50"},
		},
		{
			name: "Address: a live contact's outranks the latest fetch's source",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour)},
			dev: &agentapi.DeviceObservations{Device: testID, Registration: contact(time.Minute),
				Fetches: []agentapi.Fetch{served(-time.Minute, "")}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(0)},
		},
		{
			name: "Address: the latest fetch's source without a contact",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9"},
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{
				{File: ProvisioningFile(testID), Result: agentapi.FetchNotFound, At: t0.Add(-time.Minute), Source: "192.168.0.77"},
				{File: ProvisioningFile(testID), Result: agentapi.FetchNotFound, At: t0.Add(-2 * time.Minute), Source: "192.168.0.66"},
			}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.77"},
		},
		{
			name: "Address: a registration after the latest fetch keeps its address",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(-time.Minute)},
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-time.Hour, "")},
				Registration: contact(-drivers.RegistrationGrace - time.Second)},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(-time.Minute)},
		},
		{
			name: "Address: a fetch after the last registration moves it",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(-time.Minute)},
			dev:  &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-time.Minute+time.Nanosecond, "")}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", LastRegistered: mt(-time.Minute)},
		},
		{
			name: "LastConfigFetch: the latest served provisioning fetch with a version, rounded down",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour)},
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{
				served(-3*time.Minute, vOld),
				served(-2*time.Minute+400*time.Millisecond, vNew),
				{File: ProvisioningFile(testID), Result: agentapi.FetchNotFound, At: t0.Add(-time.Minute), Source: "192.168.0.50", Version: "x"},
				{File: ProvisioningFile(testID), Result: agentapi.FetchError, At: t0.Add(-time.Minute), Source: "192.168.0.50", Version: "y"},
				{File: "provisioning/softphone-other.xml", Result: agentapi.FetchServed, At: t0.Add(-time.Minute), Source: "192.168.0.50", Version: "z"},
				served(-30*time.Second, ""), // no version: not a config fetch
			}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50",
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-2 * time.Minute)}},
		},
		{
			name: "LastConfigFetch: an older fetch than the remembered one changes nothing",
			mem: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50",
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-time.Minute)}},
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-2*time.Minute, vOld)}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50",
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-time.Minute)}},
		},
		{
			name: "LastConfigFetch: the remembered fetch, stored in whole seconds, is not news",
			mem: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50",
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-time.Minute)}},
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-time.Minute+600*time.Millisecond, vNew)}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50",
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-time.Minute)}},
		},
		{
			name: "LastConfigFetch: another version in the same second is news",
			mem: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50",
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vOld, At: *mt(-time.Minute)}},
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-time.Minute+600*time.Millisecond, vNew)}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50",
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-time.Minute)}},
		},
		{
			name: "after a reissue only a fetch after the rotation counts: at the rotation, not",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", LastAction: rotate(-time.Minute)},
			dev:  &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-time.Minute, vNew)}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", LastAction: rotate(-time.Minute)},
		},
		{
			name: "after a reissue only a fetch after the rotation counts: 1ns after, yes",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", LastAction: rotate(-time.Minute)},
			dev:  &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-time.Minute+time.Nanosecond, vNew)}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", LastAction: rotate(-time.Minute),
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-time.Minute)}},
		},
		{
			name: "a rotation after a remembered fetch does not filter (only a reissue clears the fetch)",
			mem: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", LastAction: rotate(-time.Minute),
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vOld, At: *mt(-3 * time.Minute)}},
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-2*time.Minute, vNew)}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", LastAction: rotate(-time.Minute),
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-2 * time.Minute)}},
		},
		{
			name: "LastRegistered: set when nil (rounded up)",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51"},
			dev:  &agentapi.DeviceObservations{Device: testID, Registration: contact(time.Minute)},
			now:  t0.Add(500 * time.Millisecond),
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(time.Second)},
		},
		{
			name: "LastRegistered: 59 s old is kept",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(-59 * time.Second)},
			dev:  &agentapi.DeviceObservations{Device: testID, Registration: contact(time.Minute)},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(-59 * time.Second)},
		},
		{
			name: "LastRegistered: 60 s old advances",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(-60 * time.Second)},
			dev:  &agentapi.DeviceObservations{Device: testID, Registration: contact(time.Minute)},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(0)},
		},
		{
			name: "LastRegistered: 10 s old advances when a config fetch is after it",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(-10 * time.Second)},
			dev: &agentapi.DeviceObservations{Device: testID, Registration: contact(time.Minute),
				Fetches: []agentapi.Fetch{served(-5*time.Second, vNew)}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(0),
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-5 * time.Second)}},
		},
		{
			name: "a contact that expired 14 s ago is live: its refresh is not seen yet",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", LastRegistered: mt(-time.Hour)},
			dev:  &agentapi.DeviceObservations{Device: testID, Registration: contact(-14 * time.Second)},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(0)},
		},
		{
			name: "a contact that expired the grace ago is not live",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", LastRegistered: mt(-time.Hour)},
			dev:  &agentapi.DeviceObservations{Device: testID, Registration: contact(-drivers.RegistrationGrace)},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", LastRegistered: mt(-time.Hour)},
		},
		{
			name: "a contact expiring in 1 s is live",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", LastRegistered: mt(-time.Hour)},
			dev:  &agentapi.DeviceObservations{Device: testID, Registration: contact(time.Second)},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(0)},
		},
		{
			name: "qualify unreachable is ignored while the profile does not qualify",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", LastRegistered: mt(-time.Hour)},
			dev: &agentapi.DeviceObservations{Device: testID, Registration: func() *agentapi.Registration {
				r := contact(time.Minute)
				r.Qualify = agentapi.QualifyUnreachable
				return r
			}()},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.51", LastRegistered: mt(0)},
		},
		{
			name: "qualify unreachable is no contact when the profile qualifies",
			mem:  drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", LastRegistered: mt(-time.Hour)},
			dev: &agentapi.DeviceObservations{Device: testID, Registration: func() *agentapi.Registration {
				r := contact(time.Minute)
				r.Qualify = agentapi.QualifyUnreachable
				return r
			}()},
			profile: func(p *drivers.Profile) { p.Endpoint.QualifyFrequency = time.Minute },
			want:    drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.9", LastRegistered: mt(-time.Hour)},
		},
		{
			name: "LastAction and HandledRequest are the rows'",
			mem: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", HandledRequest: "r1",
				LastAction: rotate(-10 * time.Minute), LastConfigFetch: &v1alpha1.ConfigFetch{Version: vOld, At: *mt(-20 * time.Minute)}},
			dev: &agentapi.DeviceObservations{Device: testID, Fetches: []agentapi.Fetch{served(-time.Minute, vNew)}},
			want: drivers.Memory{FirstSeen: mt(-time.Hour), Address: "192.168.0.50", HandledRequest: "r1",
				LastAction: rotate(-10 * time.Minute), LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNew, At: *mt(-time.Minute)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.now.IsZero() {
				tc.now = now
			}
			p := cloneProfile(profile)
			if tc.profile != nil {
				tc.profile(&p)
			}
			dev := drivers.Device{Kind: drivers.KindSoftphone, Name: "chris-laptop", ID: testID,
				Model: drivers.ModelSoftphone, Number: "150", Memory: tc.mem}
			before := memString(dev.Memory)
			got := fold(dev, drivers.Observations{Device: tc.dev}, tc.now, p)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("fold =\n%s\nwant\n%s", memString(got), memString(tc.want))
			}
			if memString(dev.Memory) != before {
				t.Error("fold changed its input")
			}
		})
	}
}

func TestFoldDoesNotShareMemory(t *testing.T) {
	dev := drivers.Device{ID: testID, Memory: drivers.Memory{FirstSeen: mt(0),
		LastConfigFetch: &v1alpha1.ConfigFetch{Version: vOld, At: *mt(0)},
		LastAction:      &v1alpha1.LastAction{Kind: v1alpha1.ActionRotateToken, At: *mt(0)}}}
	got := fold(dev, drivers.Observations{}, t0, profile)
	if got.FirstSeen == dev.Memory.FirstSeen || got.LastConfigFetch == dev.Memory.LastConfigFetch || got.LastAction == dev.Memory.LastAction {
		t.Error("fold's memory shares pointers with the device's")
	}
}

func TestSecondRounding(t *testing.T) {
	for _, tc := range []struct {
		in          time.Duration
		floor, ceil time.Duration
	}{
		{0, 0, 0},
		{time.Nanosecond, 0, time.Second},
		{500 * time.Millisecond, 0, time.Second},
		{time.Second, time.Second, time.Second},
		{-time.Nanosecond, -time.Second, 0},
	} {
		if got := floorSecond(t0.Add(tc.in)); !got.Equal(mt(tc.floor)) {
			t.Errorf("floorSecond(t0%+v) = %v, want t0%+v", tc.in, got, tc.floor)
		}
		if got := ceilSecond(t0.Add(tc.in)); !got.Equal(mt(tc.ceil)) {
			t.Errorf("ceilSecond(t0%+v) = %v, want t0%+v", tc.in, got, tc.ceil)
		}
	}
}

// TestRuleTable pins the order of the rows and the state each reports.
func TestRuleTable(t *testing.T) {
	want := []struct {
		row   string
		state drivers.State
	}{
		{"S1", drivers.StateInvalid}, {"S2", drivers.StateUnknown}, {"S3", drivers.StateIssued},
		{"S4", drivers.StateProvisioned}, {"S5", drivers.StateIssued}, {"S6", drivers.StateProvisioned},
		{"S7", drivers.StateRegistered}, {"S8", drivers.StateOffline},
	}
	if len(rules) != len(want) {
		t.Fatalf("%d rules, want %d", len(rules), len(want))
	}
	for i, r := range rules {
		if r.row != want[i].row || r.state != want[i].state {
			t.Errorf("rule %d = %s %s, want %s %s", i, r.row, r.state, want[i].row, want[i].state)
		}
	}
}

// TestNoRuleMatched pins the defensive branch's shape (TestDecideTotal
// proves it is never reached): Unknown, no actions, memory untouched.
func TestNoRuleMatched(t *testing.T) {
	dev := drivers.Device{ID: testID, Request: "r2", Memory: drivers.Memory{HandledRequest: "r1", FirstSeen: mt(0)}}
	d := noRuleMatched(newFacts(dev, drivers.Observations{}, drivers.DecideSettings{}, t0))
	if d.State != drivers.StateUnknown || len(d.Actions) != 0 || d.HumanAction != "" || d.RequeueAfter != 0 {
		t.Errorf("noRuleMatched = %+v", d)
	}
	if len(d.Conditions) != 2 || d.Conditions[0].Type != v1alpha1.ConditionReady || d.Conditions[1].Type != v1alpha1.ConditionRegistered {
		t.Errorf("conditions = %+v", d.Conditions)
	}
	if !reflect.DeepEqual(d.StatusPatch, dev.Memory) {
		t.Errorf("memory changed: %s", memString(d.StatusPatch))
	}
}
