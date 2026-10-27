package messages

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Timings of the decision.
const (
	// RetainFinished is how long a Message in a final state is kept before
	// it is deleted.
	RetainFinished = 24 * time.Hour
	// PushRetry is how long a phone whose push failed waits before the next
	// one, and how often Messages waiting for an unreachable phone are
	// looked at again.
	PushRetry = 30 * time.Second
)

// PageCleared is PhoneView.PushedPage after the shell cleared the screen:
// it holds no page. A page version is hex, so it never reads as one.
const PageCleared = "cleared"

// PhoneView is what Decide knows of one Phone: the resource, what its
// driver can do, the agent's observations of it, and the shell's memory of
// its last pushes.
type PhoneView struct {
	// Name is the Phone's name, DeviceID its device ID
	// (drivers.PhoneDevice), Model its spec.model.
	Name, DeviceID, Model string
	// Exists is false for a Phone that is not there.
	Exists bool
	// Push is the model's FeaturePushNotification support on the install's
	// signalling backend.
	Push drivers.Support
	// Reachable: the agent answers (and its observations are fresh) and
	// the phone has a live registration.
	Reachable bool
	// PersonCalls is how many person-to-person calls (ringing included)
	// are up on the phone: the agent's Calls less its StreamCalls.
	PersonCalls int
	// HeldPage is the page version the phone last acknowledged, as the
	// agent reports it (DeviceObservations.Page); "" for none.
	HeldPage string
	// PushedPage is the page version the shell last pushed successfully
	// (PageCleared after a clear), kept until the agent's observations
	// show it or 30 s pass; "" when there is none. It is newer than
	// HeldPage, so while it is set it is what the phone holds.
	PushedPage string
	// PushFailure is why the shell's last push to the phone failed, ""
	// once one has succeeded since; PushFailedAt is when it failed.
	PushFailure  string
	PushFailedAt time.Time
	// Renderer renders the model's message page; nil when its driver
	// cannot.
	Renderer drivers.MessageRenderer
}

// onPhone is the page version the phone is taken to hold, "" for none:
// the shell's push when it made one a moment ago, else the agent's
// observation.
func (p PhoneView) onPhone() string {
	switch p.PushedPage {
	case "":
		return p.HeldPage
	case PageCleared:
		return ""
	}
	return p.PushedPage
}

// canPush reports whether a push may go to the phone now: nobody is on a
// person call, it can be reached, and it is not waiting to retry a failed
// push.
func (p PhoneView) canPush(now time.Time) bool {
	return p.PersonCalls == 0 && p.Reachable && !p.waiting(now)
}

// waiting reports whether the phone's last push failed less than
// PushRetry ago.
func (p PhoneView) waiting(now time.Time) bool {
	return p.PushFailure != "" && now.Before(p.PushFailedAt.Add(PushRetry))
}

// pushable reports whether the phone exists and its model takes pushes.
func (p PhoneView) pushable() bool {
	return p.Exists && p.Push != drivers.Unsupported && p.Renderer != nil
}

// Input is everything Decide judges the Messages from.
type Input struct {
	// Messages are every Message of the namespace.
	Messages []v1alpha1.Message
	// House is the house state; the zero House when none was ever told
	// (not quiet, someone home: deliver).
	House messagesapi.House
	// Phones are the Phones by name.
	Phones map[string]PhoneView
	// Location is the install's timezone, for the times on the page.
	Location *time.Location
	// Now is the controller's clock.
	Now time.Time
}

// PagePush is one push to a phone: a page (with the chime when Beep), or a
// clear.
type PagePush struct {
	Phone, DeviceID, PageVersion string
	Page                         []byte
	Beep, Clear                  bool
}

// Event is one Event to emit about a Message once its status is written.
type Event struct {
	// Message is the Message's name.
	Message string
	Type    string // corev1.EventTypeNormal or EventTypeWarning
	Reason  string
	Action  string
	Note    string
}

// Verdict is what to do.
type Verdict struct {
	// Status holds the new status of each Message whose status changed, by
	// name.
	Status map[string]v1alpha1.MessageStatus
	// Pushes go out before the statuses are written, one per phone, in
	// phone name order.
	Pushes []PagePush
	// Delete names the Messages to delete, sorted.
	Delete []string
	// RequeueAfter is when something changes on its own (an expiry, a
	// deletion, a retry); 0 for never.
	RequeueAfter time.Duration
	// Events are emitted after the status writes, each regarding its
	// Message.
	Events []Event
}

