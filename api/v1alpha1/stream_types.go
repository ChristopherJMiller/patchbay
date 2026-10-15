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

	// Name is the stream's display and caller ID name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`

	// Source is the absolute URL the stream bridge reads audio from, for
	// example an HDHomeRun channel (http://192.168.0.58:5004/auto/v5.1).
	// The controller never probes it: a probe would hold a tuner.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() != '' && url(self).getHost() != ''",message="source must be an absolute URL with a scheme and host"
	Source string `json:"source"`
}

// StreamStatus is written only by the controller.
type StreamStatus struct {
	// State is a one-word summary for humans and `kubectl get` (Idle,
	// Playing, Failing). It is not a contract; tools read Conditions.
	// +optional
	State string `json:"state,omitempty"`

	// ObservedGeneration is the .metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: NumberConflict, plus the stream conditions of release B.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
