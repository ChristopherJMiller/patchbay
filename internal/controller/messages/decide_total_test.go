package messages_test

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/messages"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// gen draws random inputs for TestDecideTotal.
type gen struct{ r *rand.Rand }

func (g gen) chance(p float64) bool { return g.r.Float64() < p }

func pick[T any](g gen, xs ...T) T { return xs[g.r.IntN(len(xs))] }

// at is at0 moved by a whole number of seconds in [lo, hi).
func (g gen) at(lo, hi time.Duration) time.Time {
	span := int64((hi - lo) / time.Second)
	return at0.Add(lo + time.Duration(g.r.Int64N(span))*time.Second)
}

var totalPhones = []string{"desk", "kitchen", "den"}

func (g gen) phones() map[string]messages.PhoneView {
	out := map[string]messages.PhoneView{}
	for _, name := range totalPhones {
		if g.chance(0.15) {
			continue
		}
		pv := view(name)
		pv.Exists = !g.chance(0.05)
		pv.Push = pick(g, drivers.Supported, drivers.Supported, drivers.Untested, drivers.Unsupported)
		switch {
		case g.chance(0.05):
			pv.Renderer = nil
		case g.chance(0.03):
			pv.Renderer = brokenRenderer{}
		}
		pv.Reachable = !g.chance(0.2)
		pv.PersonCalls = pick(g, 0, 0, 0, 1, 2)
		pv.HeldPage = pick(g, "", "", "0123456789abcdef")
		pv.PushedPage = pick(g, "", "", "", messages.PageCleared, "fedcba9876543210")
		for i := range 7 {
			if g.chance(0.1) { // the shell's last push showed it; the cache may lag
				pv.Showing = append(pv.Showing, types.UID(fmt.Sprintf("uid-m%d", i)))
			}
		}
		if g.chance(0.2) {
			pv.PushFailure = "the phone could not be reached"
			pv.PushFailedAt = g.at(-time.Minute, 0)
		}
		out[name] = pv
	}
	return out
}

