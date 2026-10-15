package install

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
)

// UnknownPhoneMessage is the note of an UnknownPhone Event, in design §5's
// words: "unknown phone <MAC> requested config from <IP>".
func UnknownPhoneMessage(mac, source string) string {
	return "unknown phone " + mac + " requested config from " + source
}
