// Package v1alpha1 contains API schema definitions for omni.gitops.dev/v1alpha1.
// +kubebuilder:object:generate=true
// +groupName=omni.gitops.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group + version for all omni-controller CRDs.
var GroupVersion = schema.GroupVersion{Group: "omni.gitops.dev", Version: "v1alpha1"}

var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &OmniCluster{}, &OmniClusterList{}, &Machine{}, &MachineList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