func (g gen) messages() []v1alpha1.Message {
	n := g.r.IntN(7)
	var out []v1alpha1.Message
	for i := range n {
		created := g.at(-3*time.Hour, 0)
		if i > 0 && g.chance(0.2) {
			created = out[i-1].CreationTimestamp.Time // a tie: the name decides
		}
		m := message(fmt.Sprintf("m%d", i), pick(g, "desk", "desk", "kitchen", "den", "garage"), created)
		m.Spec.ExpiresAt = metav1.NewTime(created.Add(time.Duration(g.r.IntN(4*3600)-3600) * time.Second))
		m.Spec.Key = pick(g, "", "", "fridge", "leak")
		m.Spec.Urgent = g.chance(0.3)
		if g.chance(0.15) {
			withdrawn(&m)
		}
		m.Status = g.status(m)
		out = append(out, m)
	}
	g.r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func (g gen) status(m v1alpha1.Message) v1alpha1.MessageStatus {
	gen := pick(g, m.Generation, m.Generation, 1)
	finishedAt := ptime(g.at(-30*time.Hour, 0))
	if g.chance(0.05) {
		finishedAt = nil
	}
	var shownAt *metav1.Time
	if g.chance(0.4) {
		shownAt = ptime(m.CreationTimestamp.Time)
	}
	switch g.r.IntN(5) {
	case 0:
		return v1alpha1.MessageStatus{}
	case 1:
		return v1alpha1.MessageStatus{
			State: v1alpha1.MessageStateHeld, ObservedGeneration: gen, Message: "held",
			Reason: pick(g, v1alpha1.ReasonInCall, v1alpha1.ReasonQuietHours, v1alpha1.ReasonAway,
				v1alpha1.ReasonPhoneUnreachable, v1alpha1.ReasonPushFailing),
		}
	case 2:
		return v1alpha1.MessageStatus{State: v1alpha1.MessageStateShown, ShownAt: ptime(m.CreationTimestamp.Time), ObservedGeneration: gen}
	case 3:
		st := pick(g, v1alpha1.MessageStateExpired, v1alpha1.MessageStateWithdrawn, v1alpha1.MessageStateReplaced, v1alpha1.MessageStateFailed)
		reason := ""
		if st == v1alpha1.MessageStateFailed {
			reason = pick(g, v1alpha1.ReasonPhoneMissing, v1alpha1.ReasonPushUnsupported)
		}
		return v1alpha1.MessageStatus{State: st, Reason: reason, FinishedAt: finishedAt, ShownAt: shownAt, ObservedGeneration: gen, Message: "done"}
	}
	return v1alpha1.MessageStatus{State: v1alpha1.MessageStateHeld, Reason: v1alpha1.ReasonQuietHours, ObservedGeneration: gen}
}

func (g gen) input() messages.Input {
	in := messages.Input{Messages: g.messages(), Phones: g.phones(), Location: time.UTC, Now: at0}
	in.House.Quiet, in.House.Away = g.chance(0.3), g.chance(0.2)
	return in
}

// cloneInput is a deep copy of in (the renderers shared).
func cloneInput(in messages.Input) messages.Input {
	out := in
	if in.Messages != nil {
		out.Messages = make([]v1alpha1.Message, len(in.Messages))
	}
	for i := range in.Messages {
		in.Messages[i].DeepCopyInto(&out.Messages[i])
	}
	out.Phones = map[string]messages.PhoneView{}
	for k, v := range in.Phones {
		out.Phones[k] = v
	}
	return out
}

// apply is the input after the shell carried out v: statuses written,
// Messages deleted, each push succeeded (ok) or failed, and the failure of
// a phone owed nothing forgotten.
func apply(in messages.Input, v messages.Verdict, ok bool) messages.Input {
	out := cloneInput(in)
	out.Messages = out.Messages[:0]
	for _, m := range cloneInput(in).Messages {
		if slices.Contains(v.Delete, m.Name) {
			continue
		}
		if st, changed := v.Status[m.Name]; changed {
			m.Status = st
		}
		out.Messages = append(out.Messages, m)
	}
	for _, p := range v.Pushes {
		pv := out.Phones[p.Phone]
		switch {
		case !ok:
			pv.PushFailure, pv.PushFailedAt = "refused", in.Now
		case p.Clear:
			pv.PushedPage, pv.PushFailure, pv.Showing = messages.PageCleared, "", nil
		default:
			pv.PushedPage, pv.PushFailure, pv.Showing = p.PageVersion, "", p.Messages
		}
		out.Phones[p.Phone] = pv
	}
	for name, pv := range out.Phones {
		if pv.PushFailure != "" && !slices.Contains(v.Owed, name) {
			pv.PushFailure, pv.PushFailedAt = "", time.Time{}
			out.Phones[name] = pv
		}
	}
	return out
}

var finalStates = []v1alpha1.MessageState{
	v1alpha1.MessageStateExpired, v1alpha1.MessageStateWithdrawn, v1alpha1.MessageStateReplaced, v1alpha1.MessageStateFailed,
}

var holdReasons = []string{
	v1alpha1.ReasonInCall, v1alpha1.ReasonQuietHours, v1alpha1.ReasonAway, v1alpha1.ReasonPhoneUnreachable, v1alpha1.ReasonPushFailing,
}

// TestDecideTotal: over 20 000 seeded random inputs (Messages in every
// state for phones that exist or not, can push or not, are in a call,
// unreachable, failing; house quiet or away; ties in creation time),
// Decide is deterministic and leaves its input alone; every Message is
// decided by a row; a status is returned only when it changed, final stays
// final, finishedAt is set exactly on final states, a Held Message has no
// shownAt and a hold reason; no push goes to a phone in a person call, one
// that cannot be reached or is waiting to retry, one that cannot push, or
// one no Message names; a push is a page or a clear, and chimes only for a
// page that holds a Message never shown; no Message is Shown at or past
// its expiry; only final Messages a day old are deleted; RequeueAfter
// comes before the next expiry. And it converges: carrying out the
// verdict (pushes succeeding, or failing) and deciding again settles
// within two rounds, with nothing left to push or write.
func TestDecideTotal(t *testing.T) {
	g := gen{rand.New(rand.NewPCG(2026, 1026))}
	rows := map[string]int{}
	for i := range 20000 {
		in := g.input()
		v := messages.Decide(in)
		if g.chance(0.3) && len(v.Pushes) > 0 {
			// The phone holds what Decide wanted it to: the Shown path.
			p := v.Pushes[0]
			pv := in.Phones[p.Phone]
			switch {
			case p.Clear:
				pv.HeldPage, pv.PushedPage = "", ""
			case g.chance(0.5):
				pv.HeldPage, pv.PushedPage = p.PageVersion, ""
			default:
				pv.PushedPage, pv.Showing = p.PageVersion, p.Messages
			}
			in.Phones[p.Phone] = pv
			v = messages.Decide(in)
		}
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("input %d: "+format+"\nhouse %+v\nphones %+v\nmessages\n%sverdict %+v",
				append(append([]any{i}, args...), in.House, in.Phones, fmtMessages(in.Messages), v)...)
		}
		before := cloneInput(in)
		if again := messages.Decide(in); !reflect.DeepEqual(again, v) {
			fail("Decide is not deterministic")
		}
		if !reflect.DeepEqual(before, in) {
			fail("Decide changed its input")
		}
		checkTotal(in, v, fail, rows)

		for _, ok := range []bool{true, false} {
			in2 := apply(in, v, ok)
			v2 := messages.Decide(in2)
			in3 := apply(in2, v2, ok)
			v3 := messages.Decide(in3)
			if len(v2.Pushes) > 0 || len(v3.Pushes) > 0 || len(v3.Status) > 0 || len(v3.Delete) > 0 {
				fail("pushes ok=%v: no fixed point after two rounds:\nround 2 %+v\nround 3 %+v", ok, v2, v3)
			}
			later := map[string]int{}
			checkTotal(in2, v2, fail, later)
			checkTotal(in3, v3, fail, later)
		}
	}
	for _, r := range messages.RuleNames() {
		if rows[r] == 0 {
			t.Errorf("no random input reaches row %s", r)
		}
	}
	t.Logf("rows reached: %v", rows)
}

