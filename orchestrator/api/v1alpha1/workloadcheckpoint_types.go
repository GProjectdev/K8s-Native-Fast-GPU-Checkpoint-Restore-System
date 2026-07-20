package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpucrv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/api/v1alpha1"
)

// WorkloadAction selects checkpoint vs restore for the whole workload.
// +kubebuilder:validation:Enum=Checkpoint;Restore
type WorkloadAction string

const (
	ActionCheckpoint WorkloadAction = "Checkpoint"
	ActionRestore    WorkloadAction = "Restore"
)

// CoordinationPolicy controls whether replicas are frozen independently or
// together. "Barrier" is required for distributed jobs (e.g. NCCL collectives)
// where an inconsistent, per-replica snapshot would be unrecoverable.
// +kubebuilder:validation:Enum=None;Barrier
type CoordinationPolicy string

const (
	CoordinationNone    CoordinationPolicy = "None"
	CoordinationBarrier CoordinationPolicy = "Barrier"
)

// WorkloadTargetRef identifies a workload that may resolve to one *or many* Pods.
// Unlike the per-Pod GPUCheckpoint.WorkloadRef, this carries no node info — the
// controller resolves the concrete Pods (and their nodes) at reconcile time.
type WorkloadTargetRef struct {
	// Kind of the target workload.
	// +kubebuilder:validation:Enum=Pod;Job;Deployment;StatefulSet;ReplicaSet
	// +kubebuilder:default=Pod
	// +optional
	Kind string `json:"kind,omitempty"`

	// Namespace of the target workload.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name of the target workload (the Pod name when kind=Pod).
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Container is the GPU container name propagated to each child. If empty,
	// the child resolves the first container of its Pod.
	// +optional
	Container string `json:"container,omitempty"`
}

// WorkloadCheckpointSpec is the desired state for a workload-wide checkpoint.
type WorkloadCheckpointSpec struct {
	// WorkloadRef points at the workload to checkpoint/restore.
	// +kubebuilder:validation:Required
	WorkloadRef WorkloadTargetRef `json:"workloadRef"`

	// Action is Checkpoint (default) or Restore.
	// +kubebuilder:default=Checkpoint
	// +optional
	Action WorkloadAction `json:"action,omitempty"`

	// Storage is the checkpoint backend, propagated to every child. The
	// controller appends the Pod name to the path/subPath so replicas never
	// collide on shared storage.
	// +kubebuilder:validation:Required
	Storage gpucrv1alpha1.StorageSpec `json:"storage"`

	// PodSelector optionally narrows the resolved Pods further (ANDed with the
	// workload's own selector). Useful to target a subset of replicas.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// Coordination is None (independent replicas) or Barrier (coordinated
	// snapshot for distributed jobs). See CoordinationPolicy.
	// +kubebuilder:default=None
	// +optional
	Coordination CoordinationPolicy `json:"coordination,omitempty"`

	// Schedule is propagated to each child. It accepts a Go duration ("5m",
	// "1h") or a standard cron expression ("0 */2 * * *", "@hourly"). Empty
	// means a single one-shot checkpoint. When set, the orchestrator also
	// periodically re-resolves the workload so new/replaced replicas are
	// automatically given their own child.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Incremental propagates GCR incremental checkpointing to children.
	// +kubebuilder:default=false
	// +optional
	Incremental bool `json:"incremental,omitempty"`

	// RequireGPU filters resolved Pods to those requesting a GPU
	// (nvidia.com/gpu). Defaults to true.
	// +kubebuilder:default=true
	// +optional
	RequireGPU *bool `json:"requireGPU,omitempty"`

	// MaxConcurrent caps how many child checkpoints are created at once
	// (0 = unlimited). Lets you rate-limit fan-out on a busy cluster.
	// +kubebuilder:default=0
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
}

// WorkloadCheckpointPhase is the aggregate lifecycle phase.
type WorkloadCheckpointPhase string

const (
	WCPhasePending         WorkloadCheckpointPhase = "Pending"
	WCPhaseResolving       WorkloadCheckpointPhase = "Resolving"
	WCPhaseInProgress      WorkloadCheckpointPhase = "InProgress"
	WCPhaseCompleted       WorkloadCheckpointPhase = "Completed"
	WCPhasePartiallyFailed WorkloadCheckpointPhase = "PartiallyFailed"
	WCPhaseFailed          WorkloadCheckpointPhase = "Failed"
)

// TargetStatus is the per-Pod (per-child) rollup shown on the parent.
type TargetStatus struct {
	// PodName is the resolved Pod.
	PodName string `json:"podName"`
	// Node the Pod runs on.
	// +optional
	Node string `json:"node,omitempty"`
	// ChildName is the owned GPUCheckpoint object handling this Pod.
	// +optional
	ChildName string `json:"childName,omitempty"`
	// Phase mirrors the child GPUCheckpoint.status.phase.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Path is the stored artifact path once completed.
	// +optional
	Path string `json:"path,omitempty"`
	// Message carries per-target error detail.
	// +optional
	Message string `json:"message,omitempty"`
}

// WorkloadCheckpointStatus is the observed, aggregated state.
type WorkloadCheckpointStatus struct {
	// Phase is the aggregate lifecycle phase.
	// +optional
	Phase WorkloadCheckpointPhase `json:"phase,omitempty"`

	// ObservedWorkloadKind is the kind actually resolved.
	// +optional
	ObservedWorkloadKind string `json:"observedWorkloadKind,omitempty"`

	// Total is the number of resolved target Pods (children expected).
	// +optional
	Total int32 `json:"total"`
	// Active is the number of children not yet in a terminal phase.
	// +optional
	Active int32 `json:"active"`
	// Completed is the number of children that finished successfully.
	// +optional
	Completed int32 `json:"completed"`
	// Failed is the number of children that failed.
	// +optional
	Failed int32 `json:"failed"`

	// Targets is the per-Pod rollup.
	// +optional
	Targets []TargetStatus `json:"targets,omitempty"`

	// StartTime is when fan-out began.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime is when the aggregate reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message carries human-readable aggregate detail.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions follow the standard Kubernetes condition convention.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wckpt
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.workloadRef.kind`
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workloadRef.name`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.total`
// +kubebuilder:printcolumn:name="Done",type=integer,JSONPath=`.status.completed`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failed`

// WorkloadCheckpoint fans a workload-wide checkpoint/restore out to per-Pod
// GPUCheckpoint children, one per replica, and aggregates their status.
type WorkloadCheckpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadCheckpointSpec   `json:"spec,omitempty"`
	Status WorkloadCheckpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadCheckpointList contains a list of WorkloadCheckpoint.
type WorkloadCheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadCheckpoint `json:"items"`
}
