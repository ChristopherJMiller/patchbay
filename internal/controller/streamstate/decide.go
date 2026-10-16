// Package streamstate is the Stream reconciler of release B
// (docs/plans/release-b.md RT.5, §2(n)): a leader-only poller of the
// stream bridge's observations (internal/controller/bridgeclient), and the
// reconciler that writes each Stream's status from the number table, the
// compiled streams (<fullname>-streams, install.ReadStreams) and the
// poller's latest snapshot.
//
// The state is a pure rule table, Decide; everything that reads or writes
// the cluster or the bridge is in the reconciler and the poller. Ready is
// True when dialling the stream now should play. The bridge's memory is
// disposable: when a stream last played and its last failure are copied
// into status (the fold), so they survive a bridge restart, and each new
// failure is an Event once.
package streamstate

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// Display states (status.state) of a Stream. Not a contract: tools read
// the conditions.
const (
	StateIdle        = "Idle"
	StatePlaying     = "Playing"
	StateBusy        = "Busy"
	StateFailing     = "Failing"
	StatePending     = "Pending"
	StateUnavailable = "Unavailable"
	StateInvalid     = "Invalid"
	StateUnknown     = "Unknown"
)

// Snapshot is what the controller knows of the stream bridge after a poll.
type Snapshot struct {
	// Document is the last document the bridge returned; the zero value
	// before the first success. When the latest poll failed it is the
	// previous good one (Reachable says so).
	Document bridgeapi.Observations
	// PolledAt is when Document was fetched, by the controller's clock;
	// zero before the first success.
	PolledAt time.Time
	// Reachable is whether the latest poll succeeded.
	Reachable bool
	// Polls counts the poll attempts that have ended. Zero: the first is
	// still running, and no Stream is judged on the bridge yet.
	Polls uint64
	// LastError is why the latest poll failed; empty after a success.
	LastError string
}

// Started reports whether the first poll attempt has ended.
func (s Snapshot) Started() bool { return s.Polls > 0 }

// Input is everything Decide judges a Stream from.
type Input struct {
	// Stream is the Stream; its status is the memory (what the fold keeps).
	Stream *v1alpha1.Stream
	// Claim is why it holds no number (compile.ResolveClaims over every
	// resource), or nil when it holds it.
	Claim *compile.ClaimProblem
	// Settings are the install's settings (the stream bridge).
	Settings compile.Settings
	// Compiled is the compiled streams.json (<fullname>-streams).
	Compiled streams.Document
	// Snapshot is the poller's latest.
	Snapshot Snapshot
	// StaleAfter is how old Snapshot.PolledAt may be before the bridge
	// counts as unreachable (install.StaleAfter): now - PolledAt >
	// StaleAfter.
	StaleAfter time.Duration
	// Now is the controller's clock.
	Now time.Time
}

// Event is one Event to emit about the Stream once its status is written.
type Event struct {
	Type   string // corev1.EventTypeNormal or EventTypeWarning
	Reason string
	Action string
	Note   string
}

// Verdict is a Stream's judged status.
type Verdict struct {
	// Wait: write nothing yet. The bridge has not been polled once since
	// the controller started, so rows 4-9 cannot be judged (a restarted
	// controller never flips every Stream for one poll).
	Wait bool

	// State is status.state; Ready and NumberConflict its conditions,
	// without generation or transition time (the reconciler sets those).
	State          string
	Ready          metav1.Condition
	NumberConflict metav1.Condition

	// The status fields after the fold (rows 5-9), or the Stream's own
	// (rows 1-4).
	Listeners   int32
	TunerPool   string
	LastPlayed  *metav1.Time
	LastFailure *v1alpha1.StreamFailure

	// Events are what to emit after the write: StateChanged when the state
	// changed, and SourceFailed or CallRefused for a failure the fold copied
	// now (once per failure).
	Events []Event
}

