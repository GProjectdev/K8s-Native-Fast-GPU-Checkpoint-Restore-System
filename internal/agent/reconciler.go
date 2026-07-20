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
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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

	// Only kind=Pod is resolved today; other workload kinds are reserved.
	if k := cr.Spec.WorkloadRef.Kind; k != "" && k != "Pod" {
		return r.fail(ctx, &cr, "workloadRef.kind "+k+" not supported yet (only Pod)")
	}

	// Resolve the Pod: node/container are filled from the Pod when the CR omits them.
	var pod corev1.Pod
	key := types.NamespacedName{Namespace: cr.Spec.WorkloadRef.Namespace, Name: cr.Spec.WorkloadRef.Name}
	if err := r.Get(ctx, key, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, &cr, "target Pod not found")
		}
		return ctrl.Result{}, err
	}
	targetNode := cr.Spec.WorkloadRef.NodeInfo
	if targetNode == "" {
		targetNode = pod.Spec.NodeName
	}
	container := cr.Spec.WorkloadRef.Container
	if container == "" && len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	podUID := string(pod.UID) // keys the interceptor control channel

	// Only the agent on the target node proceeds.
	if targetNode != r.NodeName {
		klog.V(4).Infof("CR %s targets node %s, not me (%s); skipping", req.NamespacedName, targetNode, r.NodeName)
		return ctrl.Result{}, nil
	}

	// Schedule may be a Go duration ("5m") or a cron expression ("0 */2 * * *").
	now := time.Now()
	if _, _, err := NextRun(cr.Spec.Schedule, now); err != nil {
		return r.fail(ctx, &cr, "invalid schedule: "+err.Error())
	}
	recurring := !IsOneShot(cr.Spec.Schedule)

	// Decide whether a checkpoint is due now.
	if cr.Status.LastCheckpointTime != nil {
		if !recurring {
			// One-shot already taken: nothing more to do.
			return ctrl.Result{}, nil
		}
		nextDue, _, err := NextRun(cr.Spec.Schedule, cr.Status.LastCheckpointTime.Time)
		if err != nil {
			return r.fail(ctx, &cr, "invalid schedule: "+err.Error())
		}
		if now.Before(nextDue) {
			return ctrl.Result{RequeueAfter: time.Until(nextDue)}, nil
		}
	}

	// Mark in-progress.
	cr.Status.Phase = gpucrv1alpha1.PhaseCheckpointing
	cr.Status.ObservedNode = targetNode
	_ = r.Status().Update(ctx, &cr)

	target := Target{
		Namespace: cr.Spec.WorkloadRef.Namespace,
		Pod:       cr.Spec.WorkloadRef.Name,
		PodUID:    podUID,
		Container: container,
		Storage:   cr.Spec.Storage,
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

	if !recurring {
		klog.Infof("one-shot checkpoint completed for %s", req.NamespacedName)
		return ctrl.Result{}, nil
	}
	nextDue, _, _ := NextRun(cr.Spec.Schedule, time.Now())
	klog.Infof("periodic checkpoint #%d done for %s; next at %s", cr.Status.CheckpointCount, req.NamespacedName, nextDue.Format(time.RFC3339))
	return ctrl.Result{RequeueAfter: time.Until(nextDue)}, nil
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
		// Only reconcile on spec changes (create/spec-update) and our own
		// RequeueAfter timers — NOT on our own status writes, which would
		// otherwise retrigger reconcile in a tight loop.
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