// PageVersion names a page: the first 16 hex digits of its SHA-256.
func PageVersion(page []byte) string {
	sum := sha256.Sum256(page)
	return hex.EncodeToString(sum[:])[:16]
}

// rule is one row of the table: its name, when it matches a Message, and
// what it decides.
type rule struct {
	name    string
	match   func(*decider, *v1alpha1.Message) bool
	outcome func(*decider, *v1alpha1.Message)
}

// rules is the table, in order (first match wins). Rows 8-12 apply only to
// a Message never shown (ruling R14): once Shown, a Message stays Shown
// and on the page until it expires, is withdrawn or is replaced.
var rules = []rule{
	{"finished-retained", (*decider).isRetained, (*decider).remove},
	{"final", (*decider).isFinal, (*decider).keep},
	{"expired", (*decider).isExpired, (*decider).expire},
	{"withdrawn", (*decider).isWithdrawn, (*decider).withdraw},
	{"replaced", (*decider).isReplaced, (*decider).replace},
	{"phone-missing", (*decider).isPhoneMissing, (*decider).phoneMissing},
	{"push-unsupported", (*decider).isPushUnsupported, (*decider).pushUnsupported},
	{"held-in-call", (*decider).isInCall, (*decider).holdInCall},
	{"held-quiet", (*decider).isQuiet, (*decider).holdQuiet},
	{"held-away", (*decider).isAway, (*decider).holdAway},
	{"unreachable", (*decider).isUnreachable, (*decider).holdUnreachable},
	{"push-failing", (*decider).isPushFailing, (*decider).onPage},
	{"current", func(*decider, *v1alpha1.Message) bool { return true }, (*decider).onPage},
}