// Decide judges a Stream (docs/plans/release-b.md RT.5). Rows, first match
// wins:
//
//  1. Invalid: it holds no number (NumberConflict, NumberNotInPlan);
//  2. Invalid: streams.CheckSource refuses its source (InvalidSource);
//  3. Unavailable: the install has no stream bridge (NoStreamBridge);
//
// then, if the bridge has not been polled once, Wait;
//
//  4. Unknown: the latest poll failed, or the last good one is older than
//     StaleAfter (Ready Unknown, BridgeUnreachable); the status keeps
//     listeners, the pool, lastPlayed and lastFailure;
//
// then the observation is folded into the status (listeners, the compiled
// pool, the later lastPlayed, a newer lastFailure);
//
//  5. Pending: it is not compiled as it is now, or the bridge's loaded
//     streams.json lacks it or has it with another source or pool
//     (BridgePending);
//  6. Playing: it has listeners (Ready True);
//  7. Failing: its last failure is SourceFailed or SourceEnded and newer
//     than lastPlayed (SourceFailed);
//  8. Busy: its pool runs Max sessions with listeners, none on its source
//     (TunerBusy); a TunerBusy failure alone never makes it Failing, nor
//     replaces the source failure row 7 holds;
//  9. Idle (Ready True).
//
// It is pure: no I/O, no clock but Now.
func Decide(in Input) Verdict {
	st := in.Stream
	number := string(st.Spec.Number)
	v := Verdict{
		NumberConflict: condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, ""),
		Listeners:      st.Status.Listeners,
		TunerPool:      st.Status.TunerPool,
		LastPlayed:     st.Status.LastPlayed.DeepCopy(),
		LastFailure:    st.Status.LastFailure.DeepCopy(),
	}
	switch {
	case in.Claim != nil && in.Claim.NumberConflict:
		v.set(StateInvalid, metav1.ConditionFalse, v1alpha1.ReasonNumberConflict, in.Claim.Message)
		v.NumberConflict = condition(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, v1alpha1.ReasonNumberConflict, in.Claim.Message)
		return v.withEvents(st)
	case in.Claim != nil: // out of the plan: the only other problem a Stream can have
		v.set(StateInvalid, metav1.ConditionFalse, v1alpha1.ReasonNumberNotInPlan, in.Claim.Message)
		return v.withEvents(st)
	}
	if err := streams.CheckSource(st.Spec.Source); err != nil {
		v.set(StateInvalid, metav1.ConditionFalse, v1alpha1.ReasonInvalidSource, strings.TrimPrefix(err.Error(), "streams: "))
		return v.withEvents(st)
	}
	if in.Settings.StreamBridge == "" {
		v.set(StateUnavailable, metav1.ConditionFalse, v1alpha1.ReasonNoStreamBridge,
			fmt.Sprintf(`streams are disabled (streamBridge.enabled is false): dialling %s plays "not available"`, number))
		return v.withEvents(st)
	}
	snap := in.Snapshot
	if !snap.Started() {
		return Verdict{Wait: true}
	}
	if age := in.Now.Sub(snap.PolledAt); !snap.Reachable || age > in.StaleAfter {
		why := snap.LastError
		if why == "" {
			why = fmt.Sprintf("its last observations are %s old", age.Truncate(time.Second))
		}
		v.set(StateUnknown, metav1.ConditionUnknown, v1alpha1.ReasonBridgeUnreachable, "cannot reach the stream bridge: "+why)
		return v.withEvents(st)
	}

	id := streams.UUID(st.Name)
	compiled, isCompiled := in.Compiled.Streams[id]
	obs := observation(snap.Document, id)
	newFailure := v.fold(compiled.Pool, obs)

	switch {
	case !isCompiled || compiled.Source != st.Spec.Source:
		v.set(StatePending, metav1.ConditionFalse, v1alpha1.ReasonBridgePending,
			fmt.Sprintf("this stream is not compiled yet (compiled streams %s)", short(in.Compiled.Version)))
	case obs == nil || obs.Source != compiled.Source || obs.Pool != compiled.Pool:
		v.set(StatePending, metav1.ConditionFalse, v1alpha1.ReasonBridgePending,
			fmt.Sprintf("the stream bridge has not loaded this stream yet (it plays streams %s, compiled %s)",
				short(snap.Document.Bridge.StreamsVersion), short(in.Compiled.Version)))
	case v.Listeners > 0:
		v.set(StatePlaying, metav1.ConditionTrue, v1alpha1.ReasonPlaying, listeners(v.Listeners)+inPool(compiled.Pool))
	case failing(v.LastFailure, v.LastPlayed):
		v.set(StateFailing, metav1.ConditionFalse, v1alpha1.ReasonSourceFailed,
			v.LastFailure.At.UTC().Format(time.RFC3339)+": "+v.LastFailure.Message)
	default:
		if holders, limit, busy := busyPool(snap.Document, compiled); busy {
			v.set(StateBusy, metav1.ConditionFalse, v1alpha1.ReasonTunerBusy,
				fmt.Sprintf("pool %s (%d at a time) is playing %s", compiled.Pool, limit, holders))
			break
		}
		msg := "nobody is listening"
		if v.LastPlayed != nil {
			msg += "; last played " + v.LastPlayed.UTC().Format(time.RFC3339)
		}
		v.set(StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, msg)
	}
	v = v.withEvents(st)
	if newFailure {
		v.Events = append(v.Events, failureEvent(v.LastFailure))
	}
	return v
}

