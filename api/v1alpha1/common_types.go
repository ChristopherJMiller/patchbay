package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Number is a dialable number: one or more ASCII digits, written as a string.
// Quote it in YAML (`number: "100"`). A bare `number: 100` is rejected by the
// API server ("must be of type string"). A string keeps leading zeros, and
// avoids YAML 1.1 reading `number: 010` as octal 8 (design §4).
//
// The schema checks only that it is digits. Which range means what (1XX
// devices, 3XX people, 7XX streams, ...) is the number plan, which lives in
// chart values and is enforced by the controller (design §4). When two
// resources claim one number, the older one keeps it and the newer one gets
// NumberConflict=True.
//
// +kubebuilder:validation:Pattern=`^[0-9]+$`
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=16
type Number string

// MAC is an Ethernet address written as exactly 12 hexadecimal digits with no
// separators, for example D0C78915E8C8 (the phone's sticker, or the name
// after "SEP" on its settings screen).
//
// Case is not significant: d0c78915e8c8 and D0C78915E8C8 are the same phone.
// patchbay always uses the upper-case form internally and on the wire, because
// that is how Cisco names the phone's files (SEPD0C78915E8C8.cnf.xml). See
// Normalized.
//
// +kubebuilder:validation:Pattern=`^[0-9A-Fa-f]{12}$`
type MAC string

// Normalized returns the canonical upper-case form of the address. It does
// not validate; the CRD schema already has.
func (m MAC) Normalized() string {
	return strings.ToUpper(string(m))
}

// LocalName is the name of another resource in the same namespace.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=253
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
type LocalName string

// Condition types written by the controller (design §8.1). Each kind uses the
// subset that applies to it. Tools read these; status.state is display only.
const (
	// ConditionReady is the summary: True when the device needs nothing
	// from anyone. A softphone that is only asleep (Offline) is still Ready.
	ConditionReady = "Ready"
	// ConditionFirmwareAvailable is True when every file of the device's
	// target load is staged on the firmware volume with the SHA-256 the
	// driver manifest pins.
	ConditionFirmwareAvailable = "FirmwareAvailable"
	// ConditionFirmwareCurrent is True when the device runs its target load
	// (spec.firmware, or the driver default).
	ConditionFirmwareCurrent = "FirmwareCurrent"
	// ConditionProvisioned is True once the device has fetched the config
	// patchbay currently renders for it.
	ConditionProvisioned = "Provisioned"
	// ConditionRegistered is True while the device holds a live SIP
	// registration with Asterisk.
	ConditionRegistered = "Registered"
	// ConditionConfigCurrent is True when the device runs the config version
	// the controller most recently compiled for it.
	ConditionConfigCurrent = "ConfigCurrent"
	// ConditionUnsupported is True when the spec asks for something the
	// driver or the active signalling backend cannot deliver (design §8.5).
	// The feature is refused, never rendered as a dead key.
	ConditionUnsupported = "Unsupported"
	// ConditionNumberConflict is True when an older resource (by
	// creationTimestamp) already holds this resource's number. A conflicted
	// resource is not routed.
	ConditionNumberConflict = "NumberConflict"
	// ConditionMACConflict is True on a Phone when an older Phone (by
	// creationTimestamp) already has the same MAC, compared upper case. Two
	// Phones with one MAC would render the same SEP<MAC>.cnf.xml, so only the
	// oldest is rendered; the newer one is Invalid and gets no files.
	ConditionMACConflict = "MACConflict"
)

// Request annotations: how a human asks the controller for something
// (design §6, §8.4) without editing spec and without the controller ever
// writing metadata. The value is opaque, at most 256 characters (a longer one
// is ignored); `patchbay` sets the current time (RFC 3339). The controller
// acts once per distinct value: it compares the annotation with
// status.handledRequest and, having acted, records the value there. Setting
// the same value again does nothing; set a new one to ask again. Removing the
// annotation is never needed.
const (
	// AnnotationResyncRequested on a Phone asks for one resync (the phone
	// re-reads its config), outside the usual rate limit.
	// `patchbay phone resync` sets it.
	AnnotationResyncRequested = "patchbay.chrismiller.xyz/resync-requested"
	// AnnotationReissueRequested on a Softphone asks for a fresh
	// provisioning token, moving it back to Issued.
	// `patchbay softphone qr --reissue` sets it.
	AnnotationReissueRequested = "patchbay.chrismiller.xyz/reissue-requested"
)

// Condition reasons named by the design (§7, §8.4). Drivers add their own.
const (
	// ReasonAgentUnreachable means the controller cannot reach the PBX agent,
	// so registration is unknown. Calls keep working.
	ReasonAgentUnreachable = "AgentUnreachable"
	// ReasonPBXDown means the agent reports Asterisk is not running.
	ReasonPBXDown = "PBXDown"
	// ReasonApplyFailed means the agent refused or rolled back the latest
	// config version; the previous version stays live.
	ReasonApplyFailed = "ApplyFailed"
	// ReasonOffline means a softphone that was registered has no contact
	// now. Mobile apps sleep; it is not an error.
	ReasonOffline = "Offline"
)

// ActionKind names something done to a device. Only the controller decides
// actions (through a driver's Decide), and status.lastAction records them,
// which is what rate-limits them.
//
// +kubebuilder:validation:Enum=resync;restart;rotateToken
type ActionKind string

// Actions the drivers can ask for (design §5, §8.3, §8.4).
const (
	// ActionResync asks a phone to re-read its config without rebooting: a
	// Cisco "NOTIFY Event: service-control" with body action=restart, sent
	// by the agent (POST /v1/actions/resync). This is CUCM's "apply config".
	ActionResync ActionKind = "resync"
	// ActionRestart reboots a phone: the same NOTIFY with action=reset
	// (POST /v1/actions/restart).
	ActionRestart ActionKind = "restart"
	// ActionRotateToken spends a softphone's provisioning token and mints a
	// new one, so each QR code works once. The controller does this itself
	// (it owns the token Secret); the agent is not involved.
	ActionRotateToken ActionKind = "rotateToken"
)

// LastAction is the most recent action taken on a device. It is the state
// machine's memory of what it already tried: "at most once per version per 5
// minutes" and "after 2 attempts, ask a human" are decided from it.
type LastAction struct {
	// Kind is what was done.
	// +required
	Kind ActionKind `json:"kind"`

	// Version is the config version the action was meant to bring the
	// device to. Empty for actions that are not about a config version.
	// +optional
	Version string `json:"version,omitempty"`

	// At is when the action was last sent.
	// +required
	At metav1.Time `json:"at"`

	// Attempts counts how many times this kind has been sent for this
	// version, including the latest.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Attempts int32 `json:"attempts,omitempty"`
}

// ConfigFetch records the device fetching a rendered config version.
type ConfigFetch struct {
	// Version is the config version of the file the device fetched.
	// +required
	Version string `json:"version"`

	// At is when the agent served it.
	// +required
	At metav1.Time `json:"at"`
}
