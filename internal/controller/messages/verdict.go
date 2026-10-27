package messages

import (
	"fmt"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Decide's outcomes: each row's status, the page pass, and the verdict.

// retire is row 1: the Message is deleted in the verdict unless its phone
// is owed.
func (d *decider) retire(m *v1alpha1.Message) { d.retired = append(d.retired, m) }

func (d *decider) keep(m *v1alpha1.Message) {
	if m.Status.FinishedAt != nil {
		return
	}
	st := *m.Status.DeepCopy()
	st.FinishedAt = d.now.DeepCopy()
	d.status[m.Name] = st
}

func (d *decider) expire(m *v1alpha1.Message) {
	msg := "expired"
	if d.unshown(m) {
		msg = "expired before it was shown"
	}
	d.finish(m, v1alpha1.MessageStateExpired, "", msg)
}

func (d *decider) withdraw(m *v1alpha1.Message) {
	msg := "withdrawn"
	if d.unshown(m) {
		msg = "withdrawn before it was shown"
	}
	d.finish(m, v1alpha1.MessageStateWithdrawn, "", msg)
}

func (d *decider) replace(m *v1alpha1.Message) {
	d.finish(m, v1alpha1.MessageStateReplaced, "", "replaced by "+d.replacement(m).Name)
}

func (d *decider) phoneMissing(m *v1alpha1.Message) {
	d.finish(m, v1alpha1.MessageStateFailed, v1alpha1.ReasonPhoneMissing, "no Phone "+string(m.Spec.Phone))
}

func (d *decider) pushUnsupported(m *v1alpha1.Message) {
	pv, _ := d.phone(m)
	d.finish(m, v1alpha1.MessageStateFailed, v1alpha1.ReasonPushUnsupported,
		fmt.Sprintf("Phone %s (%s) cannot show messages", pv.Name, pv.Model))
}

func (d *decider) holdInCall(m *v1alpha1.Message) {
	d.hold(m, v1alpha1.ReasonInCall, string(m.Spec.Phone)+" is in a call")
}

func (d *decider) holdQuiet(m *v1alpha1.Message) { d.hold(m, v1alpha1.ReasonQuietHours, "quiet hours") }

func (d *decider) holdAway(m *v1alpha1.Message) { d.hold(m, v1alpha1.ReasonAway, "nobody is home") }

func (d *decider) holdUnreachable(m *v1alpha1.Message) {
	d.hold(m, v1alpha1.ReasonPhoneUnreachable,
		string(m.Spec.Phone)+" cannot be reached: the agent does not answer or the phone is not registered")
}

func (d *decider) onPage(m *v1alpha1.Message) {
	pv, _ := d.phone(m)
	d.page[pv.Name] = append(d.page[pv.Name], pageLine{m: m, failing: d.isPushFailing(m)})
}

// set decides m's status: its own with state, reason and message (bounded)
// for the current generation.
func (d *decider) set(m *v1alpha1.Message, state v1alpha1.MessageState, reason, msg string) *v1alpha1.MessageStatus {
	st := *m.Status.DeepCopy()
	st.State, st.Reason, st.Message = state, reason, install.Text(msg, install.MaxStatusMessage)
	st.ObservedGeneration = m.Generation
	d.status[m.Name] = st
	return &st
}

func (d *decider) finish(m *v1alpha1.Message, state v1alpha1.MessageState, reason, msg string) {
	st := d.set(m, state, reason, msg)
	st.FinishedAt = d.now.DeepCopy()
	d.status[m.Name] = *st
}

// hold holds a Message never shown (every caller checks d.unshown first):
// its shownAt is unset already, and hold never touches it.
func (d *decider) hold(m *v1alpha1.Message, reason, msg string) {
	st := d.set(m, v1alpha1.MessageStateHeld, reason, msg)
	st.FinishedAt = nil
	d.status[m.Name] = *st
}

func (d *decider) show(m *v1alpha1.Message) {
	st := d.set(m, v1alpha1.MessageStateShown, "", "shown on "+string(m.Spec.Phone))
	if st.ShownAt == nil {
		st.ShownAt = d.now.DeepCopy()
	}
	st.FinishedAt = nil
	d.status[m.Name] = *st
}

func (d *decider) pushFailing(m *v1alpha1.Message, failure string) {
	d.hold(m, v1alpha1.ReasonPushFailing, "pushing to "+string(m.Spec.Phone)+" failed: "+failure)
}

// screen decides one phone's page: Shown, a push, a clear, or nothing, and
// whether the phone is owed one.
func (d *decider) screen(name string) {
	pv, ok := d.in.Phones[name]
	if !ok || !pv.pushable() {
		return
	}
	lines := d.page[name]
	if len(lines) == 0 {
		if pv.onPhone() == "" {
			return
		}
		d.owed[name] = true
		if pv.canPush(d.in.Now) {
			d.pushes = append(d.pushes, PagePush{Phone: name, DeviceID: pv.DeviceID, Clear: true})
		}
		return
	}
	shown := make([]drivers.MessageLine, len(lines))
	uids := make([]types.UID, len(lines))
	for i, l := range lines {
		shown[i] = drivers.MessageLine{
			Sent: l.m.CreationTimestamp.Time, Expires: l.m.Spec.ExpiresAt.Time,
			Title: l.m.Spec.Title, Text: l.m.Spec.Text, Urgent: l.m.Spec.Urgent,
		}
		uids[i] = l.m.UID
	}
	page, err := pv.Renderer.RenderMessages(pv.Model, shown, d.in.Location)
	if err != nil {
		for _, l := range lines {
			if d.unshown(l.m) {
				d.pushFailing(l.m, "rendering the page: "+err.Error())
			}
		}
		return
	}
	version := PageVersion(page)
	held := pv.onPhone() == version
	beep := false
	for _, l := range lines {
		fresh := d.unshown(l.m)
		switch {
		case held || !fresh:
			d.show(l.m)
		case l.failing:
			d.pushFailing(l.m, pv.PushFailure)
		}
		beep = beep || fresh
	}
	if held {
		return
	}
	d.owed[name] = true
	if pv.canPush(d.in.Now) {
		d.pushes = append(d.pushes, PagePush{
			Phone: name, DeviceID: pv.DeviceID, PageVersion: version, Page: page, Beep: beep, Messages: uids,
		})
	}
}

// verdict keeps the statuses that changed, with their Events, deletes the
// retired Messages of phones that are not owed, and works out when to look
// again.
func (d *decider) verdict() Verdict {
	v := Verdict{Status: map[string]v1alpha1.MessageStatus{}, Pushes: d.pushes, Owed: slices.Sorted(maps.Keys(d.owed))}
	for _, m := range d.retired {
		if !d.owed[string(m.Spec.Phone)] {
			v.Delete = append(v.Delete, m.Name)
		}
	}
	slices.Sort(v.Delete)
	for _, m := range d.msgs { // newest first
		st, ok := d.status[m.Name]
		if !ok || equality.Semantic.DeepEqual(st, m.Status) {
			continue
		}
		v.Status[m.Name] = st
		if e, ok := event(m, st); ok {
			v.Events = append(v.Events, e)
		}
	}
	v.RequeueAfter = d.requeue(v)
	return v
}

// event is the Event of a status change: one when the state or reason
// changes, or a push failure's message.
func event(m *v1alpha1.Message, st v1alpha1.MessageStatus) (Event, bool) {
	before := m.Status
	if before.State == st.State && before.Reason == st.Reason &&
		(st.Reason != v1alpha1.ReasonPushFailing || before.Message == st.Message) {
		return Event{}, false
	}
	e := Event{Message: m.Name, Type: corev1.EventTypeNormal}
	note := m.Name + ": " + st.Message
	switch st.State {
	case v1alpha1.MessageStateHeld:
		e.Reason, e.Action = install.EventReasonMessageHeld, install.EventActionHold
		note = m.Name + " held: " + st.Reason + ": " + st.Message
		if st.Reason == v1alpha1.ReasonPushFailing {
			e.Type, e.Reason, e.Action = corev1.EventTypeWarning, install.EventReasonPushFailed, install.EventActionPush
			note = m.Name + ": " + st.Message
		}
	case v1alpha1.MessageStateShown:
		e.Reason, e.Action = install.EventReasonMessageShown, install.EventActionPush
	case v1alpha1.MessageStateExpired:
		e.Reason, e.Action = install.EventReasonMessageExpired, install.EventActionExpire
	case v1alpha1.MessageStateWithdrawn:
		e.Reason, e.Action = install.EventReasonMessageWithdrawn, install.EventActionWithdraw
	case v1alpha1.MessageStateReplaced:
		e.Reason, e.Action = install.EventReasonMessageReplaced, install.EventActionReplace
	case v1alpha1.MessageStateFailed:
		e.Type, e.Reason, e.Action = corev1.EventTypeWarning, install.EventReasonMessageFailed, install.EventActionDecide
		note = m.Name + " failed: " + st.Reason + ": " + st.Message
	default:
		return Event{}, false
	}
	e.Note = install.EventNote(note)
	return e, true
}

// requeue is how long until something changes on its own: the next expiry
// or deletion, at most PushRetry while a Message waits for an unreachable
// phone or a failed push or a phone is owed, and an owed phone's retry.
// A push failure on a phone that is owed nothing schedules nothing.
func (d *decider) requeue(v Verdict) time.Duration {
	var next time.Time
	consider := func(t time.Time) {
		if t.After(d.in.Now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	retry := len(v.Owed) > 0
	for _, m := range d.msgs {
		if slices.Contains(v.Delete, m.Name) {
			continue
		}
		st, ok := v.Status[m.Name]
		if !ok {
			st = m.Status
		}
		switch {
		case final(st.State):
			if st.FinishedAt != nil {
				consider(st.FinishedAt.Add(RetainFinished))
			}
		default:
			consider(m.Spec.ExpiresAt.Time)
			if st.State == v1alpha1.MessageStateHeld &&
				(st.Reason == v1alpha1.ReasonPhoneUnreachable || st.Reason == v1alpha1.ReasonPushFailing) {
				retry = true
			}
		}
	}
	for _, name := range v.Owed {
		if pv := d.in.Phones[name]; pv.PushFailure != "" {
			consider(pv.PushFailedAt.Add(PushRetry))
		}
	}
	if retry {
		consider(d.in.Now.Add(PushRetry))
	}
	if next.IsZero() {
		return 0
	}
	return next.Sub(d.in.Now)
}
