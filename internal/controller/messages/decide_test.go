package messages_test

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/messages"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// checkOwed: the verdict owes every phone it pushes to, and the phones in
// notPushed besides.
func checkOwed(t *testing.T, v messages.Verdict, notPushed []string) {
	t.Helper()
	want := slices.Clone(notPushed)
	for _, p := range v.Pushes {
		want = append(want, p.Phone)
	}
	slices.Sort(want)
	want = slices.Compact(want)
	if !slices.Equal(v.Owed, want) && (len(v.Owed) > 0 || len(want) > 0) {
		t.Errorf("owed %v, want %v", v.Owed, want)
	}
}

// uids are the UIDs of msgs.
func uids(msgs ...v1alpha1.Message) []types.UID {
	out := make([]types.UID, len(msgs))
	for i, m := range msgs {
		out[i] = m.UID
	}
	return out
}

// at0 is the decision's clock in the table tests.
var at0 = time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)

const model = "cisco-7945"

// view is a Phone that can be pushed to now: it exists, its model pushes,
// it is registered and the agent answers, nobody is on it, and its screen
// holds nothing.
func view(name string, edit ...func(*messages.PhoneView)) messages.PhoneView {
	pv := messages.PhoneView{
		Name: name, DeviceID: drivers.DeviceID(drivers.KindPhone, name), Model: model,
		Exists: true, Push: drivers.Supported, Reachable: true, Renderer: cisco79x5.New(),
	}
	for _, e := range edit {
		e(&pv)
	}
	return pv
}

// message is a Message for phone, sent at created, expiring an hour later,
// never decided.
func message(name, phone string, created time.Time, edit ...func(*v1alpha1.Message)) v1alpha1.Message {
	m := v1alpha1.Message{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID("uid-" + name), Generation: 1, CreationTimestamp: metav1.NewTime(created),
		},
		Spec: v1alpha1.MessageSpec{
			Phone: v1alpha1.LocalName(phone), Title: "T " + name, Text: "text of " + name,
			ExpiresAt: metav1.NewTime(created.Add(time.Hour)),
		},
	}
	for _, e := range edit {
		e(&m)
	}
	return m
}

func urgent(m *v1alpha1.Message)    { m.Spec.Urgent = true }
func withdrawn(m *v1alpha1.Message) { m.Spec.Withdrawn = true; m.Generation = 2 }

func key(k string) func(*v1alpha1.Message) { return func(m *v1alpha1.Message) { m.Spec.Key = k } }

func expires(t time.Time) func(*v1alpha1.Message) {
	return func(m *v1alpha1.Message) { m.Spec.ExpiresAt = metav1.NewTime(t) }
}

func ptime(t time.Time) *metav1.Time { m := metav1.NewTime(t); return &m }

// shown: decided Shown at t.
func shown(t time.Time) func(*v1alpha1.Message) {
	return func(m *v1alpha1.Message) {
		m.Status = v1alpha1.MessageStatus{
			State: v1alpha1.MessageStateShown, Message: "shown on " + string(m.Spec.Phone),
			ShownAt: ptime(t), ObservedGeneration: m.Generation,
		}
	}
}

// heldFor: decided Held for reason.
func heldFor(reason string) func(*v1alpha1.Message) {
	return func(m *v1alpha1.Message) {
		m.Status = v1alpha1.MessageStatus{State: v1alpha1.MessageStateHeld, Reason: reason, Message: "held", ObservedGeneration: m.Generation}
	}
}

// finished: decided into a final state at t.
func finished(state v1alpha1.MessageState, t time.Time) func(*v1alpha1.Message) {
	return func(m *v1alpha1.Message) {
		m.Status = v1alpha1.MessageStatus{State: state, Message: "done", FinishedAt: ptime(t), ObservedGeneration: m.Generation}
	}
}

func input(msgs []v1alpha1.Message, phones ...messages.PhoneView) messages.Input {
	in := messages.Input{Messages: msgs, Phones: map[string]messages.PhoneView{}, Location: time.UTC, Now: at0}
	for _, p := range phones {
		in.Phones[p.Name] = p
	}
	return in
}

