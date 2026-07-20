// Package v1alpha1 defines the WorkloadCheckpoint API — the high-level,
// workload-scoped orchestrator that fans a single request out to one per-Pod
// GPUCheckpoint (in the existing gpu-cr.io/v1alpha1 API group) per replica.
//
// It shares the gpu-cr.io group/version with GPUCheckpoint but lives in its own
// Go package and its own controller binary, so the per-node Node Agent is never
// touched: the agent keeps watching GPUCheckpoint objects, and this package only
// *creates* those objects.
//
// +kubebuilder:object:generate=true
// +groupName=gpu-cr.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion matches the existing CRs: apiVersion: gpu-cr.io/v1alpha1.
	GroupVersion = schema.GroupVersion{Group: "gpu-cr.io", Version: "v1alpha1"}

	// SchemeBuilder registers only the WorkloadCheckpoint kinds; GPUCheckpoint is
	// registered by its own package and added to the scheme separately.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the WorkloadCheckpoint types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&WorkloadCheckpoint{}, &WorkloadCheckpointList{})
}
