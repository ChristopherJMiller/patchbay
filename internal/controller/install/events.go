package install

import (
	"crypto/sha256"
	"encoding/hex"

	corev1 "k8s.io/api/core/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Kubernetes Events the controller emits (events.k8s.io/v1, through
// controller-runtime's Manager.GetEventRecorder(EventRecorderName), whose
// recorder is client-go's tools/events.EventRecorder; tests use
// events.NewFakeRecorder). Every Event has a reason and an action from the
// lists below, and a note that never holds a secret, a token or a
// provisioning URL.
//
// Which object an Event regards:
//   - a device's own Events (state, actions, requests, ApplyFailed): the
//     Phone or Softphone;
//   - discovery and the bundle's Events (UnknownPhone, ConfigWritten,
//     CompileFailed): the pbx-config ConfigMap, or, while it does not exist
//     yet, the Settings ConfigMap (CompileFailed only).
//
// Series: client-go's events broadcaster joins Events with the same type,
// reason, action, regarding object (its resourceVersion included) and
// related object, within six minutes of the last one, into one series:
// the first Event's count grows, its note stays, and the later notes are
// lost. So an Event whose note says something new must differ in one of
// those: a device's Events after a status write regard its new
// resourceVersion; each UnknownPhone Event names its MAC in related
// (UnknownPhoneRelated); each distinct compile error names itself in
// related (CompileFailedRelated). An ActionFailed with a new note regards
// a new resourceVersion: the status write after the one before put that
// failure in Ready. What stays one series is the same note repeated, as
// it should.

// EventRecorderName is the reporting controller of every Event.
const EventRecorderName = "patchbay-controller"

// Event reasons.
const (
	// EventReasonStateChanged (Normal): status.state changed. Note:
	// "<old> -> <new>: <Ready condition message>" (old "" is written as
	// "None").
	EventReasonStateChanged = "StateChanged"
	// EventReasonActionSent (Normal): an action went out. Note: "resync sent
	// for version <v[:12]>", "restart sent", "provisioning token rotated".
	EventReasonActionSent = "ActionSent"
	// EventReasonActionFailed (Warning): an action did not go out (the agent
	// answered Sent=false or could not be reached, or the token Secret
	// could not be written). Status keeps the previous lastAction and
	// handledRequest, and the device is decided again in 30 s. Note:
	// "<kind> for version <v[:12]> not sent: <reason>".
	EventReasonActionFailed = "ActionFailed"
	// EventReasonRequestIgnored (Normal): a request annotation was marked
	// handled without the action it asks for, because the device's state
	// does not allow it. Note: "<annotation> request <value> ignored: the
	// device is <state>".
	EventReasonRequestIgnored = "RequestIgnored"
	// EventReasonApplyFailed (Warning): the device's ConfigCurrent became
	// False with reason ApplyFailed (the agent refused or rolled back the
	// compiled bundle). Emitted on that transition only. Note: "config
	// version <v[:12]> was not applied: <agent error>".
	EventReasonApplyFailed = "ApplyFailed"
	// EventReasonUnknownPhone (Normal), on pbx-config: a MAC without a
	// Phone asked for its config. Once per MAC per agent StartedAt. Note:
	// UnknownPhoneMessage.
	EventReasonUnknownPhone = "UnknownPhone"
	// EventReasonConfigWritten (Normal), on pbx-config: a new bundle version
	// was written. Note: "version <v[:12]> written: <n> files, <d> devices".
	EventReasonConfigWritten = "ConfigWritten"
	// EventReasonCompileFailed (Warning): compiling or writing the bundle
	// failed; the previous bundle stays. Note: the error.
	EventReasonCompileFailed = "CompileFailed"

	// EventReasonSourceFailed (Warning), on a Stream (release B): the
	// controller copied a new failure of its source into
	// status.lastFailure (reason SourceFailed or SourceEnded): its callers
	// heard "not available". Once per failure (its At). Note: "<reason>:
	// <the bridge's message>".
	EventReasonSourceFailed = "SourceFailed"
	// EventReasonCallRefused (Normal), on a Stream (release B): a call was
	// refused because its tuner pool was busy (a new lastFailure with
	// reason TunerBusy). Once per refusal (its At). Note: the bridge's
	// message, which names what held the pool.
	EventReasonCallRefused = "CallRefused"

	// Phone messages. They regard the Message (a device-less object), so each
	// is its own series per Message and resourceVersion; a note that says
	// something new about one Message follows a status write.

	// EventReasonMessageShown (Normal), on a Message: pushed to its phone.
	EventReasonMessageShown = "MessageShown"
	// EventReasonMessageHeld (Normal), on a Message: not shown yet. Note: the
	// reason (InCall, QuietHours, Away).
	EventReasonMessageHeld = "MessageHeld"
	// EventReasonMessageExpired (Normal), on a Message: expiresAt passed.
	EventReasonMessageExpired = "MessageExpired"
	// EventReasonMessageWithdrawn (Normal), on a Message: spec.withdrawn was set.
	EventReasonMessageWithdrawn = "MessageWithdrawn"
	// EventReasonMessageReplaced (Normal), on a Message: a newer Message with
	// the same key took over.
	EventReasonMessageReplaced = "MessageReplaced"
	// EventReasonMessageFailed (Warning), on a Message: it can never be
	// shown. Note: the reason (PhoneMissing, PushUnsupported).
	EventReasonMessageFailed = "MessageFailed"
	// EventReasonPushFailed (Warning), on a Message: a push did not go out;
	// it is retried. Note: the agent's error.
	EventReasonPushFailed = "PushFailed"
)

// Event actions (events.k8s.io requires one: what the controller did or
// was doing).
const (
	EventActionDecide      = "Decide"
	EventActionResync      = "Resync"
	EventActionRestart     = "Restart"
	EventActionRotateToken = "RotateToken"
	EventActionDiscover    = "Discover"
	EventActionCompile     = "Compile"
	// EventActionPlay is the action of a Stream's SourceFailed and
	// CallRefused Events: a call tried to play it.
	EventActionPlay = "Play"

	// Message actions.
	EventActionPush     = "Push"
	EventActionHold     = "Hold"
	EventActionExpire   = "Expire"
	EventActionWithdraw = "Withdraw"
	EventActionReplace  = "Replace"
)

// UnknownPhoneMessage is the note of an UnknownPhone Event, in design §5's
// words: "unknown phone <MAC> requested config from <IP>".
func UnknownPhoneMessage(mac, source string) string {
	return "unknown phone " + mac + " requested config from " + source
}

// UnknownPhoneRelated is an UnknownPhone Event's related object: the Phone
// that does not exist yet, named by the MAC it would have
// (fieldPath "spec.mac{<MAC>}", no name). It keeps each MAC's Event its own:
// without it the broadcaster would join every UnknownPhone on one
// pbx-config into one series with the first phone's note.
func UnknownPhoneRelated(namespace, mac string) *corev1.ObjectReference {
	return &corev1.ObjectReference{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: string(drivers.KindPhone), Namespace: namespace,
		FieldPath: "spec.mac{" + mac + "}",
	}
}

// CompileFailedRelated is a CompileFailed Event's related object: the
// ConfigMap the Event regards, with fieldPath "error{<the note's SHA-256,
// 16 hex digits>}". It keeps each distinct error its own Event: without it
// the broadcaster joined every CompileFailed on an unchanged pbx-config
// into the first error's series (the 5 min retry keeps a series alive),
// which kept that first note while a new error persisted. The same error
// repeated still joins its own series.
func CompileFailedRelated(cm *corev1.ConfigMap, note string) *corev1.ObjectReference {
	sum := sha256.Sum256([]byte(note))
	return &corev1.ObjectReference{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: cm.Namespace, Name: cm.Name, UID: cm.UID,
		FieldPath: "error{" + hex.EncodeToString(sum[:8]) + "}",
	}
}