func condition(typ string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: status, Reason: reason, Message: install.Text(message, install.MaxStatusMessage)}
}

// set sets the state and Ready.
func (v *Verdict) set(state string, status metav1.ConditionStatus, reason, message string) {
	v.State = state
	v.Ready = condition(v1alpha1.ConditionReady, status, reason, message)
}

// withEvents adds StateChanged when the state changes ("<old> -> <new>:
// <Ready's message>", old "" as "None").
func (v Verdict) withEvents(st *v1alpha1.Stream) Verdict {
	if before := st.Status.State; before != v.State {
		if before == "" {
			before = "None"
		}
		v.Events = append(v.Events, Event{
			Type: corev1.EventTypeNormal, Reason: install.EventReasonStateChanged, Action: install.EventActionDecide,
			Note: install.EventNote(before + " -> " + v.State + ": " + v.Ready.Message),
		})
	}
	return v
}

// observation is the bridge's entry for the stream with UUID id, or nil.
func observation(doc bridgeapi.Observations, id string) *bridgeapi.StreamObservations {
	for i := range doc.Streams {
		if doc.Streams[i].UUID == id {
			return &doc.Streams[i]
		}
	}
	return nil
}

// fold copies the observation into the status fields: listeners (none
// without an observation), the compiled pool, the later lastPlayed, and
// the observation's lastFailure when it is newer than the status's,
// except a TunerBusy refusal while the status holds a source failure no
// call has played past (the stream stays Failing; the bridge keeps that
// failure too: docs/plans/release-b.md §13). It reports whether that
// failure is new. Times are compared as status keeps
// them, to the second (metav1.Time's precision), so a failure is copied,
// and its Event emitted, once.
func (v *Verdict) fold(pool string, obs *bridgeapi.StreamObservations) (newFailure bool) {
	v.TunerPool = pool
	v.Listeners = 0
	if obs == nil {
		return false
	}
	v.Listeners = int32(max(0, obs.Listeners))
	if obs.LastPlayed != nil {
		if at := statusTime(*obs.LastPlayed); v.LastPlayed == nil || at.After(v.LastPlayed.Time) {
			v.LastPlayed = &at
		}
	}
	f := obs.LastFailure
	if f == nil {
		return false
	}
	reason, known := failureReason(f.Reason)
	at := statusTime(f.At)
	if !known || (v.LastFailure != nil && !at.After(v.LastFailure.At.Time)) {
		return false
	}
	if reason == v1alpha1.StreamFailureTunerBusy && failing(v.LastFailure, v.LastPlayed) {
		return false // a refusal never hides a broken source that no call has played since
	}
	v.LastFailure = &v1alpha1.StreamFailure{At: at, Reason: reason, Message: install.Text(f.Message, install.MaxStatusMessage)}
	return true
}

