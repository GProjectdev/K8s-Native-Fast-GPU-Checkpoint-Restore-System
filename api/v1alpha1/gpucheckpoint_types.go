package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodRef identifies the GPU Pod (and container) to be checkpointed.
type PodRef struct {
	// Namespace of the target Pod.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name of the target Pod.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Container is the name of the GPU container inside the Pod.
	// If empty, the first container is used.
	// +optional
	Container string `json:"container,omitempty"`

	// NodeInfo names the node the target Pod runs on. Per the DCN design, the
	// GPUCheckpoint CR carries everything the Node Agent needs, so the agent can
	// act directly on the CR (it watches CRs and only handles those whose
	// NodeInfo matches the node it runs on). When empty, the agent falls back to
	// resolving the node from the Pod's spec.nodeName at reconcile time.
	// +optional
	NodeInfo string `json:"nodeInfo,omitempty"`
}

// StorageType enumerates the supported checkpoint backends.
// +kubebuilder:validation:Enum=hostPath;nfs;s3
type StorageType string

const (
	StorageHostPath StorageType = "hostPath"
	StorageNFS      StorageType = "nfs"
	StorageS3       StorageType = "s3"
)

// StorageSpec defines where the checkpoint artifact (Checkpoint.tar) is stored.
type StorageSpec struct {
	// Type of the storage backend (e.g. hostPath, nfs, s3).
	// +kubebuilder:default=hostPath
	Type StorageType `json:"type"`

	// Path is the directory (or bucket prefix) the checkpoint file is written to.
	// e.g. /var/lib/gcr-checkpoint
	// +kubebuilder:validation:Required
	Path string `json:"path"`

	// Endpoint is an optional backend host/endpoint (e.g. NFS server, S3 host).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// GPUCheckpointSpec defines the desired checkpoint behaviour for a Pod.
type GPUCheckpointSpec struct {
	// PodRef points at the Pod (and container) to checkpoint.
	// +kubebuilder:validation:Required
	PodRef PodRef `json:"podRef"`

	// Storage defines the checkpoint backend and path.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`

	// Period is the checkpoint interval encoded as a fixed-width HHMMSS string
	// (e.g. "000500" == every 5 minutes, "010000" == hourly). An empty value
	// or "000000" means a single one-shot checkpoint.
	// +kubebuilder:validation:Pattern=`^[0-9]{6}$`
	// +optional
	Period string `json:"period,omitempty"`

	// Incremental enables GCR shadow-execution based incremental checkpointing
	// for checkpoints after the first one (dirty buffers only).
	// +kubebuilder:default=false
	// +optional
	Incremental bool `json:"incremental,omitempty"`
}

// CheckpointPhase is the lifecycle phase of a GPUCheckpoint.
type CheckpointPhase string

const (
	PhasePending      CheckpointPhase = "Pending"
	PhaseCheckpointing CheckpointPhase = "Checkpointing"
	PhaseCompleted    CheckpointPhase = "Completed"
	PhaseFailed       CheckpointPhase = "Failed"
)

// GPUCheckpointStatus reports the observed checkpoint state.
type GPUCheckpointStatus struct {
	// Phase is the high-level lifecycle phase.
	// +optional
	Phase CheckpointPhase `json:"phase,omitempty"`

	// ObservedNode is the node the checkpoint was (or is being) taken on.
	// +optional
	ObservedNode string `json:"observedNode,omitempty"`

	// LastCheckpointTime is when the most recent checkpoint completed.
	// +optional
	LastCheckpointTime *metav1.Time `json:"lastCheckpointTime,omitempty"`

	// CheckpointCount is the total number of successful checkpoints taken.
	// +optional
	CheckpointCount int64 `json:"checkpointCount,omitempty"`

	// LastCheckpointPath is the path of the most recently stored artifact.
	// +optional
	LastCheckpointPath string `json:"lastCheckpointPath,omitempty"`

	// Message carries human-readable detail (e.g. error reason).
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
// +kubebuilder:resource:shortName=gpuckpt
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.podRef.name`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.observedNode`
// +kubebuilder:printcolumn:name="Period",type=string,JSONPath=`.spec.period`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Count",type=integer,JSONPath=`.status.checkpointCount`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastCheckpointTime`

// GPUCheckpoint is the Schema for the gpucheckpoints API.
type GPUCheckpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUCheckpointSpec   `json:"spec,omitempty"`
	Status GPUCheckpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUCheckpointList contains a list of GPUCheckpoint.
type GPUCheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUCheckpoint `json:"items"`
}
