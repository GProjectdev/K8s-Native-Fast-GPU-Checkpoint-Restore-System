package agent

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpucrv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/api/v1alpha1"
)

// Reconciler is the per-node controller. Every node runs one (as a DaemonSet);
// each instance only acts on GPUCheckpoint CRs whose target Pod lives on its
// own node, so the actual checkpoint work is always local.
type Reconciler struct {
	client.Client
	NodeName     string
	Checkpointer *Checkpointer
}

// +kubebuilder:rbac:groups=gpu-cr.io,resources=gpucheckpoints,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gpu-cr.io,resources=gpucheckpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile drives one GPUCheckpoint toward its desired (periodic) checkpoint state.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr gpucrv1alpha1.GPUCheckpoint
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the target node.
	targetNode := cr.Spec.PodRef.NodeName
	container := cr.Spec.PodRef.Container
	var podUID string
	if targetNode == "" || container == "" {
		var pod corev1.Pod
		key := types.NamespacedName{Namespace: cr.Spec.PodRef.Namespace, Name: cr.Spec.PodRef.Name}
		if err := r.Get(ctx, key, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				return r.fail(ctx, &cr, "target Pod not found")
			}
			return ctrl.Result{}, err
		}
		if targetNode == "" {
			targetNode = pod.Spec.NodeName
		}
		if container == "" && len(pod.Spec.Containers) > 0 {
			container = pod.Spec.Containers[0].Name
		}
		podUID = string(pod.UID)
	}

	// Only the agent on the target node proceeds.
	if targetNode != r.NodeName {
		klog.V(4).Infof("CR %s targets node %s, not me (%s); skipping", req.NamespacedName, targetNode, r.NodeName)
		return ctrl.Result{}, nil
	}

	period, err := ParsePeriod(cr.Spec.Period)
	if err != nil {
		return r.fail(ctx, &cr, "invalid period: "+err.Error())
	}

	// Decide whether a checkpoint is due now.
	now := time.Now()
	if cr.Status.LastCheckpointTime != nil {
		if period == 0 {
			// One-shot already taken: nothing more to do.
			return ctrl.Result{}, nil
		}
		nextDue := cr.Status.LastCheckpointTime.Add(period)
		if now.Before(nextDue) {
			return ctrl.Result{RequeueAfter: time.Until(nextDue)}, nil
		}
	}

	// Mark in-progress.
	cr.Status.Phase = gpucrv1alpha1.PhaseCheckpointing
	cr.Status.ObservedNode = targetNode
	_ = r.Status().Update(ctx, &cr)

	// Resolve container PID on this node.
	cid, pid, err := r.Checkpointer.ResolvePID(ctx, cr.Spec.PodRef.Namespace, cr.Spec.PodRef.Name, container)
	if err != nil {
		return r.fail(ctx, &cr, "resolve container pid: "+err.Error())
	}

	target := Target{
		Namespace:   cr.Spec.PodRef.Namespace,
		Pod:         cr.Spec.PodRef.Name,
		PodUID:      podUID,
		Container:   container,
		ContainerID: cid,
		HostPID:     pid,
		Incremental: cr.Spec.Incremental && cr.Status.CheckpointCount > 0,
		Storage:     cr.Spec.Storage,
	}

	res, err := r.Checkpointer.Checkpoint(ctx, target)
	if err != nil {
		return r.fail(ctx, &cr, "checkpoint failed: "+err.Error())
	}

	// Record success.
	t := metav1.NewTime(res.TakenAt)
	cr.Status.Phase = gpucrv1alpha1.PhaseCompleted
	cr.Status.LastCheckpointTime = &t
	cr.Status.LastCheckpointPath = res.ArchivePath
	cr.Status.CheckpointCount++
	cr.Status.Message = "checkpoint stored successfully"
	setCondition(&cr.Status, "Ready", metav1.ConditionTrue, "CheckpointStored", res.ArchivePath)
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, err
	}

	if period == 0 {
		klog.Infof("one-shot checkpoint completed for %s", req.NamespacedName)
		return ctrl.Result{}, nil
	}
	klog.Infof("periodic checkpoint #%d done for %s; next in %s", cr.Status.CheckpointCount, req.NamespacedName, period)
	return ctrl.Result{RequeueAfter: period}, nil
}

func (r *Reconciler) fail(ctx context.Context, cr *gpucrv1alpha1.GPUCheckpoint, msg string) (ctrl.Result, error) {
	klog.Errorf("GPUCheckpoint %s/%s failed: %s", cr.Namespace, cr.Name, msg)
	cr.Status.Phase = gpucrv1alpha1.PhaseFailed
	cr.Status.Message = msg
	setCondition(&cr.Status, "Ready", metav1.ConditionFalse, "CheckpointFailed", msg)
	_ = r.Status().Update(ctx, cr)
	// Retry failed checkpoints after a backoff.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func setCondition(status *gpucrv1alpha1.GPUCheckpointStatus, condType string, s metav1.ConditionStatus, reason, msg string) {
	now := metav1.Now()
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			if status.Conditions[i].Status != s {
				status.Conditions[i].LastTransitionTime = now
			}
			status.Conditions[i].Status = s
			status.Conditions[i].Reason = reason
			status.Conditions[i].Message = msg
			return
		}
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             s,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gpucrv1alpha1.GPUCheckpoint{}).
		Complete(r)
}
