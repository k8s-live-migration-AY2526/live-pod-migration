#!/bin/bash
set -euo pipefail

echo "=== Running make undeploy for full reset ==="
make undeploy
echo

echo "=== Deleting all custom resources ==="
kubectl delete containercheckpoint --all --all-namespaces --ignore-not-found=true
kubectl delete podcheckpoint --all --all-namespaces --ignore-not-found=true
kubectl delete containercheckpointcontent --all --all-namespaces --ignore-not-found=true
kubectl delete podcheckpointcontent --all --all-namespaces --ignore-not-found=true
echo

echo "=== Deleting sample resources ==="
kubectl delete -f config/samples/lpm_v1_podmigration.yaml \
               -f config/samples/lpm_v1_podcheckpoint.yaml \
               -f config/samples/lpm_v1_containercheckpoint.yaml \
               --ignore-not-found=true
echo

echo "=== Deleting controller deployment and agents ==="
kubectl delete -n live-pod-migration-controller-system deployment lpm-controller-manager --ignore-not-found=true
kubectl delete -n live-pod-migration-controller-system daemonset lpm-live-pod-migration-controller-checkpoint-agent --ignore-not-found=true
kubectl delete -n live-pod-migration-controller-system daemonset live-pod-migration-controller-live-pod-migration-controller-checkpoint-agent --ignore-not-found=true
echo

echo "=== Deleting namespace ==="
kubectl delete namespace live-pod-migration-controller-system --ignore-not-found=true
echo

echo "=== Deleting CRDs ==="
kubectl delete crd containercheckpoints.lpm.my.domain --ignore-not-found=true
kubectl delete crd podcheckpoints.lpm.my.domain --ignore-not-found=true
kubectl delete crd containercheckpointcontents.lpm.my.domain --ignore-not-found=true
kubectl delete crd podcheckpointcontents.lpm.my.domain --ignore-not-found=true
kubectl delete crd podmigrations.lpm.my.domain --ignore-not-found=true
kubectl delete -f config/crd/bases/ --ignore-not-found=true
echo

echo "=== Removing container images from CRI-O storage ==="
sudo crictl rmi localhost/checkpoint-agent:latest localhost/controller:latest || true
echo

echo "=== Cleaning up checkpoint files from kubelet directory ==="
sudo find /var/lib/kubelet/checkpoints/ -name "checkpoint-*.tar" -delete
echo

echo "=== Cleaning up shared storage infrastructure ==="
kubectl delete -f config/storage/checkpoint-pvc.yaml --ignore-not-found=true
kubectl delete -f config/storage/nfs-provisioner.yaml --ignore-not-found=true
kubectl delete job/nfs-setup -n kube-system --ignore-not-found=true
kubectl delete configmap/nfs-setup-script -n kube-system --ignore-not-found=true
echo

echo "=== Cleaning up test pods and migrations ==="
kubectl delete pod test-pod multi-container-pod stateful-pod --ignore-not-found=true
kubectl delete podmigration --all --all-namespaces --ignore-not-found=true
echo

echo "=== Cleanup complete! ==="