// Decide judges every Message, then every phone's screen. Rows, per
// Message, first match wins:
//
//  1. finished-retained: a final state whose finishedAt is RetainFinished
//     old: deleted;
//  2. final: Expired, Withdrawn, Replaced or Failed: kept as it is (a
//     missing finishedAt is set to now, so it is deleted in time);
//  3. expired: now is at or past expiresAt: Expired (never pushed, even
//     when it arrived expired);
//  4. withdrawn: spec.withdrawn: Withdrawn;
//  5. replaced: a newer Message (creationTimestamp, then name) for the
//     same phone with the same non-empty key, not withdrawn, in any state:
//     Replaced;
//  6. phone-missing: no such Phone: Failed PhoneMissing;
//  7. push-unsupported: its model does not take pushes, or its driver
//     renders no page: Failed PushUnsupported;
//
// and, only for a Message never shown (shownAt unset),
//
//  8. held-in-call: a person call is up on the phone, ringing included
//     (a Stream call is not one), urgent or not: Held InCall;
//  9. held-quiet: not urgent, in quiet hours: Held QuietHours;
//  10. held-away: not urgent, nobody home: Held Away;
//  11. unreachable: the agent or the phone cannot be reached: Held
//     PhoneUnreachable;
//  12. push-failing: the last push to the phone failed: Held PushFailing
//     with the failure, and on the page the next push retries;
//  13. current: on the page.
//
// Then, for each phone some Message names (a phone none names is left
// alone, so a test push stays), the page: its current Messages (rows 12
// and 13), newest first, rendered in Location. One already shown stays
// Shown; when the phone holds the page (PhoneView.onPhone), the rest are
// Shown too (shownAt now). Otherwise, when a push may go out (no person
// call, reachable, not waiting PushRetry after a failure), the page is
// pushed, with the chime when one of them was never shown; their statuses
// wait for the push (the shell decides again with its result). With no
// current Message, a phone that holds a page is cleared; in a person call
// nothing at all is pushed. A page that does not render holds its unshown
// Messages PushFailing.
//
// RequeueAfter is the earliest expiry or deletion to come, at most
// PushRetry while a Message waits for an unreachable phone or a failed
// push, and the end of a phone's retry wait.
//
// It is pure: no I/O, no clock but Now.
func Decide(in Input) Verdict {
	d := newDecider(in)
	for _, m := range d.msgs {
		for _, r := range rules {
			if r.match(d, m) {
				r.outcome(d, m)
				break
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(d.named)) {
		d.screen(name)
	}
	return d.verdict()
}

// decider is one decision under way.
type decider struct {
	in  Input
	now metav1.Time // in.Now as status keeps it

	// msgs are the Messages, newest first.
	msgs []*v1alpha1.Message
	// newest is the newest Message not withdrawn, per phone and key.
	newest map[phoneKey]*v1alpha1.Message
	// named are the phones some Message names.
	named map[string]bool
	// page holds each phone's current Messages, newest first, and which
	// of them row 12 put there.
	page map[string][]pageLine

	status  map[string]v1alpha1.MessageStatus // decided, by name
	deleted []string
	pushes  []PagePush
}

type phoneKey struct{ phone, key string }

type pageLine struct {
	m       *v1alpha1.Message
	failing bool
}

func newDecider(in Input) *decider {
	d := &decider{
		in: in, now: statusTime(in.Now),
		newest: map[phoneKey]*v1alpha1.Message{}, named: map[string]bool{}, page: map[string][]pageLine{},
		status: map[string]v1alpha1.MessageStatus{},
	}
	for i := range in.Messages {
		d.msgs = append(d.msgs, &in.Messages[i])
	}
	slices.SortFunc(d.msgs, func(a, b *v1alpha1.Message) int {
		if newer(a, b) {
			return -1
		}
		if newer(b, a) {
			return 1
		}
		return 0
	})
	for _, m := range d.msgs {
		d.named[string(m.Spec.Phone)] = true
		if m.Spec.Key == "" || m.Spec.Withdrawn {
			continue
		}
		k := phoneKey{string(m.Spec.Phone), m.Spec.Key}
		if _, ok := d.newest[k]; !ok { // msgs is newest first
			d.newest[k] = m
		}
	}
	return d
}

// newer reports whether a was created after b: by creationTimestamp, then
// by name.
func newer(a, b *v1alpha1.Message) bool {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c > 0
	}
	return a.Name > b.Name
}

// statusTime is t as status keeps it: UTC, whole seconds.
func statusTime(t time.Time) metav1.Time { return metav1.NewTime(t.UTC().Truncate(time.Second)) }

func (d *decider) phone(m *v1alpha1.Message) (PhoneView, bool) {
	pv, ok := d.in.Phones[string(m.Spec.Phone)]
	return pv, ok && pv.Exists
}

func unshown(m *v1alpha1.Message) bool { return m.Status.ShownAt == nil }

// The rows' predicates.

func (d *decider) isRetained(m *v1alpha1.Message) bool {
	at := m.Status.FinishedAt
	return final(m.Status.State) && at != nil && !d.in.Now.Before(at.Add(RetainFinished))
}

func (d *decider) isFinal(m *v1alpha1.Message) bool { return final(m.Status.State) }

func (d *decider) isExpired(m *v1alpha1.Message) bool { return !d.in.Now.Before(m.Spec.ExpiresAt.Time) }

func (d *decider) isWithdrawn(m *v1alpha1.Message) bool { return m.Spec.Withdrawn }

func (d *decider) isReplaced(m *v1alpha1.Message) bool { return d.replacement(m) != nil }

// replacement is the newer Message that replaces m, or nil.
func (d *decider) replacement(m *v1alpha1.Message) *v1alpha1.Message {
	if m.Spec.Key == "" {
		return nil
	}
	n := d.newest[phoneKey{string(m.Spec.Phone), m.Spec.Key}]
	if n == nil || !newer(n, m) {
		return nil
	}
	return n
}

func (d *decider) isPhoneMissing(m *v1alpha1.Message) bool {
	_, ok := d.phone(m)
	return !ok
}

func (d *decider) isPushUnsupported(m *v1alpha1.Message) bool {
	pv, _ := d.phone(m)
	return !pv.pushable()
}

func (d *decider) isInCall(m *v1alpha1.Message) bool {
	pv, _ := d.phone(m)
	return unshown(m) && pv.PersonCalls > 0
}

func (d *decider) isQuiet(m *v1alpha1.Message) bool {
	return unshown(m) && !m.Spec.Urgent && d.in.House.Quiet
}

func (d *decider) isAway(m *v1alpha1.Message) bool {
	return unshown(m) && !m.Spec.Urgent && d.in.House.Away
}

func (d *decider) isUnreachable(m *v1alpha1.Message) bool {
	pv, _ := d.phone(m)
	return unshown(m) && !pv.Reachable
}

func (d *decider) isPushFailing(m *v1alpha1.Message) bool {
	pv, _ := d.phone(m)
	return unshown(m) && pv.PushFailure != ""
}
