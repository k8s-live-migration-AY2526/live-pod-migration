#!/bin/bash
set -euo pipefail

echo "Deploying shared storage that creates pvc/checkpoint-repo in namespace live-pod-migration-controller-system"

# Configurations
NFS_SERVER_IP="192.168.56.10"
NFS_PATH="/var/nfs/checkpoint-storage"

PVC_NAME="checkpoint-repo"
PVC_NAMESPACE="live-pod-migration-controller-system"
PVC_STORAGE_SIZE="20Gi"


echo "-------------- Configurations ---------------"
echo "NFS_SERVER_IP: $NFS_SERVER_IP"
echo "NFS_PATH: $NFS_PATH"
echo "PVC_STORAGE_SIZE: $PVC_STORAGE_SIZE"
echo "PVC_NAME: $PVC_NAME"
echo "PVC_NAMESPACE: $PVC_NAMESPACE"
echo "--------------------------------"

# Ensure non-interactive apt behavior (avoid prompts like kernel restart)
export DEBIAN_FRONTEND=noninteractive
# Disable needrestart prompts entirely and avoid restarting services/reboot prompts
export NEEDRESTART_SUSPEND=1
export NEEDRESTART_MODE=l

# 1. Install NFS server
echo "1. Installing NFS server..."
sudo -E apt-get update
sudo -E apt-get \
  -o Dpkg::Options::="--force-confdef" \
  -o Dpkg::Options::="--force-confnew" \
  install -y nfs-kernel-server

# 2. Create shared directory
echo "2. Creating NFS shared directory..."
sudo mkdir -p $NFS_PATH
sudo chown nobody:nogroup $NFS_PATH
sudo chmod 0777 $NFS_PATH

# 3. Configure exports
echo "3. Configuring NFS exports..."
EXPORT_LINE="$NFS_PATH *(rw,sync,no_subtree_check,no_root_squash)"
if ! grep -qF "$EXPORT_LINE" /etc/exports; then
    echo "$EXPORT_LINE" | sudo tee -a /etc/exports
fi

echo "Reloading NFS exports..."
sudo exportfs -ra

# 4. Start and enable NFS server
echo "4. Restarting and enabling NFS server..."
sudo systemctl restart nfs-kernel-server
sudo systemctl enable nfs-kernel-server

# 5. Check exported NFS shares
echo "5. Verifying NFS exports..."
if sudo exportfs -v | grep -q "$NFS_PATH"; then
    echo "✅ NFS share exported successfully."
else
    echo "❌ NFS share not found in exportfs output."
    exit 1
fi

# 6. Install NFS CSI driver
# Reference: https://github.com/kubernetes-csi/csi-driver-nfs/blob/master/docs/install-csi-driver-v4.11.0.md
echo "6. Installing NFS CSI driver..."
curl -skSL https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/v4.11.0/deploy/install-driver.sh | bash -s v4.11.0 --

# 7. Wait for NFS CSI driver to be ready
echo "7. Waiting for NFS CSI driver to be ready..."
kubectl wait --for=condition=ready pod -l app=csi-nfs-node -n kube-system --timeout=10m
kubectl wait --for=condition=ready pod -l app=csi-nfs-controller -n kube-system --timeout=10m

# 8. Deploy StorageClass and PVC
echo "8. Deploying StorageClass and PVC..."
sed -i "s|<NFS_SERVER_IP>|$NFS_SERVER_IP|g" ../config/storage/checkpoint-storage-class.yaml
sed -i "s|<NFS_PATH>|$NFS_PATH|g" ../config/storage/checkpoint-storage-class.yaml
kubectl apply -f ../config/storage/checkpoint-storage-class.yaml

sed -i "s|<PVC_NAME>|$PVC_NAME|g" ../config/storage/checkpoint-pvc.yaml
sed -i "s|<PVC_NAMESPACE>|$PVC_NAMESPACE|g" ../config/storage/checkpoint-pvc.yaml
sed -i "s|<PVC_STORAGE_SIZE>|$PVC_STORAGE_SIZE|g" ../config/storage/checkpoint-pvc.yaml
kubectl apply -f ../config/storage/checkpoint-pvc.yaml

# 9. Wait for PVC to be bound
echo "9. Waiting for PVC to be bound..."
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/checkpoint-repo -n live-pod-migration-controller-system --timeout=10m

echo ""
echo "✅ Shared storage deployment complete!"
echo ""
echo "Verification:"
echo "kubectl get pvc -n live-pod-migration-controller-system"