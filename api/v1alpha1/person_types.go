package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RingTarget names exactly one device, in the same namespace, that rings
// when the person's number is dialled. Set phone or softphone, not both.
//
// +kubebuilder:validation:XValidation:rule="has(self.phone) != has(self.softphone)",message="exactly one of phone or softphone must be set"
type RingTarget struct {
	// Phone is the name of a Phone.
	// +optional
	Phone LocalName `json:"phone,omitempty"`

	// Softphone is the name of a Softphone.
	// +optional
	Softphone LocalName `json:"softphone,omitempty"`
}

// PersonSpec is the intended state of a person's number.
type PersonSpec struct {
	// Number is the person's extension, in the people range of the number
	// plan (3XX by default).
	// +required
	Number Number `json:"number"`

	// Name is the person's display and caller ID name. No control
	// characters (line breaks included); anything else is escaped wherever
	// it is rendered.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F-\x9F]*$`
	Name string `json:"name"`

	// Rings lists the devices that ring, all at once, when Number is
	// dialled. A target that does not exist is skipped (and reported in
	// status), not an error.
	// +required
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Rings []RingTarget `json:"rings"`
}

// PersonStatus is written only by the controller.
type PersonStatus struct {
	// State is a one-word summary for humans and `kubectl get`. It is not a
	// contract; tools read Conditions.
	// +optional
	State string `json:"state,omitempty"`

	// ObservedGeneration is the .metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: NumberConflict, plus any reported by the controller about
	// unresolved ring targets.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Person is a number that rings all of one person's devices.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=patchbay
// +kubebuilder:printcolumn:name="Number",type=string,JSONPath=`.spec.number`
// +kubebuilder:printcolumn:name="Display",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Person struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec PersonSpec `json:"spec"`
	// +optional
	Status PersonStatus `json:"status,omitempty"`
}

// PersonList is a list of People.
//
// +kubebuilder:object:root=true
type PersonList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Person `json:"items"`
}
