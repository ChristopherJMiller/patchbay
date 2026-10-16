package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StreamSpec is the intended state of a dialable live-audio stream
// (release B, design §9).
type StreamSpec struct {
	// Number is the stream's extension, in the streams range of the number
	// plan (7XX by default).
	// +required
	Number Number `json:"number"`

	// Name is the stream's display name. No control characters (line
	// breaks included); anything else is escaped wherever it is rendered.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F-\x9F]*$`
	Name string `json:"name"`

	// Source is the live audio the stream bridge plays: an http or https
	// URL of a stream its decoder (ffmpeg) reads directly, for example an
	// HDHomeRun channel (http://192.168.0.58:5004/auto/v5.1) or an Icecast
	// mount (https://ice2.somafm.com/groovesalad-128-mp3). Not a playlist:
	// put one of a .pls or .m3u file's stream URLs here (an HLS .m3u8 is a
	// stream, and is fine). No credentials: the source is shown in status,
	// Events and logs. A source whose host is one of a tuner pool's hosts
	// (settings streams.tunerPools) counts against that pool's limit. The
	// controller never probes it: a probe would hold a tuner.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^[^\s\x00-\x1F\x7F]+$`
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() != '' && url(self).getHost() != ''",message="source must be an absolute URL with a scheme and host"
	// +kubebuilder:validation:XValidation:rule="!isURL(self) || url(self).getScheme() in ['http', 'https']",message="source must be an http or https URL: the stream bridge reads only those"
	// +kubebuilder:validation:XValidation:rule="!self.matches('^[A-Za-z][A-Za-z0-9+.-]*://[^/?#]*@')",message="source must not carry credentials (user@ or user:password@): it is shown in status, Events and logs"
	// +kubebuilder:validation:XValidation:rule="!self.matches('(?i)^[a-z][a-z0-9+.-]*://[^/?#]*/[^?#]*[.](pls|m3u)([?#].*)?$')",message="source is a playlist (.pls or .m3u): use one of the stream URLs it lists"
	Source string `json:"source"`
}

// StreamStatus is written only by the controller, from the compiled
// bundle and the stream bridge's observations (it never probes a source).
type StreamStatus struct {
	// State is a one-word summary for humans and `kubectl get` (Idle,
	// Playing, Busy, Failing, Pending, Unavailable, Invalid, Unknown). It
	// is not a contract; tools read Conditions.
	// +optional
	State string `json:"state,omitempty"`

	// ObservedGeneration is the .metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready (True when dialling it now should play: reason
	// Idle or Playing; otherwise TunerBusy, SourceFailed, InvalidSource,
	// BridgePending, NoStreamBridge, NumberConflict or NumberNotInPlan;
	// Unknown with BridgeUnreachable) and NumberConflict.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Listeners is how many calls are playing it, as of the controller's
	// last look at the stream bridge.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Listeners int32 `json:"listeners,omitempty"`

	// TunerPool is the tuner pool its source counts against (settings
	// streams.tunerPools, matched by the source's host), or empty when it
	// needs no tuner.
	// +optional
	TunerPool string `json:"tunerPool,omitempty"`

	// LastPlayed is when a call on it last began to hear its source.
	// +optional
	LastPlayed *metav1.Time `json:"lastPlayed,omitempty"`

	// LastFailure is its most recent failure: its source would not start or
	// stopped, or a call was refused because its tuner pool was busy. It is
	// kept across stream bridge restarts; a later LastPlayed means the
	// stream works again.
	// +optional
	LastFailure *StreamFailure `json:"lastFailure,omitempty"`
}

// StreamFailureReason says what went wrong with a stream.
//
// +kubebuilder:validation:Enum=SourceFailed;SourceEnded;TunerBusy
type StreamFailureReason string

// Stream failure reasons (the stream bridge's, streambridge/api).
const (
	// StreamFailureSourceFailed: the source would not start (the HDHomeRun
	// refused because every tuner was busy, the host did not answer, no
	// audio came); the caller heard "not available".
	StreamFailureSourceFailed StreamFailureReason = "SourceFailed"
	// StreamFailureSourceEnded: the source stopped while calls heard it;
	// they heard "not available".
	StreamFailureSourceEnded StreamFailureReason = "SourceEnded"
	// StreamFailureTunerBusy: a call was refused because the stream's tuner
	// pool was playing as many other sources as it may.
	StreamFailureTunerBusy StreamFailureReason = "TunerBusy"
)

// StreamFailure is one failure of a stream.
type StreamFailure struct {
	// At is when it happened.
	// +required
	At metav1.Time `json:"at"`

	// Reason is what happened.
	// +required
	Reason StreamFailureReason `json:"reason"`

	// Message says it for a person (the decoder's error, or what held the
	// tuner pool).
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Message string `json:"message,omitempty"`
}

// Stream is a number that plays live audio from a source URL, holding the
// source (a tuner) only while someone listens. Release B.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=patchbay
// +kubebuilder:printcolumn:name="Number",type=string,JSONPath=`.spec.number`
// +kubebuilder:printcolumn:name="Display",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Listeners",type=integer,JSONPath=`.status.listeners`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.status.tunerPool`,priority=1
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Stream struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec StreamSpec `json:"spec"`
	// +optional
	Status StreamStatus `json:"status,omitempty"`
}

// StreamList is a list of Streams.
//
// +kubebuilder:object:root=true
type StreamList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Stream `json:"items"`
}
