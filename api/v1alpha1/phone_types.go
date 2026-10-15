package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PhoneModel selects the driver that renders config for, and decides the
// lifecycle of, a hardware phone. Models are drivers in Go code, not data
// (design §4): a new model is a new driver, not a new field.
//
// +kubebuilder:validation:Enum=cisco-7945;cisco-7965;cisco-7975
type PhoneModel string

// Supported phone models. All three are served by the cisco79x5 driver (one
// firmware family).
const (
	PhoneModelCisco7945 PhoneModel = "cisco-7945"
	PhoneModelCisco7965 PhoneModel = "cisco-7965"
	PhoneModelCisco7975 PhoneModel = "cisco-7975"
)

// PhoneSpec is the intended state of a hardware desk phone.
type PhoneSpec struct {
	// Model is the hardware model; it selects the driver.
	// +required
	Model PhoneModel `json:"model"`

	// MAC is the phone's Ethernet address: 12 hex digits, no separators, any
	// case (normalised to upper case).
	// +required
	MAC MAC `json:"mac"`

	// Number is the phone's own extension, in the device range of the number
	// plan (1XX by default).
	// +required
	Number Number `json:"number"`

	// Label is shown on the phone's line button and sent as its caller ID
	// name. Defaults to the resource name when empty. The driver derives
	// the phone's short screen label from it (for the 79x5: spaces removed,
	// at most 12 characters, with a warning condition when that changes it).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Label string `json:"label,omitempty"`

	// Firmware is the load the phone should run, by the name the phone
	// reports and the SEP file's loadInformation carries, for example
	// SIP45.9-4-2SR4-3S. Empty means the driver's default load. Its files
	// are staged with `patchbay firmware load`, never committed.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	Firmware string `json:"firmware,omitempty"`
}

// PhoneObserved is what patchbay has seen the phone do. The agent's memory is
// disposable, so the controller copies here anything the state machine must
// remember (design §8.1).
type PhoneObserved struct {
	// FirstSeen is when the phone was first seen fetching a file or
	// registering.
	// +optional
	FirstSeen *metav1.Time `json:"firstSeen,omitempty"`

	// Firmware is the load the phone last reported (for example
	// SIP45.9-4-2SR4-3S, or jar45sccp.9-4-2SR4-3 while still on SCCP).
	// +optional
	Firmware string `json:"firmware,omitempty"`

	// Address is the IP address the phone last fetched config or registered
	// from.
	// +optional
	Address string `json:"address,omitempty"`

	// LastConfigFetch is the most recent fetch of this phone's rendered
	// config.
	// +optional
	LastConfigFetch *ConfigFetch `json:"lastConfigFetch,omitempty"`

	// LastRegistered is the last time the phone was seen holding a SIP
	// registration.
	// +optional
	LastRegistered *metav1.Time `json:"lastRegistered,omitempty"`

	// UpgradeStartedAt is when the phone was first seen fetching firmware
	// for the upgrade in progress. Cleared when the upgrade ends.
	// +optional
	UpgradeStartedAt *metav1.Time `json:"upgradeStartedAt,omitempty"`
}

// PhoneStatus is written only by the controller.
type PhoneStatus struct {
	// State is a one-word summary for humans and `kubectl get`. It is not a
	// contract and its values may change; tools read Conditions.
	// +optional
	State string `json:"state,omitempty"`

	// ObservedGeneration is the .metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready, FirmwareAvailable, FirmwareCurrent, Provisioned,
	// Registered, ConfigCurrent, and Unsupported / NumberConflict when they
	// apply.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Observed is what the phone has been seen doing.
	// +optional
	Observed *PhoneObserved `json:"observed,omitempty"`

	// LastAction is the most recent action taken on the phone.
	// +optional
	LastAction *LastAction `json:"lastAction,omitempty"`

	// HumanAction is the exact instruction for a person, set whenever the
	// phone cannot make progress without one (connect it, stage firmware,
	// factory-reset it, power-cycle it). Empty when nothing is needed.
	// +optional
	HumanAction string `json:"humanAction,omitempty"`
}

// Phone is a provisioned hardware desk phone. Creating one is the whole
// onboarding step: patchbay renders its config, serves it over TFTP, and
// walks the phone to Registered.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=patchbay
// +kubebuilder:printcolumn:name="Number",type=string,JSONPath=`.spec.number`
// +kubebuilder:printcolumn:name="Label",type=string,JSONPath=`.spec.label`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`,priority=1
// +kubebuilder:printcolumn:name="MAC",type=string,JSONPath=`.spec.mac`,priority=1
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.observed.address`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Phone struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec PhoneSpec `json:"spec"`
	// +optional
	Status PhoneStatus `json:"status,omitempty"`
}

// PhoneList is a list of Phones.
//
// +kubebuilder:object:root=true
type PhoneList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Phone `json:"items"`
}
