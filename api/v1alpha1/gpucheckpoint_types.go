package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadRef identifies the GPU workload (and container) to checkpoint. It
// generalizes the earlier PodRef via a Kind selector: "Pod" is supported today;
// "Deployment"/"StatefulSet" are reserved for future multi-replica resolution.
type WorkloadRef struct {
	// Kind of the target workload.
	// +kubebuilder:validation:Enum=Pod;Deployment;StatefulSet
	// +kubebuilder:default=Pod
	// +optional
	Kind string `json:"kind,omitempty"`

	// Namespace of the target workload.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name of the target workload (the Pod name when kind=Pod).
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Container is the name of the GPU container. If empty, the first is used.
	// +optional
	Container string `json:"container,omitempty"`

	// NodeInfo names the node the target runs on. The CR carries everything the
	// Node Agent needs; each agent only handles CRs whose NodeInfo matches its
	// node. When empty, the agent resolves the node from the Pod at reconcile.
	// +optional
	NodeInfo string `json:"nodeInfo,omitempty"`
}

// StorageType enumerates the supported checkpoint backends.
// +kubebuilder:validation:Enum=hostPath;mount;nfs;pvc;s3
type StorageType string

const (
	StorageHostPath StorageType = "hostPath" // a volume already mounted into the agent
	StorageMount    StorageType = "mount"    // generic file mount: fsType+source+options (nfs, efs, cifs, cephfs, ...)
	StorageNFS      StorageType = "nfs"      // convenience alias for mount with fsType=nfs (endpoint:path)
	StoragePVC      StorageType = "pvc"      // CSI-backed claim (EBS, EFS, ...) via the checkpoint mover
	StorageS3       StorageType = "s3"       // object storage
)

// StorageSpec defines where the checkpoint artifact (Checkpoint.tar) is stored.
type StorageSpec struct {
	// Type of the storage backend (e.g. hostPath, nfs, s3).
	// +kubebuilder:default=hostPath
	Type StorageType `json:"type"`

	// Path is the directory the tar is written to (hostPath), or the subdir under
	// the backend for mount/pvc. Required for hostPath/nfs; optional otherwise.
	// +optional
	Path string `json:"path,omitempty"`

	// Endpoint is an optional backend host/endpoint (e.g. NFS server, S3 host).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// --- generic file-mount backend (type: mount) ---
	// FsType is the filesystem type passed to mount(8): nfs, nfs4, cifs, ceph, etc.
	// +optional
	FsType string `json:"fsType,omitempty"`
	// Source is the mount source, e.g. "10.178.0.15:/mnt/nfs", "fs-xxxx.efs.region.amazonaws.com:/", "//host/share".
	// +optional
	Source string `json:"source,omitempty"`
	// Options are comma-separated mount -o options (e.g. "nfsvers=4,nolock").
	// +optional
	Options string `json:"options,omitempty"`
	// SubPath is an optional subdirectory under the backend to write into.
	// +optional
	SubPath string `json:"subPath,omitempty"`

	// --- CSI-backed backend (type: pvc) ---
	// ClaimName is the PersistentVolumeClaim to store into (EBS, EFS, ... via CSI).
	// +optional
	ClaimName string `json:"claimName,omitempty"`
}

// GPUCheckpointSpec defines the desired checkpoint behaviour for a Pod.
type GPUCheckpointSpec struct {
	// WorkloadRef points at the workload (and container) to checkpoint.
	// +kubebuilder:validation:Required
	WorkloadRef WorkloadRef `json:"workloadRef"`

	// Storage defines the checkpoint backend and path.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`

	// Schedule is the checkpoint cadence. It accepts either a Go duration string
	// ("30s", "5m", "1h") or a standard cron expression ("0 */2 * * *",
	// "@hourly"). An empty value means a single one-shot checkpoint.
	// +optional
	Schedule string `json:"schedule,omitempty"`

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
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workloadRef.name`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.observedNode`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
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