// pageOf renders the page of msgs (in this order, newest first) as Decide
// must, and returns its version.
func pageOf(t *testing.T, msgs ...v1alpha1.Message) (string, []byte) {
	t.Helper()
	lines := make([]drivers.MessageLine, len(msgs))
	for i, m := range msgs {
		lines[i] = drivers.MessageLine{
			Sent: m.CreationTimestamp.Time, Expires: m.Spec.ExpiresAt.Time,
			Title: m.Spec.Title, Text: m.Spec.Text, Urgent: m.Spec.Urgent,
		}
	}
	page, err := cisco79x5.New().RenderMessages(model, lines, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return messages.PageVersion(page), page
}

func versionOf(t *testing.T, msgs ...v1alpha1.Message) string {
	t.Helper()
	v, _ := pageOf(t, msgs...)
	return v
}

// want is a Message's expected new status (only the fields a row decides).
type want struct {
	state    v1alpha1.MessageState
	reason   string
	shownAt  *time.Time // nil: ShownAt nil
	finished bool       // FinishedAt is at0
}

func wantShown(at time.Time) want { return want{state: v1alpha1.MessageStateShown, shownAt: &at} }

// push is an expected PagePush: a page of these Messages (newest first),
// or a clear.
type push struct {
	phone string
	page  []v1alpha1.Message
	beep  bool
	clear bool
}

type decideCase struct {
	name string
	in   messages.Input
	// target is the Message the row is about; row the rule that must
	// decide it (the first matching row).
	target, row string
	status      map[string]want // the changed statuses, exactly
	pushes      []push
	delete      []string
	events      []string // "<message> <type> <reason>", in order
	// owed are the phones owed a page or a clear that is not pushed now
	// (every pushed phone is owed too).
	owed []string
}

func TestDecide(t *testing.T) {
	sent := at0.Add(-10 * time.Minute)
	later := at0.Add(-5 * time.Minute)
	desk := "desk"
	old := message("m-old", desk, sent, shown(sent))
	oldVersion := func(t *testing.T) string { return versionOf(t, old) }
	held := func(r string) want { return want{state: v1alpha1.MessageStateHeld, reason: r} }
	final := func(st v1alpha1.MessageState, r string) want { return want{state: st, reason: r, finished: true} }
	ev := func(name, typ, reason string) string { return name + " " + typ + " " + reason }
	const normal, warning = "Normal", "Warning"

	cases := func(t *testing.T) []decideCase {
		return []decideCase{
			{
				name: "finished-retained: deleted a day after it finished",
				in: input([]v1alpha1.Message{
					message("m", desk, at0.Add(-26*time.Hour), finished(v1alpha1.MessageStateExpired, at0.Add(-24*time.Hour))),
				}, view(desk)),
				target: "m", row: "finished-retained",
				delete: []string{"m"},
			},
			{
				name: "final: kept as it is",
				in: input([]v1alpha1.Message{
					message("m", desk, sent, withdrawn, finished(v1alpha1.MessageStateWithdrawn, at0.Add(-time.Minute))),
				}, view(desk)),
				target: "m", row: "final",
			},
			{
				name: "final without finishedAt: the retention clock starts now",
				in: input([]v1alpha1.Message{
					message("m", desk, sent, func(m *v1alpha1.Message) {
						m.Status = v1alpha1.MessageStatus{
							State: v1alpha1.MessageStateFailed, Reason: v1alpha1.ReasonPhoneMissing, Message: "no Phone desk", ObservedGeneration: 1,
						}
					}),
				}, view(desk)),
				target: "m", row: "final",
				status: map[string]want{"m": final(v1alpha1.MessageStateFailed, v1alpha1.ReasonPhoneMissing)},
			},
			{
				name: "expired: a Shown message expires and leaves the screen",
				in: input([]v1alpha1.Message{message("m-old", desk, sent, shown(sent), expires(at0))},
					view(desk, func(p *messages.PhoneView) { p.HeldPage = oldVersion(t) })),
				target: "m-old", row: "expired",
				status: map[string]want{"m-old": {state: v1alpha1.MessageStateExpired, shownAt: &sent, finished: true}},
				pushes: []push{{phone: desk, clear: true}},
				events: []string{ev("m-old", normal, install.EventReasonMessageExpired)},
			},
			{
				name:   "expired-on-arrival: never pushed, never chimes",
				in:     input([]v1alpha1.Message{message("m", desk, at0, expires(at0))}, view(desk)),
				target: "m", row: "expired",
				status: map[string]want{"m": final(v1alpha1.MessageStateExpired, "")},
				events: []string{ev("m", normal, install.EventReasonMessageExpired)},
			},
			{
				name: "withdrawn: a Held message is withdrawn",
				in: input([]v1alpha1.Message{message("m", desk, sent, withdrawn, heldFor(v1alpha1.ReasonQuietHours))},
					view(desk)),
				target: "m", row: "withdrawn",
				status: map[string]want{"m": final(v1alpha1.MessageStateWithdrawn, "")},
				events: []string{ev("m", normal, install.EventReasonMessageWithdrawn)},
			},
			{
				name: "replaced: a newer one with the key takes over the screen",
				in: input([]v1alpha1.Message{
					message("m-old", desk, sent, key("fridge"), shown(sent)),
					message("m-new", desk, later, key("fridge")),
				}, view(desk, func(p *messages.PhoneView) { p.HeldPage = versionOf(t, message("m-old", desk, sent, key("fridge"))) })),
				target: "m-old", row: "replaced",
				status: map[string]want{"m-old": {state: v1alpha1.MessageStateReplaced, shownAt: &sent, finished: true}},
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m-new", desk, later, key("fridge"))}, beep: true}},
				events: []string{ev("m-old", normal, install.EventReasonMessageReplaced)},
			},
			{
				name: "replaced by a newer one that is itself Held",
				in: func() messages.Input {
					in := input([]v1alpha1.Message{
						message("m-old", desk, sent, key("fridge"), shown(sent)),
						message("m-new", desk, later, key("fridge")),
					}, view(desk, func(p *messages.PhoneView) { p.HeldPage = versionOf(t, message("m-old", desk, sent, key("fridge"))) }))
					in.House.Quiet = true
					return in
				}(),
				target: "m-old", row: "replaced",
				status: map[string]want{
					"m-new": held(v1alpha1.ReasonQuietHours),
					"m-old": {state: v1alpha1.MessageStateReplaced, shownAt: &sent, finished: true},
				},
				pushes: []push{{phone: desk, clear: true}},
				events: []string{
					ev("m-new", normal, install.EventReasonMessageHeld),
					ev("m-old", normal, install.EventReasonMessageReplaced),
				},
			},
			{
				name: "not replaced by a newer one with another key, on another phone, or withdrawn",
				in: input([]v1alpha1.Message{
					message("m", desk, sent, key("fridge"), shown(sent)),
					message("m-other-key", desk, later, key("freezer"), shown(later)),
					message("m-other-phone", "kitchen", later, key("fridge"), shown(later)),
					message("m-withdrawn", desk, later, key("fridge"), withdrawn, finished(v1alpha1.MessageStateWithdrawn, later)),
				}, view(desk, func(p *messages.PhoneView) {
					p.HeldPage = versionOf(t,
						message("m-other-key", desk, later, key("freezer")), message("m", desk, sent, key("fridge")))
				}), view("kitchen", func(p *messages.PhoneView) {
					p.HeldPage = versionOf(t, message("m-other-phone", "kitchen", later, key("fridge")))
				})),
				target: "m", row: "current",
			},
			{
				name:   "phone-missing",
				in:     input([]v1alpha1.Message{message("m", "garage", sent)}, view(desk)),
				target: "m", row: "phone-missing",
				status: map[string]want{"m": final(v1alpha1.MessageStateFailed, v1alpha1.ReasonPhoneMissing)},
				events: []string{ev("m", warning, install.EventReasonMessageFailed)},
			},
			{
				name: "phone-missing: a view that does not exist",
				in: input([]v1alpha1.Message{message("m", desk, sent)},
					view(desk, func(p *messages.PhoneView) { p.Exists = false })),
				target: "m", row: "phone-missing",
				status: map[string]want{"m": final(v1alpha1.MessageStateFailed, v1alpha1.ReasonPhoneMissing)},
				events: []string{ev("m", warning, install.EventReasonMessageFailed)},
			},
			{
				name: "push-unsupported",
				in: input([]v1alpha1.Message{message("m", desk, sent)},
					view(desk, func(p *messages.PhoneView) { p.Push = drivers.Unsupported })),
				target: "m", row: "push-unsupported",
				status: map[string]want{"m": final(v1alpha1.MessageStateFailed, v1alpha1.ReasonPushUnsupported)},
				events: []string{ev("m", warning, install.EventReasonMessageFailed)},
			},
			{
				name: "push-unsupported: a driver that renders no message page",
				in: input([]v1alpha1.Message{message("m", desk, sent)},
					view(desk, func(p *messages.PhoneView) { p.Renderer = nil })),
				target: "m", row: "push-unsupported",
				status: map[string]want{"m": final(v1alpha1.MessageStateFailed, v1alpha1.ReasonPushUnsupported)},
				events: []string{ev("m", warning, install.EventReasonMessageFailed)},
			},
			{
				name: "held-in-call: urgent too",
				in: input([]v1alpha1.Message{message("m", desk, sent, urgent)},
					view(desk, func(p *messages.PhoneView) { p.PersonCalls = 1 })),
				target: "m", row: "held-in-call",
				status: map[string]want{"m": held(v1alpha1.ReasonInCall)},
				events: []string{ev("m", normal, install.EventReasonMessageHeld)},
			},
			{
				name: "held-quiet",
				in: func() messages.Input {
					in := input([]v1alpha1.Message{message("m", desk, sent)}, view(desk))
					in.House.Quiet = true
					return in
				}(),
				target: "m", row: "held-quiet",
				status: map[string]want{"m": held(v1alpha1.ReasonQuietHours)},
				events: []string{ev("m", normal, install.EventReasonMessageHeld)},
			},
			{
				name: "held-away",
				in: func() messages.Input {
					in := input([]v1alpha1.Message{message("m", desk, sent)}, view(desk))
					in.House.Away = true
					return in
				}(),
				target: "m", row: "held-away",
				status: map[string]want{"m": held(v1alpha1.ReasonAway)},
				events: []string{ev("m", normal, install.EventReasonMessageHeld)},
			},
			{
				name: "unreachable",
				in: input([]v1alpha1.Message{message("m", desk, sent)},
					view(desk, func(p *messages.PhoneView) { p.Reachable = false })),
				target: "m", row: "unreachable",
				status: map[string]want{"m": held(v1alpha1.ReasonPhoneUnreachable)},
				events: []string{ev("m", normal, install.EventReasonMessageHeld)},
			},
			{
				name: "push-failing: held with the agent's error, no push before the retry",
				in: input([]v1alpha1.Message{message("m", desk, sent)}, view(desk, func(p *messages.PhoneView) {
					p.PushFailure, p.PushFailedAt = "the phone could not be reached: i/o timeout", at0.Add(-5*time.Second)
				})),
				target: "m", row: "push-failing",
				status: map[string]want{"m": held(v1alpha1.ReasonPushFailing)},
				events: []string{ev("m", warning, install.EventReasonPushFailed)},
				owed:   []string{desk},
			},
			{
				name: "push-failing: pushed again once PushRetry has passed",
				in: input([]v1alpha1.Message{message("m", desk, sent, heldFor(v1alpha1.ReasonPushFailing))}, view(desk, func(p *messages.PhoneView) {
					p.PushFailure, p.PushFailedAt = "held", at0.Add(-messages.PushRetry)
				})),
				target: "m", row: "push-failing",
				status: map[string]want{"m": held(v1alpha1.ReasonPushFailing)},
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m", desk, sent)}, beep: true}},
				events: []string{ev("m", warning, install.EventReasonPushFailed)},
			},
			{
				name:   "current: pushed with the chime, Shown once the phone holds it",
				in:     input([]v1alpha1.Message{message("m", desk, sent)}, view(desk)),
				target: "m", row: "current",
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m", desk, sent)}, beep: true}},
			},
			{
				name: "current: Shown when the shell has just pushed the page",
				in: input([]v1alpha1.Message{message("m", desk, sent, heldFor(v1alpha1.ReasonInCall))},
					view(desk, func(p *messages.PhoneView) { p.PushedPage = versionOf(t, message("m", desk, sent)) })),
				target: "m", row: "current",
				status: map[string]want{"m": wantShown(at0)},
				events: []string{ev("m", normal, install.EventReasonMessageShown)},
			},
			{
				name: "current: Shown when the agent reports the page acknowledged",
				in: input([]v1alpha1.Message{message("m", desk, sent)},
					view(desk, func(p *messages.PhoneView) { p.HeldPage = versionOf(t, message("m", desk, sent)) })),
				target: "m", row: "current",
				status: map[string]want{"m": wantShown(at0)},
				events: []string{ev("m", normal, install.EventReasonMessageShown)},
			},
			{
				name: "current: Shown despite a stale push failure, when the phone holds the page",
				in: input([]v1alpha1.Message{message("m", desk, sent, heldFor(v1alpha1.ReasonPushFailing))}, view(desk, func(p *messages.PhoneView) {
					p.HeldPage = versionOf(t, message("m", desk, sent))
					p.PushFailure, p.PushFailedAt = "timeout", at0.Add(-time.Second)
				})),
				target: "m", row: "push-failing",
				status: map[string]want{"m": wantShown(at0)},
				events: []string{ev("m", normal, install.EventReasonMessageShown)},
			},
			{
				name: "shown-stays-current: a Shown one stays on the page under a new one",
				in: input([]v1alpha1.Message{old, message("m-new", desk, later)},
					view(desk, func(p *messages.PhoneView) { p.HeldPage = oldVersion(t) })),
				target: "m-old", row: "current",
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m-new", desk, later), old}, beep: true}},
			},
			{
				name: "shown-stays-current: quiet hours never pull a message off the screen",
				in: func() messages.Input {
					in := input([]v1alpha1.Message{old}, view(desk, func(p *messages.PhoneView) { p.HeldPage = oldVersion(t) }))
					in.House.Quiet, in.House.Away = true, true
					return in
				}(),
				target: "m-old", row: "current",
			},
			{
				name: "shown-stays-current: in a call, nothing is pushed and nothing is held",
				in: input([]v1alpha1.Message{old, message("m-new", desk, later)}, view(desk, func(p *messages.PhoneView) {
					p.HeldPage, p.PersonCalls = oldVersion(t), 1
				})),
				target: "m-old", row: "current",
				status: map[string]want{"m-new": held(v1alpha1.ReasonInCall)},
				events: []string{ev("m-new", normal, install.EventReasonMessageHeld)},
			},
			{
				name: "urgent-in-quiet: the urgent one shows, the other waits",
				in: func() messages.Input {
					in := input([]v1alpha1.Message{message("m", desk, sent), message("m-urgent", desk, later, urgent)}, view(desk))
					in.House.Quiet = true
					return in
				}(),
				target: "m-urgent", row: "current",
				status: map[string]want{"m": held(v1alpha1.ReasonQuietHours)},
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m-urgent", desk, later, urgent)}, beep: true}},
				events: []string{ev("m", normal, install.EventReasonMessageHeld)},
			},
			{
				name: "urgent while away",
				in: func() messages.Input {
					in := input([]v1alpha1.Message{message("m-urgent", desk, later, urgent)}, view(desk))
					in.House.Away = true
					return in
				}(),
				target: "m-urgent", row: "current",
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m-urgent", desk, later, urgent)}, beep: true}},
			},
			{
				name: "newest-first: by creationTimestamp, then name",
				in: input([]v1alpha1.Message{
					message("m-a", desk, sent), message("m-c", desk, later), message("m-b", desk, later),
				}, view(desk)),
				target: "m-a", row: "current",
				pushes: []push{{phone: desk, page: []v1alpha1.Message{
					message("m-c", desk, later), message("m-b", desk, later), message("m-a", desk, sent),
				}, beep: true}},
			},
			{
				name: "beep-only-new: a re-render for an expiry does not chime",
				in: input([]v1alpha1.Message{
					message("m-old", desk, sent, shown(sent), expires(at0)), message("m-new", desk, later, shown(later)),
				}, view(desk, func(p *messages.PhoneView) {
					p.HeldPage = versionOf(t, message("m-new", desk, later), message("m-old", desk, sent))
				})),
				target: "m-old", row: "expired",
				status: map[string]want{"m-old": {state: v1alpha1.MessageStateExpired, shownAt: &sent, finished: true}},
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m-new", desk, later)}}},
				events: []string{ev("m-old", normal, install.EventReasonMessageExpired)},
			},
			{
				name: "clear-when-none-left: the last one withdrawn",
				in: input([]v1alpha1.Message{message("m-old", desk, sent, shown(sent), withdrawn)},
					view(desk, func(p *messages.PhoneView) { p.HeldPage = oldVersion(t) })),
				target: "m-old", row: "withdrawn",
				status: map[string]want{"m-old": {state: v1alpha1.MessageStateWithdrawn, shownAt: &sent, finished: true}},
				pushes: []push{{phone: desk, clear: true}},
				events: []string{ev("m-old", normal, install.EventReasonMessageWithdrawn)},
			},
			{
				name: "clear-when-none-left: also a page the shell pushed a moment ago",
				in: input([]v1alpha1.Message{message("m", desk, sent, shown(sent), withdrawn)},
					view(desk, func(p *messages.PhoneView) { p.PushedPage = versionOf(t, message("m", desk, sent)) })),
				target: "m", row: "withdrawn",
				status: map[string]want{"m": {state: v1alpha1.MessageStateWithdrawn, shownAt: &sent, finished: true}},
				pushes: []push{{phone: desk, clear: true}},
				events: []string{ev("m", normal, install.EventReasonMessageWithdrawn)},
			},
			{
				name: "no clear twice: the shell cleared it a moment ago",
				in: input([]v1alpha1.Message{message("m", desk, sent, withdrawn, finished(v1alpha1.MessageStateWithdrawn, at0))},
					view(desk, func(p *messages.PhoneView) {
						p.HeldPage, p.PushedPage = versionOf(t, message("m", desk, sent)), messages.PageCleared
					})),
				target: "m", row: "final",
			},
			{
				name: "no clear during a call: owed",
				in: input([]v1alpha1.Message{message("m-old", desk, sent, shown(sent), withdrawn)},
					view(desk, func(p *messages.PhoneView) { p.HeldPage, p.PersonCalls = oldVersion(t), 1 })),
				target: "m-old", row: "withdrawn",
				status: map[string]want{"m-old": {state: v1alpha1.MessageStateWithdrawn, shownAt: &sent, finished: true}},
				events: []string{ev("m-old", normal, install.EventReasonMessageWithdrawn)},
				owed:   []string{desk},
			},
			{
				name: "finished-retained: kept while its phone is owed a clear it cannot take",
				in: input([]v1alpha1.Message{
					message("m-old", desk, at0.Add(-26*time.Hour), shown(at0.Add(-26*time.Hour)), withdrawn,
						finished(v1alpha1.MessageStateWithdrawn, at0.Add(-25*time.Hour))),
				}, view(desk, func(p *messages.PhoneView) {
					p.HeldPage, p.Reachable = versionOf(t, message("m-old", desk, at0.Add(-26*time.Hour))), false
				})),
				target: "m-old", row: "finished-retained",
				owed: []string{desk},
			},
			{
				name: "finished-retained: kept while the clear it is owed goes out",
				in: input([]v1alpha1.Message{
					message("m-old", desk, at0.Add(-26*time.Hour), withdrawn, finished(v1alpha1.MessageStateWithdrawn, at0.Add(-25*time.Hour))),
				}, view(desk, func(p *messages.PhoneView) {
					p.HeldPage = versionOf(t, message("m-old", desk, at0.Add(-26*time.Hour)))
				})),
				target: "m-old", row: "finished-retained",
				pushes: []push{{phone: desk, clear: true}},
			},
			{
				name: "showing: a Message the shell's last push showed counts as shown while the cache is behind",
				in: func() messages.Input {
					fresh := message("m-new", desk, later)
					in := input([]v1alpha1.Message{fresh, message("m-old", desk, sent, shown(sent), withdrawn)},
						view(desk, func(p *messages.PhoneView) {
							p.HeldPage = versionOf(t, fresh, message("m-old", desk, sent))
							p.Showing = uids(fresh, old)
						}))
					in.House.Quiet = true
					return in
				}(),
				target: "m-new", row: "current",
				status: map[string]want{
					"m-new": wantShown(at0),
					"m-old": {state: v1alpha1.MessageStateWithdrawn, shownAt: &sent, finished: true},
				},
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m-new", desk, later)}}},
				events: []string{
					ev("m-new", normal, install.EventReasonMessageShown),
					ev("m-old", normal, install.EventReasonMessageWithdrawn),
				},
			},
			{
				name: "a phone no Message names is left alone (a test push stays)",
				in: input([]v1alpha1.Message{message("m", desk, sent)}, view(desk),
					view("kitchen", func(p *messages.PhoneView) { p.HeldPage = "0123456789abcdef" })),
				target: "m", row: "current",
				pushes: []push{{phone: desk, page: []v1alpha1.Message{message("m", desk, sent)}, beep: true}},
			},
			{
				name: "a page that does not render: held, pushing failed",
				in: input([]v1alpha1.Message{message("m", desk, sent)},
					view(desk, func(p *messages.PhoneView) { p.Renderer = brokenRenderer{} })),
				target: "m", row: "current",
				status: map[string]want{"m": held(v1alpha1.ReasonPushFailing)},
				events: []string{ev("m", warning, install.EventReasonPushFailed)},
			},
		}
	}

	reached := map[string]bool{}
	for _, tc := range cases(t) {
		t.Run(tc.name, func(t *testing.T) {
			rows := messages.MatchingRows(tc.in, tc.target)
			if len(rows) == 0 {
				t.Fatalf("no row matches %s", tc.target)
			}
			if got := messages.RuleNames()[rows[0]-1]; got != tc.row {
				t.Errorf("%s decided by %s, want %s (matching rows %v)", tc.target, got, tc.row, rows)
			}
			reached[messages.RuleNames()[rows[0]-1]] = true
			v := messages.Decide(tc.in)
			checkVerdict(t, tc.in, v, tc.status, tc.pushes, tc.delete, tc.events)
			checkOwed(t, v, tc.owed)
		})
	}
	for _, r := range messages.RuleNames() {
		if !reached[r] {
			t.Errorf("no case reaches row %s", r)
		}
	}
}

// brokenRenderer fails every page.
type brokenRenderer struct{}

func (brokenRenderer) RenderMessages(string, []drivers.MessageLine, *time.Location) ([]byte, error) {
	return nil, errors.New("no page today")
}

func checkVerdict(t *testing.T, in messages.Input, v messages.Verdict, status map[string]want, pushes []push, del []string, events []string) {
	t.Helper()
	if got, wantKeys := slices.Sorted(maps.Keys(v.Status)), slices.Sorted(maps.Keys(status)); !slices.Equal(got, wantKeys) {
		t.Errorf("statuses changed for %v, want %v (%+v)", got, wantKeys, v.Status)
	}
	byName := map[string]v1alpha1.Message{}
	for _, m := range in.Messages {
		byName[m.Name] = m
	}
	for name, w := range status {
		st, ok := v.Status[name]
		if !ok {
			continue
		}
		if st.State != w.state || st.Reason != w.reason {
			t.Errorf("%s: %s/%s, want %s/%s (%q)", name, st.State, st.Reason, w.state, w.reason, st.Message)
		}
		if st.Message == "" {
			t.Errorf("%s: no status message", name)
		}
		switch {
		case w.shownAt == nil && st.ShownAt != nil:
			t.Errorf("%s: shownAt %v, want none", name, st.ShownAt)
		case w.shownAt != nil && (st.ShownAt == nil || !st.ShownAt.Equal(&metav1.Time{Time: *w.shownAt})):
			t.Errorf("%s: shownAt %v, want %v", name, st.ShownAt, *w.shownAt)
		}
		switch {
		case w.finished && (st.FinishedAt == nil || !st.FinishedAt.Time.Equal(at0)):
			t.Errorf("%s: finishedAt %v, want %v", name, st.FinishedAt, at0)
		case !w.finished && st.FinishedAt != nil:
			t.Errorf("%s: finishedAt %v, want none", name, st.FinishedAt)
		}
		if st.ObservedGeneration != byName[name].Generation {
			t.Errorf("%s: observedGeneration %d, want %d", name, st.ObservedGeneration, byName[name].Generation)
		}
	}
	if len(v.Pushes) != len(pushes) {
		t.Fatalf("pushes %+v, want %d", v.Pushes, len(pushes))
	}
	for i, p := range pushes {
		got := v.Pushes[i]
		pv := in.Phones[p.phone]
		if got.Phone != p.phone || got.DeviceID != pv.DeviceID || got.Clear != p.clear || got.Beep != p.beep {
			t.Errorf("push %d: %s %s clear=%v beep=%v, want %s %s clear=%v beep=%v",
				i, got.Phone, got.DeviceID, got.Clear, got.Beep, p.phone, pv.DeviceID, p.clear, p.beep)
		}
		if p.clear {
			if got.Page != nil || got.PageVersion != "" {
				t.Errorf("push %d: a clear with a page (%s)", i, got.PageVersion)
			}
			continue
		}
		version, page := pageOf(t, p.page...)
		if got.PageVersion != version || string(got.Page) != string(page) {
			t.Errorf("push %d: page %s:\n%s\nwant %s:\n%s", i, got.PageVersion, got.Page, version, page)
		}
		if want := uids(p.page...); !slices.Equal(got.Messages, want) {
			t.Errorf("push %d: Messages %v, want %v", i, got.Messages, want)
		}
	}
	if !slices.Equal(v.Delete, del) {
		t.Errorf("delete %v, want %v", v.Delete, del)
	}
	var gotEvents []string
	for _, e := range v.Events {
		gotEvents = append(gotEvents, e.Message+" "+e.Type+" "+e.Reason)
		if !strings.Contains(e.Note, e.Message) {
			t.Errorf("event note %q does not name its Message %s", e.Note, e.Message)
		}
		if e.Action == "" {
			t.Errorf("event %+v has no action", e)
		}
	}
	if !slices.Equal(gotEvents, events) {
		t.Errorf("events %v, want %v", gotEvents, events)
	}
}

// TestRuleOrder: the rule table's rows, in the order the brief gives
// (rows 1-11), then push-failing (ruling R15) and current.
func TestRuleOrder(t *testing.T) {
	want := []string{
		"finished-retained", "final", "expired", "withdrawn", "replaced", "phone-missing", "push-unsupported",
		"held-in-call", "held-quiet", "held-away", "unreachable", "push-failing", "current",
	}
	if got := messages.RuleNames(); !slices.Equal(got, want) {
		t.Errorf("rule order = %v, want %v", got, want)
	}
}

// TestDecideResendsAfterAgentRestart (Review Focus 1): the agent's page
// store is memory, so after a restart it reports no page while a Message
// is Shown. Decide pushes the same page again, without the chime, and the
// Message stays Shown.
func TestDecideResendsAfterAgentRestart(t *testing.T) {
	sent := at0.Add(-10 * time.Minute)
	m := message("m", "desk", sent, shown(sent))
	in := input([]v1alpha1.Message{m}, view("desk"))
	v := messages.Decide(in)
	checkVerdict(t, in, v, nil, []push{{phone: "desk", page: []v1alpha1.Message{m}}}, nil, nil)

	// Once the shell has pushed it, nothing more.
	in.Phones["desk"] = view("desk", func(p *messages.PhoneView) { p.PushedPage = v.Pushes[0].PageVersion })
	checkVerdict(t, in, messages.Decide(in), nil, nil, nil, nil)
}

// TestDecideLongLivedPageAcrossMidnight: a message living over 12 h shows
// its weekday on the page ("Mon 19:00"), from the message alone, so the
// page the phone holds is still the one Decide wants after midnight, and
// nothing is pushed again.
func TestDecideLongLivedPageAcrossMidnight(t *testing.T) {
	sent := at0.Add(-time.Hour) // Monday 19:00 UTC
	m := message("m", "desk", sent, shown(sent), expires(sent.Add(20*time.Hour)))
	held, page := pageOf(t, m)
	if !strings.Contains(string(page), "Mon 19:00  T m: text of m") {
		t.Fatalf("the page holds no weekday:\n%s", page)
	}
	for _, now := range []time.Time{at0, at0.Add(5 * time.Hour)} { // Monday 20:00, Tuesday 01:00
		in := input([]v1alpha1.Message{m}, view("desk", func(p *messages.PhoneView) { p.HeldPage = held }))
		in.Now = now
		checkVerdict(t, in, messages.Decide(in), nil, nil, nil, nil)
	}
}

// TestDecideOwedClearOutlivesRetention (ruling R16a): the last Message on
// a phone is withdrawn during a call that lasts past RetainFinished. The
// phone is owed a clear all along, so the Message is kept (and keeps the
// phone named) until the call ends and the clear lands; then it is
// deleted.
func TestDecideOwedClearOutlivesRetention(t *testing.T) {
	sent := at0.Add(-26 * time.Hour)
	m := message("m", "desk", sent, shown(sent), withdrawn, finished(v1alpha1.MessageStateWithdrawn, at0.Add(-25*time.Hour)))
	held := versionOf(t, message("m", "desk", sent))
	in := input([]v1alpha1.Message{m}, view("desk", func(p *messages.PhoneView) { p.HeldPage, p.PersonCalls = held, 1 }))
	v := messages.Decide(in)
	checkVerdict(t, in, v, nil, nil, nil, nil)
	checkOwed(t, v, []string{"desk"})
	if v.RequeueAfter == 0 || v.RequeueAfter > messages.PushRetry {
		t.Errorf("RequeueAfter %s, want at most PushRetry", v.RequeueAfter)
	}

	// The call ends: the clear goes out; m stays until it has landed.
	in.Phones["desk"] = view("desk", func(p *messages.PhoneView) { p.HeldPage = held })
	v = messages.Decide(in)
	checkVerdict(t, in, v, nil, []push{{phone: "desk", clear: true}}, nil, nil)

	// The clear landed: m is deleted.
	in.Phones["desk"] = view("desk", func(p *messages.PhoneView) { p.HeldPage, p.PushedPage = held, messages.PageCleared })
	v = messages.Decide(in)
	checkVerdict(t, in, v, nil, nil, []string{"m"}, nil)
	checkOwed(t, v, nil)
}

// TestDecideRequeue: RequeueAfter is the next time something changes on
// its own: the earliest expiry, the earliest deletion, PushRetry while a
// phone cannot be reached or a push failed, or the end of a push's retry
// wait.
func TestDecideRequeue(t *testing.T) {
	sent := at0.Add(-10 * time.Minute)
	for _, tc := range []struct {
		name string
		in   messages.Input
		want time.Duration
	}{
		{"nothing", input(nil, view("desk")), 0},
		{"the earliest expiry", input([]v1alpha1.Message{
			message("m1", "desk", sent, shown(sent), expires(at0.Add(20*time.Minute))),
			message("m2", "desk", sent, shown(sent), expires(at0.Add(7*time.Minute))),
		}, view("desk", func(p *messages.PhoneView) {
			p.HeldPage = versionOf(t, message("m2", "desk", sent), message("m1", "desk", sent))
		})), 7 * time.Minute},
		{"PushRetry while a page is owed (here, re-sent)", input([]v1alpha1.Message{
			message("m1", "desk", sent, shown(sent), expires(at0.Add(20*time.Minute))),
		}, view("desk")), messages.PushRetry},
		{"PushRetry while a clear is owed during a call", input([]v1alpha1.Message{
			message("m1", "desk", sent, shown(sent), withdrawn, finished(v1alpha1.MessageStateWithdrawn, at0.Add(-time.Minute))),
		}, view("desk", func(p *messages.PhoneView) {
			p.HeldPage, p.PersonCalls = versionOf(t, message("m1", "desk", sent)), 1
		})), messages.PushRetry},
		{"a failure on a phone owed nothing schedules nothing", input([]v1alpha1.Message{
			message("m1", "desk", sent, finished(v1alpha1.MessageStateExpired, at0.Add(-time.Hour))),
		}, view("desk", func(p *messages.PhoneView) {
			p.PushFailure, p.PushFailedAt = "refused", at0.Add(-10*time.Second)
		})), messages.RetainFinished - time.Hour},
		{"a deletion", input([]v1alpha1.Message{
			message("m1", "desk", sent, finished(v1alpha1.MessageStateExpired, at0.Add(-23*time.Hour))),
		}, view("desk")), time.Hour},
		{"a deletion of one finished now", input([]v1alpha1.Message{
			message("m1", "desk", sent, withdrawn),
		}, view("desk")), messages.RetainFinished},
		{"capped while a phone cannot be reached", input([]v1alpha1.Message{
			message("m1", "desk", sent, expires(at0.Add(20*time.Minute))),
		}, view("desk", func(p *messages.PhoneView) { p.Reachable = false })), messages.PushRetry},
		{"the end of a push's retry wait", input([]v1alpha1.Message{
			message("m1", "desk", sent, expires(at0.Add(20*time.Minute))),
		}, view("desk", func(p *messages.PhoneView) {
			p.PushFailure, p.PushFailedAt = "refused", at0.Add(-20*time.Second)
		})), 10 * time.Second},
		{"PushRetry after a retry goes out", input([]v1alpha1.Message{
			message("m1", "desk", sent, expires(at0.Add(20*time.Minute))),
		}, view("desk", func(p *messages.PhoneView) {
			p.PushFailure, p.PushFailedAt = "refused", at0.Add(-time.Minute)
		})), messages.PushRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := messages.Decide(tc.in).RequeueAfter; got != tc.want {
				t.Errorf("RequeueAfter %s, want %s", got, tc.want)
			}
		})
	}
}

