// Package v1alpha1 is the patchbay.chrismiller.xyz/v1alpha1 API: the
// resources a person writes to describe their phone system.
//
// The rule (design §4) is one resource per dialable thing, and it carries its
// own number: Phone (provisioned hardware), Softphone, Person (rings several
// devices) and Stream (release B). Every resource is namespaced and lives in
// the PBX's namespace.
//
// Spec is intent and is written by people (or git). Status is written only by
// the controller. Tools read status.conditions; status.state is a one-word
// display summary and is not a contract (design §3).
//
// The CRDs in charts/patchbay/templates/crds/ are generated from these types
// by `make generate`; never edit them by hand.
//
// +kubebuilder:object:generate=true
// +groupName=patchbay.chrismiller.xyz
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group of every patchbay resource.
const GroupName = "patchbay.chrismiller.xyz"

var (
	// GroupVersion is the group and version of this package's types.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	// SchemeBuilder registers this package's types with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds this package's types to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&Phone{}, &PhoneList{},
		&Softphone{}, &SoftphoneList{},
		&Person{}, &PersonList{},
		&Stream{}, &StreamList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