// statusTime is t as status keeps it: UTC, whole seconds.
func statusTime(t time.Time) metav1.Time { return metav1.NewTime(t.UTC().Truncate(time.Second)) }

// failureReason is the status reason of a bridge failure reason; false for
// one this controller does not know (a newer bridge), which the CRD's enum
// would refuse.
func failureReason(r bridgeapi.FailureReason) (v1alpha1.StreamFailureReason, bool) {
	switch r {
	case bridgeapi.FailureSourceFailed:
		return v1alpha1.StreamFailureSourceFailed, true
	case bridgeapi.FailureSourceEnded:
		return v1alpha1.StreamFailureSourceEnded, true
	case bridgeapi.FailureTunerBusy:
		return v1alpha1.StreamFailureTunerBusy, true
	}
	return "", false
}

// failing reports whether the last failure is the source's, and newer than
// the last play.
func failing(f *v1alpha1.StreamFailure, played *metav1.Time) bool {
	if f == nil {
		return false
	}
	switch f.Reason {
	case v1alpha1.StreamFailureSourceFailed, v1alpha1.StreamFailureSourceEnded:
		return played == nil || f.At.After(played.Time)
	case v1alpha1.StreamFailureTunerBusy:
		return false
	}
	return false
}

// busyPool reports whether the stream's pool runs Max sessions with
// listeners, none of them on its source, and names what they play:
// "kiro7 (707)", the streams of those sessions, sorted, with their numbers.
func busyPool(doc bridgeapi.Observations, s streams.Stream) (holders string, limit int, busy bool) {
	if s.Pool == "" {
		return "", 0, false
	}
	i := slices.IndexFunc(doc.Pools, func(p bridgeapi.PoolObservations) bool { return p.Name == s.Pool })
	if i < 0 {
		return "", 0, false
	}
	pool := doc.Pools[i]
	var names []string
	held := 0
	for _, sess := range pool.Sessions {
		if sess.Listeners <= 0 {
			continue // Draining: it gives way to a new call
		}
		if sess.Source == s.Source {
			return "", pool.Max, false // a call joins it
		}
		held++
		names = append(names, sess.Streams...)
	}
	if held < pool.Max {
		return "", pool.Max, false
	}
	numbers := map[string]string{}
	for _, o := range doc.Streams {
		numbers[o.Name] = o.Number
	}
	slices.SortFunc(names, cmp.Compare)
	names = slices.Compact(names)
	for i, n := range names {
		if num := numbers[n]; num != "" {
			names[i] = n + " (" + num + ")"
		}
	}
	return strings.Join(names, ", "), pool.Max, true
}

// failureEvent is the Event of a failure the fold copied: SourceFailed
// (Warning, "<reason>: <message>") for the source's, CallRefused (Normal,
// the message, which names what held the pool) for TunerBusy
// (install.EventReasonSourceFailed, EventReasonCallRefused).
func failureEvent(f *v1alpha1.StreamFailure) Event {
	switch f.Reason {
	case v1alpha1.StreamFailureTunerBusy:
		return Event{Type: corev1.EventTypeNormal, Reason: install.EventReasonCallRefused, Action: install.EventActionPlay,
			Note: install.EventNote(f.Message)}
	case v1alpha1.StreamFailureSourceFailed, v1alpha1.StreamFailureSourceEnded:
	}
	return Event{Type: corev1.EventTypeWarning, Reason: install.EventReasonSourceFailed, Action: install.EventActionPlay,
		Note: install.EventNote(string(f.Reason) + ": " + f.Message)}
}

func listeners(n int32) string {
	if n == 1 {
		return "1 listener"
	}
	return fmt.Sprintf("%d listeners", n)
}

func inPool(pool string) string {
	if pool == "" {
		return ""
	}
	return "; tuner pool " + pool
}

// short is a version's first 12 digits, or "(none)".
func short(v string) string {
	switch {
	case v == "":
		return "(none)"
	case len(v) > 12:
		return v[:12]
	}
	return v
}