// TestDecideStatusTexts: the status messages and Event notes say what
// happened in words, bounded, and never quote the message's text.
func TestDecideStatusTexts(t *testing.T) {
	sent := at0.Add(-10 * time.Minute)
	long := strings.Repeat("x", 3000)
	in := input([]v1alpha1.Message{
		message("m-fail", "desk", sent),
		message("m-gone", "garage", sent),
		message("m-new", "desk", fiveAfter(sent), key("fridge")),
		message("m-old", "desk", sent, key("fridge"), shown(sent)),
	}, view("desk", func(p *messages.PhoneView) { p.PushFailure, p.PushFailedAt = long, at0 }))
	v := messages.Decide(in)
	for name, st := range v.Status {
		if len(st.Message) > install.MaxStatusMessage {
			t.Errorf("%s: status message of %d bytes", name, len(st.Message))
		}
		if strings.Contains(st.Message, "text of") {
			t.Errorf("%s: status message quotes the text: %q", name, st.Message)
		}
	}
	for _, e := range v.Events {
		if len(e.Note) > install.MaxEventNote {
			t.Errorf("%s: note of %d bytes", e.Message, len(e.Note))
		}
		if strings.Contains(e.Note, "text of") {
			t.Errorf("%s: note quotes the text: %q", e.Message, e.Note)
		}
	}
	if got := v.Status["m-old"].Message; !strings.Contains(got, "m-new") {
		t.Errorf("replaced: %q does not name the newer Message", got)
	}
	if got := v.Status["m-gone"].Message; !strings.Contains(got, "garage") {
		t.Errorf("phone missing: %q does not name the Phone", got)
	}
	if got := v.Status["m-fail"].Message; !strings.Contains(got, "xxxx") {
		t.Errorf("push failing: %q does not hold the failure", got)
	}
}

