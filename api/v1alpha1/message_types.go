package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MessageSpec is one text bound for one desk phone. The endpoint and the
// CLI fan a target list out into one Message per phone. Every field but
// Withdrawn is immutable; Withdrawn may only go from false to true.
//
// +kubebuilder:validation:XValidation:rule="self.phone == oldSelf.phone",message="phone is immutable"
// +kubebuilder:validation:XValidation:rule="self.title == oldSelf.title",message="title is immutable"
// +kubebuilder:validation:XValidation:rule="self.text == oldSelf.text",message="text is immutable"
// +kubebuilder:validation:XValidation:rule="self.urgent == oldSelf.urgent",message="urgent is immutable"
// +kubebuilder:validation:XValidation:rule="self.expiresAt == oldSelf.expiresAt",message="expiresAt is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.key) == has(oldSelf.key) && (!has(self.key) || self.key == oldSelf.key)",message="key is immutable"
// +kubebuilder:validation:XValidation:rule="!oldSelf.withdrawn || self.withdrawn",message="withdrawn can only change from false to true"
type MessageSpec struct {
	// Phone is the Phone (same namespace) the text is for.
	// +required
	Phone LocalName `json:"phone"`

	// Title is the heading shown above the text. No control characters.
	// +optional
	// +kubebuilder:default=""
	// +kubebuilder:validation:MinLength=0
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F-\x9F]*$`
	Title string `json:"title"`

	// Text is the message itself. No control characters.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F-\x9F]*$`
	Text string `json:"text"`

	// Urgent messages are shown even when the phone is in a call, in quiet
	// hours or away.
	// +optional
	// +kubebuilder:default=false
	Urgent bool `json:"urgent"`

	// ExpiresAt is when the message stops being worth showing.
	// +required
	ExpiresAt metav1.Time `json:"expiresAt"`

	// Key makes a newer Message with the same key (for the same phone)
	// replace this one.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Key string `json:"key,omitempty"`

	// Withdrawn takes the message back. It can only be set, never cleared.
	// +optional
	// +kubebuilder:default=false
	Withdrawn bool `json:"withdrawn"`
}

// MessageState is what became of a Message.
//
// +kubebuilder:validation:Enum=Held;Shown;Expired;Withdrawn;Replaced;Failed
type MessageState string

// Message states (the controller's decision, written to status).
const (
	// MessageStateHeld: not shown yet; Reason says why.
	MessageStateHeld MessageState = "Held"
	// MessageStateShown: pushed to the phone.
	MessageStateShown MessageState = "Shown"
	// MessageStateExpired: ExpiresAt passed before (or after) it was shown.
	MessageStateExpired MessageState = "Expired"
	// MessageStateWithdrawn: spec.withdrawn was set.
	MessageStateWithdrawn MessageState = "Withdrawn"
	// MessageStateReplaced: a newer Message with the same key took over.
	MessageStateReplaced MessageState = "Replaced"
	// MessageStateFailed: it can never be shown.
	MessageStateFailed MessageState = "Failed"
)

// Reasons a Message is Held or Failed.
const (
	ReasonInCall           = "InCall"
	ReasonQuietHours       = "QuietHours"
	ReasonAway             = "Away"
	ReasonPhoneMissing     = "PhoneMissing"
	ReasonPushUnsupported  = "PushUnsupported"
	ReasonPushFailing      = "PushFailing"
	ReasonPhoneUnreachable = "PhoneUnreachable"
)

// MessageStatus is written only by the controller.
type MessageStatus struct {
	// State is the Message's fate so far.
	// +optional
	State MessageState `json:"state,omitempty"`

	// Reason is why it is Held or Failed (InCall, QuietHours, Away,
	// PhoneMissing, PushUnsupported, PushFailing, PhoneUnreachable).
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message says it for a person.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Message string `json:"message,omitempty"`

	// ShownAt is when the phone was told to show it.
	// +optional
	ShownAt *metav1.Time `json:"shownAt,omitempty"`

	// FinishedAt is when it reached a final state.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// ObservedGeneration is the .metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Message is one text for one desk phone.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=patchbay
// +kubebuilder:printcolumn:name="Phone",type=string,JSONPath=`.spec.phone`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Key",type=string,JSONPath=`.spec.key`
// +kubebuilder:printcolumn:name="Urgent",type=boolean,JSONPath=`.spec.urgent`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Message struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec MessageSpec `json:"spec"`
	// +optional
	Status MessageStatus `json:"status,omitempty"`
}

// MessageList is a list of Messages.
//
// +kubebuilder:object:root=true
type MessageList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Message `json:"items"`
}
