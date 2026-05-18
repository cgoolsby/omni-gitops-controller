// Package v1alpha1 contains API schema definitions for omni.gitops.dev/v1alpha1.
// +groupName=omni.gitops.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the API group + version for all omni-controller CRDs.
var GroupVersion = schema.GroupVersion{Group: "omni.gitops.dev", Version: "v1alpha1"}

var (
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&OmniCluster{}, &OmniClusterList{})
}
