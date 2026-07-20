// Package controllers holds the WorkloadCheckpoint orchestrator controller. It
// runs in its OWN binary/Deployment (not the Node Agent DaemonSet). It never
// performs checkpoint work itself: it resolves a workload to its Pods and
// creates one per-Pod GPUCheckpoint child (kind=Pod) that the existing Node
// Agent already knows how to handle, then aggregates the children's status.
package controllers

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gpucrv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/api/v1alpha1"
	wcv1alpha1 "github.com/GProjectdev/K8s-Native-Fast-GPU-Checkpoint-Restore-System/orchestrator/api/v1alpha1"
)

const (
	// ownedByLabel ties children back to their parent WorkloadCheckpoint.
	ownedByLabel = "gpu-cr.io/workload-checkpoint"
	gpuResource  = corev1.ResourceName("nvidia.com/gpu")
	// resolveInterval re-resolves a recurring (scheduled) WorkloadCheckpoint's
	// Pods so new/replaced replicas get their own child.
	resolveInterval = 30 * time.Second
	// activePollInterval polls a one-shot fan-out while children are still running.
	activePollInterval = 15 * time.Second
)

// WorkloadCheckpointReconciler reconciles WorkloadCheckpoint objects.
type WorkloadCheckpointReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=gpu-cr.io,resources=workloadcheckpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gpu-cr.io,resources=workloadcheckpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gpu-cr.io,resources=workloadcheckpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups=gpu-cr.io,resources=gpucheckpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch

// Reconcile resolves the workload, fans out per-Pod children, and aggregates.
func (r *WorkloadCheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var wc wcv1alpha1.WorkloadCheckpoint
	if err := r.Get(ctx, req.NamespacedName, &wc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Restore is scaffolded but the Node Agent's restore path is not wired yet.
	if wc.Spec.Action == wcv1alpha1.ActionRestore {
		return r.finish(ctx, &wc, wcv1alpha1.WCPhaseFailed,
			"Action=Restore is defined but the Node Agent restore path is not implemented yet")
	}

	// (1) Resolve target Pods from the workloadRef.
	pods, kind, err := r.resolvePods(ctx, &wc)
	if err != nil {
		return r.finish(ctx, &wc, wcv1alpha1.WCPhaseFailed, "resolve workload: "+err.Error())
	}
	wc.Status.ObservedWorkloadKind = kind
	if len(pods) == 0 {
		return r.finish(ctx, &wc, wcv1alpha1.WCPhaseFailed, "no matching (GPU) Pods resolved from workloadRef")
	}

	if wc.Status.Phase == "" || wc.Status.Phase == wcv1alpha1.WCPhasePending {
		wc.Status.Phase = wcv1alpha1.WCPhaseResolving
		if wc.Status.StartTime == nil {
			now := metav1.Now()
			wc.Status.StartTime = &now
		}
	}

	// (2) Optional coordinated barrier for distributed jobs (stub).
	if wc.Spec.Coordination == wcv1alpha1.CoordinationBarrier {
		if err := r.applyBarrier(ctx, &wc, pods); err != nil {
			return ctrl.Result{}, err
		}
	}

	// (3) Ensure one child GPUCheckpoint per Pod (respecting MaxConcurrent).
	created := 0
	inFlight := int32(0)
	for i := range pods {
		pod := pods[i]
		name := childName(wc.Name, pod.Name)
		var child gpucrv1alpha1.GPUCheckpoint
		errGet := r.Get(ctx, types.NamespacedName{Namespace: wc.Namespace, Name: name}, &child)
		if errGet == nil {
			if !isTerminal(child.Status.Phase) {
				inFlight++
			}
			continue
		}
		if !apierrors.IsNotFound(errGet) {
			return ctrl.Result{}, errGet
		}
		if wc.Spec.MaxConcurrent > 0 && inFlight >= wc.Spec.MaxConcurrent {
			break // rate-limited; reconcile again when a child terminates
		}
		child = r.buildChild(&wc, &pod, name)
		if err := ctrl.SetControllerReference(&wc, &child, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &child); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		created++
		inFlight++
		lg.Info("created child GPUCheckpoint", "child", name, "pod", pod.Name, "node", pod.Spec.NodeName)
	}

	// (4) Aggregate children into the parent status.
	if err := r.aggregate(ctx, &wc, pods); err != nil {
		return ctrl.Result{}, err
	}

	// (5) Requeue to re-resolve Pods so new/replaced replicas are picked up.
	// Recurring (scheduled) WorkloadCheckpoints poll indefinitely; one-shot ones
	// poll only while children are still active.
	if wc.Spec.Schedule != "" {
		return ctrl.Result{RequeueAfter: resolveInterval}, nil
	}
	if wc.Status.Active > 0 {
		return ctrl.Result{RequeueAfter: activePollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// resolvePods maps the workloadRef to concrete (optionally GPU-only) Pods.
func (r *WorkloadCheckpointReconciler) resolvePods(ctx context.Context, wc *wcv1alpha1.WorkloadCheckpoint) ([]corev1.Pod, string, error) {
	ref := wc.Spec.WorkloadRef
	ns := ref.Namespace
	kind := ref.Kind
	if kind == "" {
		kind = "Pod"
	}

	var sel labels.Selector
	switch kind {
	case "Pod":
		var pod corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &pod); err != nil {
			return nil, kind, err
		}
		return r.filterPods([]corev1.Pod{pod}, wc), kind, nil
	case "Deployment":
		var d appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &d); err != nil {
			return nil, kind, err
		}
		sel, _ = metav1.LabelSelectorAsSelector(d.Spec.Selector)
	case "ReplicaSet":
		var rs appsv1.ReplicaSet
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &rs); err != nil {
			return nil, kind, err
		}
		sel, _ = metav1.LabelSelectorAsSelector(rs.Spec.Selector)
	case "StatefulSet":
		var ss appsv1.StatefulSet
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &ss); err != nil {
			return nil, kind, err
		}
		sel, _ = metav1.LabelSelectorAsSelector(ss.Spec.Selector)
	case "Job":
		var j batchv1.Job
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &j); err != nil {
			return nil, kind, err
		}
		if j.Spec.Selector != nil {
			sel, _ = metav1.LabelSelectorAsSelector(j.Spec.Selector)
		} else {
			sel = labels.SelectorFromSet(labels.Set{"job-name": ref.Name})
		}
	default:
		return nil, kind, fmt.Errorf("unsupported workload kind %q", kind)
	}

	var list corev1.PodList
	if err := r.List(ctx, &list, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, kind, err
	}
	return r.filterPods(list.Items, wc), kind, nil
}

