package softphone_test

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

// roundTrip stores a memory the way the controller does (status, JSON,
// back to a Device), with every time in UTC so it compares with
// reflect.DeepEqual.
func roundTrip(t *testing.T, dev drivers.Device, m drivers.Memory, state drivers.State) drivers.Device {
	t.Helper()
	sp := &v1alpha1.Softphone{
		ObjectMeta: metav1.ObjectMeta{Name: dev.Name},
		Spec:       v1alpha1.SoftphoneSpec{Number: v1alpha1.Number(dev.Number), Label: dev.Label},
	}
	if dev.Request != "" {
		sp.Annotations = map[string]string{v1alpha1.AnnotationReissueRequested: dev.Request}
	}
	m.ToSoftphoneStatus(&sp.Status)
	sp.Status.State = string(state)
	b, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	var back v1alpha1.Softphone
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	out := drivers.SoftphoneDevice(&back)
	out.ConfigVersion, out.NumberConflict, out.NumberOutOfPlan, out.MACConflict, out.ClaimMessage =
		dev.ConfigVersion, dev.NumberConflict, dev.NumberOutOfPlan, dev.MACConflict, dev.ClaimMessage
	out.Model = dev.Model
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

// TestLifecycle walks one softphone through its life, storing status
// through JSON between steps as the controller does.
func TestLifecycle(t *testing.T) {
	d := softphone.New()
	dev := device()
	var state drivers.State
	step := func(name string, now time.Duration, o *agentapi.DeviceObservations, wantState drivers.State, wantRotate bool) {
		t.Helper()
		obs := observations(o)
		obs.PolledAt = t0.Add(now)
		got := d.Decide(dev, obs, settings, t0.Add(now))
		if got.State != wantState || (len(got.Actions) == 1) != wantRotate {
			t.Fatalf("%s: state %s, actions %v; want %s, rotate %t (human %q)", name, got.State, got.Actions, wantState, wantRotate, got.HumanAction)
		}
		state = got.State
		dev = roundTrip(t, dev, got.StatusPatch, state)
	}
	gone := (*agentapi.Registration)(nil)
	lapsed := live()
	lapsed.ExpiresAt = t0.Add(20 * time.Minute)

	step("created", 0, nil, drivers.StateIssued, false)
	step("QR scanned: the token is spent", 2*time.Minute+300*time.Millisecond, seen(gone, fetch(vNew, 2*time.Minute)), drivers.StateProvisioned, true)
	step("next poll: no second rotation", 2*time.Minute+900*time.Millisecond, seen(gone, fetch(vNew, 2*time.Minute)), drivers.StateProvisioned, false)
	step("registers", 3*time.Minute, seen(lapsed, fetch(vNew, 2*time.Minute)), drivers.StateRegistered, false)
	step("still registered", 13*time.Minute, seen(lapsed, fetch(vNew, 2*time.Minute)), drivers.StateRegistered, false)
	step("the app sleeps: Offline", 25*time.Minute, seen(gone, fetch(vNew, 2*time.Minute)), drivers.StateOffline, false)
	dev.Request = "2026-10-14T12:30:00Z"
	step("reissue requested", 30*time.Minute+500*time.Millisecond, seen(gone, fetch(vNew, 2*time.Minute)), drivers.StateIssued, true)
	step("the old fetch is still observed: stays Issued", 30*time.Minute+900*time.Millisecond, seen(gone, fetch(vNew, 2*time.Minute)), drivers.StateIssued, false)
	step("the new QR is scanned", 35*time.Minute, seen(gone, fetch(vNew, 2*time.Minute), fetch(vNew, 34*time.Minute)), drivers.StateProvisioned, true)
	back := live()
	back.ExpiresAt = t0.Add(50 * time.Minute)
	step("registers again", 36*time.Minute, seen(back, fetch(vNew, 34*time.Minute)), drivers.StateRegistered, false)
	if dev.Memory.HandledRequest != "2026-10-14T12:30:00Z" {
		t.Errorf("HandledRequest = %q", dev.Memory.HandledRequest)
	}
	dev.ConfigVersion = vOld + "x" // the password Secret was regenerated
	step("its settings changed", 37*time.Minute, seen(back, fetch(vNew, 34*time.Minute)), drivers.StateIssued, false)
}

var reasonPattern = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

// TestDecideTotal runs a seeded grid of agent health, specs, requests,
// memories, observations and times through Decide and checks what must hold
// for every input: the defensive branch is never reached, the conditions
// are complete and in order, a token is rotated only by S3 and S4, a
// request is answered at most once, Unknown changes nothing, and status
// survives storage: deciding again on the stored memory changes nothing and
// rotates nothing.
func TestDecideTotal(t *testing.T) {
	const n = 20000
	rng := rand.New(rand.NewPCG(2026, 1014))
	d := softphone.New()
	pick := func(k int) int { return rng.IntN(k) }
	secs := func(lo, hi int) time.Duration { return time.Duration(lo+pick(hi-lo+1)) * time.Second }
	optTime := func(lo, hi int) *metav1.Time {
		if pick(3) == 0 {
			return nil
		}
		return at(secs(lo, hi))
	}
	order := []string{v1alpha1.ConditionReady, v1alpha1.ConditionProvisioned, v1alpha1.ConditionRegistered,
		v1alpha1.ConditionConfigCurrent, v1alpha1.ConditionNumberConflict}
	versions := []string{"", vOld, vNew}
	reached := map[drivers.State]int{}
	rotations := 0

	for i := range n {
		now := t0.Add(time.Duration(pick(4)) * 250 * time.Millisecond) // sub-second nows too

		// The device: spec, claims, request and memory (whole seconds, as
		// status stores them).
		dev := device()
		switch pick(12) {
		case 0:
			dev.Model = "cisco-7945"
		case 1:
			dev.NumberConflict, dev.ClaimMessage = true, "held"
		case 2:
			dev.NumberOutOfPlan = true
		}
		if pick(2) == 0 {
			dev.ConfigVersion = vOld
		}
		switch pick(3) {
		case 0:
			dev.Request = "r1"
		case 1:
			dev.Request, dev.Memory.HandledRequest = "r1", "r1"
		}
		dev.Memory.FirstSeen = optTime(-7200, -3600)
		if pick(2) == 0 {
			dev.Memory.Address = "192.168.0.9"
		}
		if pick(2) == 0 {
			dev.Memory.LastConfigFetch = fetched(versions[1+pick(2)], secs(-900, -1))
		}
		if pick(2) == 0 {
			dev.Memory.LastAction = rotated(secs(-900, 0))
		}
		dev.Memory.LastRegistered = optTime(-900, 0)
		switch pick(4) {
		case 0:
			dev.LastState = drivers.StateRegistered
		case 1:
			dev.LastState = drivers.StateIssued
		}

		// The observations.
		obs := observations(nil)
		switch pick(8) {
		case 0:
			obs.AgentReachable = false
		case 1:
			obs.PolledAt = now.Add(-stale - time.Nanosecond)
		case 2:
			obs.Agent.AsteriskUp = false
		}
		switch pick(4) {
		case 0:
			obs.CompiledVersion = "d4" // not live yet
		case 1:
			obs.CompiledVersion, obs.Agent.LastApplyVersion, obs.Agent.LastApplyError = "d4", "d4", "refused"
		}
		if pick(4) != 0 {
			o := &agentapi.DeviceObservations{Device: spID}
			for range pick(4) {
				f := fetch(versions[pick(3)], -time.Duration(pick(900_000))*time.Millisecond)
				switch pick(5) {
				case 0:
					f.Result = agentapi.FetchNotFound
				case 1:
					f.Result = agentapi.FetchError
				}
				o.Fetches = append(o.Fetches, f)
			}
			if pick(2) == 0 {
				r := live()
				r.ExpiresAt = now.Add(time.Duration(pick(1200)-600) * time.Second)
				if pick(4) == 0 {
					r.Qualify = agentapi.QualifyUnreachable
				}
				if pick(4) == 0 {
					r.Address = ""
				}
				o.Registration = r
			}
			obs.Device = o
		}

		before := memString(dev.Memory)
		got := d.Decide(dev, obs, settings, now)
		if again := d.Decide(dev, obs, settings, now); !reflect.DeepEqual(got, again) {
			t.Fatalf("case %d: Decide is not deterministic", i)
		}
		if memString(dev.Memory) != before {
			t.Fatalf("case %d: Decide changed its input", i)
		}
		reached[got.State]++
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("case %d (dev %+v, obs %+v, now %s): "+format, append([]any{i, dev, obs, now}, args...)...)
		}

		// Never the defensive branch; Ready always first.
		if len(got.Conditions) == 0 || got.Conditions[0].Type != v1alpha1.ConditionReady {
			fail("no Ready first: %+v", got.Conditions)
		}
		if got.Conditions[0].Message == "no rule matched" {
			fail("the defensive branch was reached")
		}
		invalid := dev.Model != drivers.ModelSoftphone || dev.NumberConflict || dev.NumberOutOfPlan
		problem := obs.AgentProblem(now, stale) != ""
		unknown := !invalid && problem
		var types []string
		for _, c := range got.Conditions {
			types = append(types, c.Type)
			if !reasonPattern.MatchString(c.Reason) {
				fail("condition %s has reason %q", c.Type, c.Reason)
			}
			switch c.Status {
			case metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown:
			default:
				fail("condition %s has status %q", c.Type, c.Status)
			}
		}
		switch {
		case unknown:
			if !slices.Equal(types, []string{v1alpha1.ConditionReady, v1alpha1.ConditionRegistered}) {
				fail("Unknown lists %v", types)
			}
			if !reflect.DeepEqual(got.StatusPatch, dev.Memory) || len(got.Actions) > 0 || got.HumanAction != "" {
				fail("Unknown changed something: %+v", got)
			}
		case !slices.Equal(types, order):
			fail("conditions %v, want %v", types, order)
		}
		if invalid != (got.State == drivers.StateInvalid) {
			fail("state %s for an invalid=%t spec", got.State, invalid)
		}

		// Ready is True exactly when nothing needs anyone.
		readyTrue := got.Conditions[0].Status == metav1.ConditionTrue
		if readyTrue != (got.State == drivers.StateRegistered || got.State == drivers.StateOffline) && !unknown {
			fail("Ready %s in state %s", got.Conditions[0].Status, got.State)
		}

		// Only S3 and S4 rotate, once, recording it.
		pending := dev.Request != "" && dev.Request != dev.Memory.HandledRequest
		if len(got.Actions) > 0 {
			rotations++
			if len(got.Actions) != 1 || got.Actions[0] != (drivers.Action{Kind: v1alpha1.ActionRotateToken}) {
				fail("actions %+v", got.Actions)
			}
			if got.State != drivers.StateIssued && got.State != drivers.StateProvisioned {
				fail("rotated in state %s", got.State)
			}
			la := got.StatusPatch.LastAction
			if la == nil || la.Kind != v1alpha1.ActionRotateToken || la.Attempts != 1 || la.At.Time.Before(now) ||
				la.At.Sub(now) >= time.Second || la.At.Nanosecond() != 0 {
				fail("rotation recorded as %+v", la)
			}
		} else if !reflect.DeepEqual(got.StatusPatch.LastAction, dev.Memory.LastAction) {
			fail("LastAction changed with no action")
		}
		// A request is answered (marked handled) at most once, by S1 with no
		// action or by S3 with a rotation, and never ignored when valid and
		// the agent is fine.
		answeredBy := got.State == drivers.StateInvalid || (got.State == drivers.StateIssued && len(got.Actions) == 1)
		if h := got.StatusPatch.HandledRequest; h != dev.Memory.HandledRequest {
			if h != dev.Request || !pending || !answeredBy {
				fail("HandledRequest %q -> %q in state %s", dev.Memory.HandledRequest, h, got.State)
			}
		} else if pending && !unknown {
			fail("pending request %q not answered (state %s)", dev.Request, got.State)
		}
		if got.RequeueAfter < 0 || got.RequeueAfter > 10*time.Minute {
			fail("RequeueAfter %s", got.RequeueAfter)
		}
		if got.StatusPatch.Firmware != "" || got.StatusPatch.UpgradeStartedAt != nil {
			fail("a softphone has no firmware: %s", memString(got.StatusPatch))
		}

		// Storage changes nothing, and deciding again on the stored memory
		// changes nothing and rotates nothing.
		stored := roundTrip(t, dev, got.StatusPatch, got.State)
		if !reflect.DeepEqual(stored.Memory, got.StatusPatch) {
			fail("storing the memory changed it:\n got %s\nwant %s", memString(stored.Memory), memString(got.StatusPatch))
		}
		next := d.Decide(stored, obs, settings, now)
		if len(next.Actions) > 0 {
			fail("deciding again on the stored memory acts again: %+v (first %s, then %s)", next.Actions, got.State, next.State)
		}
		if !reflect.DeepEqual(next.StatusPatch, got.StatusPatch) {
			fail("deciding again changes the memory:\n got %s\nwant %s", memString(next.StatusPatch), memString(got.StatusPatch))
		}
	}

	// The grid reaches every state.
	for _, s := range []drivers.State{drivers.StateInvalid, drivers.StateUnknown, drivers.StateIssued, drivers.StateProvisioned,
		drivers.StateRegistered, drivers.StateOffline} {
		if reached[s] == 0 {
			t.Errorf("the grid never reached %s: %v", s, reached)
		}
	}
	if rotations == 0 {
		t.Error("the grid never rotated a token")
	}
	t.Logf("%d cases: %v, %d rotations", n, reached, rotations)
}