func fiveAfter(t time.Time) time.Time { return t.Add(5 * time.Minute) }

// TestDecideHouse: the zero House (none ever received) delivers.
func TestDecideHouse(t *testing.T) {
	in := input([]v1alpha1.Message{message("m", "desk", at0.Add(-time.Minute))}, view("desk"))
	in.House = messagesapi.House{}
	if v := messages.Decide(in); len(v.Pushes) != 1 {
		t.Errorf("no house state: %d pushes, want 1", len(v.Pushes))
	}
}

func TestPageVersion(t *testing.T) {
	v := messages.PageVersion([]byte("page"))
	if len(v) != 16 || strings.Trim(v, "0123456789abcdef") != "" {
		t.Errorf("PageVersion = %q, want 16 hex digits", v)
	}
	if v == messages.PageVersion([]byte("page2")) {
		t.Error("two pages, one version")
	}
	if strings.Trim(messages.PageCleared, "0123456789abcdef") == "" {
		t.Errorf("PageCleared %q could be a page version", messages.PageCleared)
	}
}

func fmtMessages(ms []v1alpha1.Message) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "  %s %s created %s expires %s key %q urgent %v withdrawn %v status %+v\n",
			m.Name, m.Spec.Phone, m.CreationTimestamp.Sub(at0), m.Spec.ExpiresAt.Sub(at0), m.Spec.Key, m.Spec.Urgent, m.Spec.Withdrawn, m.Status)
	}
	return b.String()
}