func checkTotal(in messages.Input, v messages.Verdict, fail func(string, ...any), rows map[string]int) {
	byName := map[string]v1alpha1.Message{}
	named := map[string]bool{}
	for _, m := range in.Messages {
		byName[m.Name] = m
		named[string(m.Spec.Phone)] = true
		r := messages.MatchingRows(in, m.Name)
		if len(r) == 0 {
			fail("%s matches no row", m.Name)
		}
		rows[messages.RuleNames()[r[0]-1]]++
	}
	for name, st := range v.Status {
		m, ok := byName[name]
		if !ok {
			fail("a status for %s, which is not in the input", name)
		}
		if equality.Semantic.DeepEqual(st, m.Status) {
			fail("%s: an unchanged status is returned", name)
		}
		if slices.Contains(finalStates, m.Status.State) && st.State != m.Status.State {
			fail("%s: final %s became %s", name, m.Status.State, st.State)
		}
		isFinal := slices.Contains(finalStates, st.State)
		if isFinal != (st.FinishedAt != nil) {
			fail("%s: state %s with finishedAt %v", name, st.State, st.FinishedAt)
		}
		switch st.State {
		case v1alpha1.MessageStateHeld:
			if st.ShownAt != nil || !slices.Contains(holdReasons, st.Reason) {
				fail("%s: Held with shownAt %v, reason %q", name, st.ShownAt, st.Reason)
			}
		case v1alpha1.MessageStateShown:
			if st.ShownAt == nil || st.Reason != "" {
				fail("%s: Shown with shownAt %v, reason %q", name, st.ShownAt, st.Reason)
			}
		case v1alpha1.MessageStateFailed:
			if st.Reason != v1alpha1.ReasonPhoneMissing && st.Reason != v1alpha1.ReasonPushUnsupported {
				fail("%s: Failed with reason %q", name, st.Reason)
			}
		case v1alpha1.MessageStateExpired, v1alpha1.MessageStateWithdrawn, v1alpha1.MessageStateReplaced:
		default:
			fail("%s: state %q", name, st.State)
		}
		if st.Message == "" && !slices.Contains(finalStates, m.Status.State) {
			fail("%s: no status message", name)
		}
		// Ruling R14: once shown, never held, and shownAt is never cleared
		// or moved.
		if m.Status.ShownAt != nil {
			if st.State == v1alpha1.MessageStateHeld {
				fail("%s: shown at %v, now Held %s", name, m.Status.ShownAt, st.Reason)
			}
			if st.ShownAt == nil || !st.ShownAt.Equal(m.Status.ShownAt) {
				fail("%s: shownAt %v became %v", name, m.Status.ShownAt, st.ShownAt)
			}
		}
	}
	// Owed: every phone pushed to, and every pushable phone some Message
	// names that cannot take its push now; nothing of a phone owed is
	// deleted.
	pushed := map[string]bool{}
	for _, p := range v.Pushes {
		pushed[p.Phone] = true
	}
	if !slices.IsSorted(v.Owed) {
		fail("owed %v is not sorted", v.Owed)
	}
	for p := range pushed {
		if !slices.Contains(v.Owed, p) {
			fail("pushed to %s, which is not owed", p)
		}
	}
	for _, name := range v.Owed {
		pv, ok := in.Phones[name]
		switch {
		case !ok || !pv.Exists || pv.Push == drivers.Unsupported || pv.Renderer == nil || !named[name]:
			fail("%s owed, but it cannot take a push or no Message names it", name)
		case !pushed[name] && pv.PersonCalls == 0 && pv.Reachable &&
			(pv.PushFailure == "" || !in.Now.Before(pv.PushFailedAt.Add(messages.PushRetry))):
			fail("%s owed and not pushed, though it can take a push", name)
		}
	}
	for _, name := range v.Delete {
		if p := string(byName[name].Spec.Phone); slices.Contains(v.Owed, p) {
			fail("%s deleted while %s is owed", name, p)
		}
	}
	// No Message stays Shown at or past its expiry.
	for _, m := range in.Messages {
		st := m.Status
		if s, ok := v.Status[m.Name]; ok {
			st = s
		}
		if st.State == v1alpha1.MessageStateShown && !in.Now.Before(m.Spec.ExpiresAt.Time) && !slices.Contains(v.Delete, m.Name) {
			fail("%s: Shown past its expiry", m.Name)
		}
	}
	seen := map[string]bool{}
	for _, p := range v.Pushes {
		pv, ok := in.Phones[p.Phone]
		switch {
		case seen[p.Phone]:
			fail("two pushes to %s", p.Phone)
		case !ok || !pv.Exists || pv.Push == drivers.Unsupported || pv.Renderer == nil:
			fail("a push to %s, which cannot take one", p.Phone)
		case !named[p.Phone]:
			fail("a push to %s, which no Message names", p.Phone)
		case pv.PersonCalls > 0:
			fail("a push to %s during a person call", p.Phone)
		case !pv.Reachable:
			fail("a push to %s, which cannot be reached", p.Phone)
		case pv.PushFailure != "" && in.Now.Before(pv.PushFailedAt.Add(messages.PushRetry)):
			fail("a push to %s before its retry", p.Phone)
		case p.DeviceID != pv.DeviceID:
			fail("a push to %s addressed to %s", p.Phone, p.DeviceID)
		case p.Clear && (p.Page != nil || p.PageVersion != "" || p.Beep):
			fail("a clear with a page or a chime: %+v", p)
		case !p.Clear && (len(p.Page) == 0 || p.PageVersion != messages.PageVersion(p.Page)):
			fail("a page push without its page or version: %+v", p)
		}
		seen[p.Phone] = true
		if p.Beep {
			fresh := false
			for _, m := range in.Messages {
				if string(m.Spec.Phone) != p.Phone || m.Status.ShownAt != nil || slices.Contains(pv.Showing, m.UID) {
					continue
				}
				switch messages.RuleNames()[messages.MatchingRows(in, m.Name)[0]-1] {
				case "push-failing", "current":
					fresh = true
				}
			}
			if !fresh {
				fail("a chime on %s without a Message never shown", p.Phone)
			}
		}
	}
	for _, name := range v.Delete {
		m := byName[name]
		if !slices.Contains(finalStates, m.Status.State) || m.Status.FinishedAt == nil ||
			in.Now.Before(m.Status.FinishedAt.Add(messages.RetainFinished)) {
			fail("%s deleted: %+v", name, m.Status)
		}
	}
	if v.RequeueAfter < 0 {
		fail("RequeueAfter %s", v.RequeueAfter)
	}
	for _, m := range in.Messages {
		st := m.Status
		if s, ok := v.Status[m.Name]; ok {
			st = s
		}
		if !slices.Contains(finalStates, st.State) && !slices.Contains(v.Delete, m.Name) {
			if d := m.Spec.ExpiresAt.Sub(in.Now); v.RequeueAfter == 0 || v.RequeueAfter > d {
				fail("%s expires in %s, RequeueAfter %s", m.Name, d, v.RequeueAfter)
			}
		}
	}
}

// The renderer is what the shell gives a 79x5.
var _ drivers.MessageRenderer = cisco79x5.New()
