// Package v1alpha1 contains API Schema definitions for the gpu-cr.io v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=gpu-cr.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version used to register these objects.
	// Matches the Progress Report CR: apiVersion: gpu-cr.io/v1alpha1
	GroupVersion = schema.GroupVersion{Group: "gpu-cr.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add Go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&GPUCheckpoint{}, &GPUCheckpointList{})
}
