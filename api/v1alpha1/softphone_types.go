package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SoftphoneSpec is the intended state of a software SIP client (Linphone).
type SoftphoneSpec struct {
	// Number is the softphone's own extension, in the device range of the
	// number plan (1XX by default).
	// +required
	Number Number `json:"number"`

	// Label is the softphone's display and caller ID name. Defaults to the
	// resource name when empty.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Label string `json:"label,omitempty"`
}

// SoftphoneObserved is what patchbay has seen the softphone do, copied from
// the agent's disposable memory by the controller (design §8.1).
type SoftphoneObserved struct {
	// FirstSeen is when the softphone was first seen fetching its
	// provisioning document or registering.
	// +optional
	FirstSeen *metav1.Time `json:"firstSeen,omitempty"`

	// Address is the IP address the softphone last fetched its provisioning
	// document or registered from.
	// +optional
	Address string `json:"address,omitempty"`

	// LastConfigFetch is the most recent fetch of the softphone's
	// provisioning document.
	// +optional
	LastConfigFetch *ConfigFetch `json:"lastConfigFetch,omitempty"`

	// LastRegistered is the last time the softphone was seen holding a SIP
	// registration. A softphone with this set and no contact now is Offline
	// (asleep), not broken.
	// +optional
	LastRegistered *metav1.Time `json:"lastRegistered,omitempty"`
}

// SoftphoneStatus is written only by the controller.
type SoftphoneStatus struct {
	// State is a one-word summary for humans and `kubectl get`. It is not a
	// contract; tools read Conditions.
	// +optional
	State string `json:"state,omitempty"`

	// ObservedGeneration is the .metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ProvisioningURL is the one-time Linphone remote-provisioning URL. The
	// token in it is spent on first use; `patchbay softphone qr --reissue`
	// asks for a new one. It is served on the LAN only.
	// +optional
	ProvisioningURL string `json:"provisioningURL,omitempty"`

	// Conditions: Ready, Provisioned, Registered, ConfigCurrent, and
	// NumberConflict when it applies.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Observed is what the softphone has been seen doing.
	// +optional
	Observed *SoftphoneObserved `json:"observed,omitempty"`

	// LastAction is the most recent action taken on the softphone.
	// +optional
	LastAction *LastAction `json:"lastAction,omitempty"`

	// HumanAction is the exact instruction for a person, when the softphone
	// cannot make progress without one. Empty when nothing is needed.
	// +optional
	HumanAction string `json:"humanAction,omitempty"`
}

// Softphone is a SIP client on a laptop or handset, provisioned by scanning a
// QR code. Adding a softphone is creating one of these.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=patchbay
// +kubebuilder:printcolumn:name="Number",type=string,JSONPath=`.spec.number`
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Softphone struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec SoftphoneSpec `json:"spec"`
	// +optional
	Status SoftphoneStatus `json:"status,omitempty"`
}

// SoftphoneList is a list of Softphones.
//
// +kubebuilder:object:root=true
type SoftphoneList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Softphone `json:"items"`
}