// filterPods keeps Running Pods, applies the optional podSelector, and (by
// default) keeps only Pods that request a GPU.
func (r *WorkloadCheckpointReconciler) filterPods(in []corev1.Pod, wc *wcv1alpha1.WorkloadCheckpoint) []corev1.Pod {
	requireGPU := true
	if wc.Spec.RequireGPU != nil {
		requireGPU = *wc.Spec.RequireGPU
	}
	var extra labels.Selector
	if wc.Spec.PodSelector != nil {
		extra, _ = metav1.LabelSelectorAsSelector(wc.Spec.PodSelector)
	}
	out := make([]corev1.Pod, 0, len(in))
	for i := range in {
		p := in[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		if extra != nil && !extra.Matches(labels.Set(p.Labels)) {
			continue
		}
		if requireGPU && !podRequestsGPU(&p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func podRequestsGPU(p *corev1.Pod) bool {
	for _, c := range p.Spec.Containers {
		if v, ok := c.Resources.Limits[gpuResource]; ok && !v.IsZero() {
			return true
		}
		if v, ok := c.Resources.Requests[gpuResource]; ok && !v.IsZero() {
			return true
		}
	}
	return false
}

// buildChild constructs a per-Pod GPUCheckpoint (kind=Pod) for the Node Agent.
func (r *WorkloadCheckpointReconciler) buildChild(wc *wcv1alpha1.WorkloadCheckpoint, pod *corev1.Pod, name string) gpucrv1alpha1.GPUCheckpoint {
	return gpucrv1alpha1.GPUCheckpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: wc.Namespace,
			Labels:    map[string]string{ownedByLabel: wc.Name},
		},
		Spec: gpucrv1alpha1.GPUCheckpointSpec{
			WorkloadRef: gpucrv1alpha1.WorkloadRef{
				Kind:      "Pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
				Container: wc.Spec.WorkloadRef.Container,
				NodeInfo:  pod.Spec.NodeName,
			},
			Storage:     perPodStorage(wc.Spec.Storage, pod.Name),
			Schedule:    wc.Spec.Schedule,
			Incremental: wc.Spec.Incremental,
		},
	}
}

// perPodStorage appends the Pod name to the destination so replicas don't
// collide on shared storage.
func perPodStorage(s gpucrv1alpha1.StorageSpec, pod string) gpucrv1alpha1.StorageSpec {
	out := s
	switch s.Type {
	case gpucrv1alpha1.StorageMount, gpucrv1alpha1.StorageNFS, gpucrv1alpha1.StoragePVC, gpucrv1alpha1.StorageS3:
		out.SubPath = joinPath(s.SubPath, pod)
	default: // hostPath and others use Path
		out.Path = joinPath(s.Path, pod)
	}
	return out
}

func joinPath(base, leaf string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return leaf
	}
	return base + "/" + leaf
}

// aggregate rolls child statuses up into the parent.
func (r *WorkloadCheckpointReconciler) aggregate(ctx context.Context, wc *wcv1alpha1.WorkloadCheckpoint, pods []corev1.Pod) error {
	var children gpucrv1alpha1.GPUCheckpointList
	if err := r.List(ctx, &children, client.InNamespace(wc.Namespace),
		client.MatchingLabels{ownedByLabel: wc.Name}); err != nil {
		return err
	}
	byName := map[string]*gpucrv1alpha1.GPUCheckpoint{}
	for i := range children.Items {
		byName[children.Items[i].Name] = &children.Items[i]
	}

	var total, active, done, failed int32
	targets := make([]wcv1alpha1.TargetStatus, 0, len(pods))
	for i := range pods {
		pod := pods[i]
		total++
		ts := wcv1alpha1.TargetStatus{PodName: pod.Name, Node: pod.Spec.NodeName, ChildName: childName(wc.Name, pod.Name)}
		if c, ok := byName[ts.ChildName]; ok {
			ts.Phase = string(c.Status.Phase)
			ts.Path = c.Status.LastCheckpointPath
			ts.Message = c.Status.Message
			switch {
			case c.Status.Phase == gpucrv1alpha1.PhaseCompleted:
				done++
			case c.Status.Phase == gpucrv1alpha1.PhaseFailed:
				failed++
			default:
				active++
			}
		} else {
			ts.Phase = "Pending"
			active++
		}
		targets = append(targets, ts)
	}

	wc.Status.Total = total
	wc.Status.Active = active
	wc.Status.Completed = done
	wc.Status.Failed = failed
	wc.Status.Targets = targets

	switch {
	case done == total:
		wc.Status.Phase = wcv1alpha1.WCPhaseCompleted
		wc.Status.Message = "all replicas checkpointed"
		if wc.Status.CompletionTime == nil {
			now := metav1.Now()
			wc.Status.CompletionTime = &now
		}
	case done+failed == total && failed > 0 && done > 0:
		wc.Status.Phase = wcv1alpha1.WCPhasePartiallyFailed
		wc.Status.Message = fmt.Sprintf("%d/%d replicas failed", failed, total)
	case done+failed == total && done == 0:
		wc.Status.Phase = wcv1alpha1.WCPhaseFailed
		wc.Status.Message = "all replicas failed"
	default:
		wc.Status.Phase = wcv1alpha1.WCPhaseInProgress
	}
	return r.Status().Update(ctx, wc)
}

// applyBarrier is a placeholder for coordinated (distributed-job) freezing. A
// real implementation would pause/quiesce all replicas at a consistent point
// (e.g. after a collective boundary) before children begin. Left as a no-op so
// Coordination=Barrier is wired end-to-end for a future data-plane hook.
func (r *WorkloadCheckpointReconciler) applyBarrier(ctx context.Context, wc *wcv1alpha1.WorkloadCheckpoint, pods []corev1.Pod) error {
	_ = ctx
	_ = wc
	_ = pods
	return nil
}

// finish writes a terminal/short-circuit status and stops reconciling.
func (r *WorkloadCheckpointReconciler) finish(ctx context.Context, wc *wcv1alpha1.WorkloadCheckpoint, phase wcv1alpha1.WorkloadCheckpointPhase, msg string) (ctrl.Result, error) {
	wc.Status.Phase = phase
	wc.Status.Message = msg
	if (phase == wcv1alpha1.WCPhaseFailed || phase == wcv1alpha1.WCPhaseCompleted) && wc.Status.CompletionTime == nil {
		now := metav1.Now()
		wc.Status.CompletionTime = &now
	}
	return ctrl.Result{}, r.Status().Update(ctx, wc)
}

func isTerminal(p gpucrv1alpha1.CheckpointPhase) bool {
	return p == gpucrv1alpha1.PhaseCompleted || p == gpucrv1alpha1.PhaseFailed
}

// childName is a deterministic, DNS-1123-safe name for a Pod's child object.
func childName(parent, pod string) string {
	base := parent + "-" + pod
	if len(base) <= 253 {
		return base
	}
	sum := sha1.Sum([]byte(pod))
	return parent + "-" + hex.EncodeToString(sum[:])[:12]
}

// SetupWithManager wires the controller: it owns GPUCheckpoint children, so a
// child status change re-triggers aggregation on the parent.
func (r *WorkloadCheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&wcv1alpha1.WorkloadCheckpoint{}).
		Owns(&gpucrv1alpha1.GPUCheckpoint{}).
		Complete(r)
}
