#!/usr/bin/env bash
# =============================================================================
# master-deploy.sh — deploy the GPU C/R system from the Kubernetes MASTER node.
#
# Run AFTER:
#   * base K8s up (master + worker joined) via Kubernetes_Installer_with_CRIO
#   * gpu-worker-setup.sh has finished on every GPU worker
#
#   bash master-deploy.sh
#
# It (1) installs the NVIDIA device plugin, (2) labels GPU nodes, (3) applies the
# CRD + RBAC + CRI-O DaemonSet. The agent image is pulled from Docker Hub
# (docker.io/jeongseungjun/gpu-cr-node-agent:v1.0, imagePullPolicy: Always).
# =============================================================================
set -Eeuo pipefail
log(){ echo -e "\n\033[1;32m[master-deploy]\033[0m $*"; }
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

log "1/4  NVIDIA device plugin (advertises nvidia.com/gpu)"
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.16.2/deployments/static/nvidia-device-plugin.yml

log "2/4  label GPU nodes (edit the node list if needed)"
for n in $(kubectl get nodes -o name | grep -v control-plane | sed 's#node/##'); do
  if kubectl describe node "$n" | grep -q 'nvidia.com/gpu'; then
    kubectl label node "$n" nvidia.com/gpu.present=true --overwrite
    echo "  labelled $n"
  fi
done

log "3/4  CRD + RBAC + namespace"
kubectl apply -f config/crd/gpu-cr.io_gpucheckpoints.yaml
kubectl create namespace gpu-cr-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f deploy/rbac.yaml

log "4/4  Node Agent DaemonSet (CRI-O variant)"
kubectl apply -f deploy/daemonset-crio.yaml
kubectl -n gpu-cr-system rollout status ds/gpu-cr-node-agent --timeout=180s || true

log "DONE. Verify:"
echo "  kubectl get nodes -L nvidia.com/gpu.present"
echo "  kubectl -n gpu-cr-system get pods -o wide"
echo "  kubectl -n gpu-cr-system logs -l app.kubernetes.io/name=gpu-cr-node-agent --tail=40"
